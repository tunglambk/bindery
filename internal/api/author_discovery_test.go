package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/metadata"
	"github.com/vavallee/bindery/internal/models"
	"github.com/vavallee/bindery/internal/notifier"
)

// eventRecorder is an eventSender that keeps every event it is given.
type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	eventType string
	payload   map[string]interface{}
}

func (r *eventRecorder) Send(_ context.Context, eventType string, payload map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{eventType: eventType, payload: payload})
}

func (r *eventRecorder) snapshot() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedEvent(nil), r.events...)
}

type discoveryFixture struct {
	db      *sql.DB
	authors *db.AuthorRepo
	books   *db.BookRepo
	profile *db.MetadataProfileRepo
	author  *models.Author
}

// newDiscoveryFixture opens a database with one monitored author. When
// populated is true the author already has one book, "Existing Work", so its
// catalogue counts as populated.
func newDiscoveryFixture(t *testing.T, populated bool) *discoveryFixture {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	f := &discoveryFixture{
		db:      database,
		authors: db.NewAuthorRepo(database),
		books:   db.NewBookRepo(database),
		profile: db.NewMetadataProfileRepo(database),
	}
	ctx := context.Background()
	f.author = &models.Author{
		ForeignID: "OL2236A", Name: "Ann Leckie", SortName: "Leckie, Ann",
		MetadataProvider: "openlibrary", Monitored: true,
	}
	if err := f.authors.Create(ctx, f.author); err != nil {
		t.Fatal(err)
	}
	if populated {
		if err := f.books.Create(ctx, &models.Book{
			ForeignID: "OL2236W0", AuthorID: f.author.ID, Title: "Existing Work", SortTitle: "existing work",
			Language: "eng", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted,
			Genres: []string{}, MetadataProvider: "openlibrary", Monitored: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func discoveryWork(n int) models.Book {
	return models.Book{
		ForeignID: fmt.Sprintf("OL2236W%d", n), Title: fmt.Sprintf("New Work %d", n), SortTitle: fmt.Sprintf("new work %d", n),
		Language: "eng", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted,
		Genres: []string{}, MetadataProvider: "openlibrary",
	}
}

func existingWork() models.Book {
	return models.Book{
		ForeignID: "OL2236W0", Title: "Existing Work", SortTitle: "existing work",
		Language: "eng", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted,
		Genres: []string{}, MetadataProvider: "openlibrary",
	}
}

func (f *discoveryFixture) handler(stub *stubMetaProvider, rec *eventRecorder) *AuthorHandler {
	return NewAuthorHandler(f.authors, nil, f.books, nil, metadata.NewAggregator(stub), nil, f.profile, nil).WithNotifier(rec)
}

// A discovery run on an author whose catalogue was already populated sends
// exactly one bookAnnounced, listing the new book with its monitored flag.
func TestDiscoverAuthorBooks_AnnouncesNewBookForPopulatedAuthor(t *testing.T) {
	f := newDiscoveryFixture(t, true)
	stub := &stubMetaProvider{works: []models.Book{existingWork(), discoveryWork(1)}}
	rec := &eventRecorder{}
	h := f.handler(stub, rec)

	created, err := h.DiscoverAuthorBooks(context.Background(), f.author)
	if err != nil {
		t.Fatalf("DiscoverAuthorBooks error: %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly one bookAnnounced", len(events))
	}
	ev := events[0]
	if ev.eventType != notifier.EventBookAnnounced {
		t.Fatalf("event type = %q, want %q", ev.eventType, notifier.EventBookAnnounced)
	}
	if ev.payload["author"] != "Ann Leckie" || ev.payload["count"] != 1 || ev.payload["more"] != 0 {
		t.Errorf("payload header = author %v count %v more %v", ev.payload["author"], ev.payload["count"], ev.payload["more"])
	}
	books, _ := ev.payload["books"].([]map[string]interface{})
	if len(books) != 1 {
		t.Fatalf("payload lists %d books, want 1", len(books))
	}
	if books[0]["title"] != "New Work 1" {
		t.Errorf("listed title = %v", books[0]["title"])
	}
	monitored, ok := books[0]["monitored"].(bool)
	if !ok || !monitored {
		t.Errorf("listed book monitored = %v (present %v), want true for a monitored all author", books[0]["monitored"], ok)
	}
	if id, _ := books[0]["id"].(int64); id == 0 {
		t.Errorf("listed book has no id: %v", books[0]["id"])
	}
}

// First population is not news: a discovery run that fills an author who never
// had a catalogue creates books and announces nothing.
func TestDiscoverAuthorBooks_NoAnnouncementOnFirstPopulation(t *testing.T) {
	f := newDiscoveryFixture(t, false)
	stub := &stubMetaProvider{works: []models.Book{discoveryWork(1), discoveryWork(2)}}
	rec := &eventRecorder{}
	h := f.handler(stub, rec)

	created, err := h.DiscoverAuthorBooks(context.Background(), f.author)
	if err != nil {
		t.Fatal(err)
	}
	if created != 2 {
		t.Fatalf("created = %d, want 2 (a never populated author is repaired)", created)
	}
	if events := rec.snapshot(); len(events) != 0 {
		t.Fatalf("first population sent %d events, want none", len(events))
	}
}

// The add flow's initial sync never announces, even on an author that already
// has books.
func TestFetchAuthorBooks_AddFlowNeverAnnounces(t *testing.T) {
	f := newDiscoveryFixture(t, true)
	stub := &stubMetaProvider{works: []models.Book{existingWork(), discoveryWork(1)}}
	rec := &eventRecorder{}
	h := f.handler(stub, rec)

	h.FetchAuthorBooks(f.author, false, models.MediaTypeEbook)

	got, err := f.books.ListByAuthor(context.Background(), f.author.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("add flow sync left %d books, want 2", len(got))
	}
	if events := rec.snapshot(); len(events) != 0 {
		t.Fatalf("add flow sync sent %d events, want none", len(events))
	}
}

// A run that adds twelve books lists ten and counts the other two in more.
func TestDiscoverAuthorBooks_AnnouncementListsTenAndCountsTheRest(t *testing.T) {
	f := newDiscoveryFixture(t, true)
	works := []models.Book{existingWork()}
	for i := 1; i <= 12; i++ {
		works = append(works, discoveryWork(i))
	}
	rec := &eventRecorder{}
	h := f.handler(&stubMetaProvider{works: works}, rec)

	if _, err := h.DiscoverAuthorBooks(context.Background(), f.author); err != nil {
		t.Fatal(err)
	}
	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want one for the whole run", len(events))
	}
	p := events[0].payload
	books, _ := p["books"].([]map[string]interface{})
	if len(books) != 10 || p["more"] != 2 || p["count"] != 12 {
		t.Fatalf("listed %d, more %v, count %v; want 10, 2, 12", len(books), p["more"], p["count"])
	}
	if msg, _ := p["message"].(string); !strings.HasSuffix(msg, " and 2 more") {
		t.Errorf("message = %q, want it to end with the more count", msg)
	}
}

// A discovery run that finds nothing new announces nothing.
func TestDiscoverAuthorBooks_NothingNewNoAnnouncement(t *testing.T) {
	f := newDiscoveryFixture(t, true)
	rec := &eventRecorder{}
	h := f.handler(&stubMetaProvider{works: []models.Book{existingWork()}}, rec)
	created, err := h.DiscoverAuthorBooks(context.Background(), f.author)
	if err != nil || created != 0 {
		t.Fatalf("created %d, err %v; want 0, nil", created, err)
	}
	if events := rec.snapshot(); len(events) != 0 {
		t.Fatalf("sent %d events, want none", len(events))
	}
}

func TestShouldAnnounceDiscovered(t *testing.T) {
	cases := []struct {
		name      string
		opts      catalogueSyncOptions
		populated bool
		created   int
		want      bool
	}{
		{"discovery on a populated author", catalogueSyncOptions{discovery: true}, true, 1, true},
		{"nothing created", catalogueSyncOptions{discovery: true}, true, 0, false},
		{"first population", catalogueSyncOptions{discovery: true}, false, 5, false},
		{"add flow initial sync", catalogueSyncOptions{autoSearch: true}, true, 5, false},
		{"add book single work fallback", catalogueSyncOptions{onlyForeignID: "OL1W"}, true, 1, false},
		{"single work even if marked discovery", catalogueSyncOptions{discovery: true, onlyForeignID: "OL1W"}, true, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAnnounceDiscovered(tc.opts, tc.populated, tc.created); got != tc.want {
				t.Errorf("shouldAnnounceDiscovered = %v, want %v", got, tc.want)
			}
		})
	}
}

// S3: provider text is capped and loses control and bidi override characters
// before it reaches a webhook. notifier.SafeText drops an invisible override
// outright instead of replacing it with a space, and neutralises markdown link
// syntax, so the surrounding text joins and "[" becomes a fullwidth lookalike.
func TestBookAnnouncedPayload_SanitisesProviderText(t *testing.T) {
	long := strings.Repeat("x", 500)
	author := &models.Author{ID: 7, Name: "Evil\u202eAuthor\r\nName"}
	created := []models.Book{
		{ID: 1, Title: "Line one\nLine two\x00\x1b[31m", ForeignID: "OL1W\t", Monitored: false},
		{ID: 2, Title: long, ForeignID: long},
	}
	p := bookAnnouncedPayload(author, created)
	if p["author"] != "EvilAuthor Name" {
		t.Errorf("author = %q", p["author"])
	}
	books := p["books"].([]map[string]interface{})
	if books[0]["title"] != "Line one Line two\uff3b31m" {
		t.Errorf("title = %q", books[0]["title"])
	}
	if books[0]["foreignId"] != "OL1W" {
		t.Errorf("foreignId = %q", books[0]["foreignId"])
	}
	if books[0]["monitored"] != false {
		t.Errorf("monitored = %v, want false carried through", books[0]["monitored"])
	}
	title := books[1]["title"].(string)
	if n := len([]rune(title)); n != announceMaxTitleRunes {
		t.Errorf("long title kept %d runes, want %d", n, announceMaxTitleRunes)
	}
	if n := len([]rune(books[1]["foreignId"].(string))); n != announceMaxForeignIDRunes {
		t.Errorf("long foreign id kept %d runes, want %d", n, announceMaxForeignIDRunes)
	}
	for _, s := range []string{p["message"].(string), p["author"].(string), title} {
		for _, r := range s {
			if r < 0x20 || r == 0x7f || r == '\u202e' {
				t.Fatalf("control character %U survived in %q", r, s)
			}
		}
	}
}

// assertInert fails when s still carries a character a chat service reads as a
// mention, a Slack escape, or markdown link syntax.
func assertInert(t *testing.T, field, s string) {
	t.Helper()
	for _, bad := range []string{"@", "<", ">", "[", "]", "://"} {
		if strings.Contains(s, bad) {
			t.Errorf("%s = %q still carries %q", field, s, bad)
		}
	}
}

// The bookAnnounced payload lands in an admin's chat channel and OpenLibrary
// is publicly editable, so a provider title must not be able to forge a
// mention, a Slack escape or a masked link (#2676). The scheme text itself
// survives: without bracket or angle syntax it is inert.
func TestBookAnnouncedPayload_NeutralisesChatMarkup(t *testing.T) {
	cases := []struct{ name, title, author, foreign string }{
		{name: "discord mention", title: "@here new book"},
		{name: "slack channel escape", title: "<!channel> read this"},
		{name: "slack link", title: "<https://evil.example|Click me>"},
		{name: "markdown link", title: "[Free nitro](https://evil.example)"},
		{name: "javascript scheme", title: "[click](javascript:alert(1))"},
		{name: "file scheme", title: "[open](file:///etc/passwd)"},
		{name: "bare url", title: "Free at https://evil.example/login"},
		{name: "markup in author", title: "Ordinary Title", author: "@everyone"},
		{name: "markup in foreign id", title: "Ordinary Title", foreign: "OL1W@here"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			authorName := c.author
			if authorName == "" {
				authorName = "Ann Leckie"
			}
			foreignID := c.foreign
			if foreignID == "" {
				foreignID = "OL1W"
			}
			p := bookAnnouncedPayload(&models.Author{ID: 7, Name: authorName}, []models.Book{
				{ID: 1, Title: c.title, ForeignID: foreignID},
			})
			books := p["books"].([]map[string]interface{})
			assertInert(t, "title", books[0]["title"].(string))
			assertInert(t, "foreignId", books[0]["foreignId"].(string))
			assertInert(t, "author", p["author"].(string))
			assertInert(t, "message", p["message"].(string))
		})
	}
	// A title with nothing to neutralise reads exactly as it did before.
	p := bookAnnouncedPayload(&models.Author{ID: 7, Name: "Ann Leckie"}, []models.Book{
		{ID: 1, Title: "Dune: Part One", ForeignID: "OL1W"},
	})
	if got := p["books"].([]map[string]interface{})[0]["title"]; got != "Dune: Part One" {
		t.Errorf("ordinary title changed: %q", got)
	}
	if got := p["message"]; got != "Dune: Part One" {
		t.Errorf("ordinary message changed: %q", got)
	}
}

// P1 and T5: with a profile that needs edition data, the prefetch fetches
// editions only for works the author does not already have. Known works,
// matched by foreign id or by a recorded Hardcover identifier, cost no call.
func TestFetchAuthorBooks_EditionPrefetchSkipsKnownWorks(t *testing.T) {
	f := newDiscoveryFixture(t, true)
	ctx := context.Background()
	profile, err := f.profile.GetByID(ctx, models.DefaultMetadataProfileID)
	if err != nil || profile == nil {
		t.Fatalf("default profile: %v", err)
	}
	profile.MinPages = 100
	if err := f.profile.Update(ctx, profile); err != nil {
		t.Fatal(err)
	}
	// A second owned book, known to this fetch only through a Hardcover id
	// recorded against it.
	hcOwned := &models.Book{
		ForeignID: "OL2236W50", AuthorID: f.author.ID, Title: "Hardcover Known", SortTitle: "hardcover known",
		Language: "eng", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted,
		Genres: []string{}, MetadataProvider: "openlibrary", Monitored: true,
	}
	if err := f.books.Create(ctx, hcOwned); err != nil {
		t.Fatal(err)
	}
	if err := f.books.UpsertBookIdentifier(ctx, hcOwned.ID, "hc:hardcover-known"); err != nil {
		t.Fatal(err)
	}

	relinked := models.Book{
		ForeignID: "OL2236W99", HardcoverForeignID: "hc:hardcover-known", Title: "Hardcover Known", SortTitle: "hardcover known",
		Language: "eng", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted, Genres: []string{}, MetadataProvider: "openlibrary",
	}
	works := []models.Book{existingWork(), relinked, discoveryWork(1), discoveryWork(2)}
	stub := &stubMetaProvider{works: works, editionsByBook: map[string][]models.Edition{
		"OL2236W1": {{ForeignID: "E1", NumPages: intPtr(300)}},
		"OL2236W2": {{ForeignID: "E2", NumPages: intPtr(300)}},
	}}
	h := f.handler(stub, &eventRecorder{})
	if _, err := h.DiscoverAuthorBooks(ctx, f.author); err != nil {
		t.Fatal(err)
	}

	stub.editionCallsMu.Lock()
	calls := append([]string(nil), stub.editionCalls...)
	stub.editionCallsMu.Unlock()
	perWork := map[string]int{}
	for _, c := range calls {
		perWork[c]++
	}
	for _, known := range []string{"OL2236W0", "OL2236W99"} {
		if perWork[known] != 0 {
			t.Errorf("GetEditions called %d times for known work %s, want 0", perWork[known], known)
		}
	}
	// Each new work is fetched once by the filter prefetch; the created book
	// hydration reuses that result through the seeded cache.
	for _, fresh := range []string{"OL2236W1", "OL2236W2"} {
		if perWork[fresh] != 1 {
			t.Errorf("GetEditions called %d times for new work %s, want 1 (all calls: %v)", perWork[fresh], fresh, calls)
		}
	}
}

// ratelimitedWorksProvider fails the works call the way a refusing provider
// does, so DiscoverAuthorBooks can be seen to hand the error back.
type ratelimitedWorksProvider struct {
	*stubMetaProvider
	err error
}

func (p ratelimitedWorksProvider) GetAuthorWorks(context.Context, string) ([]models.Book, error) {
	return nil, p.err
}

func TestDiscoverAuthorBooks_ReturnsProviderError(t *testing.T) {
	f := newDiscoveryFixture(t, true)
	sentinel := errors.New("provider refused")
	agg := metadata.NewAggregator(ratelimitedWorksProvider{stubMetaProvider: &stubMetaProvider{}, err: fmt.Errorf("works: %w", sentinel)})
	h := NewAuthorHandler(f.authors, nil, f.books, nil, agg, nil, f.profile, nil)
	created, err := h.DiscoverAuthorBooks(context.Background(), f.author)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the provider error wrapped through", err)
	}
	if created != 0 {
		t.Fatalf("created = %d, want 0", created)
	}
	if h.runningSyncs.running(f.author.ID) {
		t.Fatal("a failed discovery run left the author marked as syncing")
	}
}

// T3: a scheduled discovery run and a manual Refresh of the same author at
// once. Whichever starts first runs, the other is refused, and no book is
// created twice. Run with -race.
func TestDiscoverAuthorBooks_ConcurrentWithManualRefresh(t *testing.T) {
	type start func(t *testing.T, h *AuthorHandler, f *discoveryFixture) (refusedOther func() bool)

	refreshFirst := func(t *testing.T, h *AuthorHandler, f *discoveryFixture) func() bool {
		id := strconv.FormatInt(f.author.ID, 10)
		req := withURLParam(httptest.NewRequest(http.MethodPost, "/api/v1/author/"+id+"/refresh", nil), "id", id)
		rec := httptest.NewRecorder()
		h.Refresh(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("manual Refresh = %d, want 202", rec.Code)
		}
		return func() bool {
			_, err := h.DiscoverAuthorBooks(context.Background(), f.author)
			return errors.Is(err, ErrAuthorSyncRunning)
		}
	}
	var discoveryDone sync.WaitGroup
	discoveryFirst := func(t *testing.T, h *AuthorHandler, f *discoveryFixture) func() bool {
		discoveryDone.Add(1)
		go func() {
			defer discoveryDone.Done()
			if _, err := h.DiscoverAuthorBooks(context.Background(), f.author); err != nil {
				t.Errorf("discovery run: %v", err)
			}
		}()
		return func() bool {
			id := strconv.FormatInt(f.author.ID, 10)
			req := withURLParam(httptest.NewRequest(http.MethodPost, "/api/v1/author/"+id+"/refresh", nil), "id", id)
			rec := httptest.NewRecorder()
			h.Refresh(rec, req)
			// A user's click that meets a scheduled check is told what is
			// running, not that "a refresh" is (item 7 of the review).
			return rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "already checking this author for new books")
		}
	}

	for name, first := range map[string]start{"manual refresh first": refreshFirst, "discovery first": discoveryFirst} {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryFixture(t, true)
			gate := make(chan struct{})
			entered := make(chan bool, 4)
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			defer release()
			stub := &stubMetaProvider{
				works:           []models.Book{existingWork(), discoveryWork(1)},
				author:          &models.Author{ForeignID: "OL2236A", Name: "Ann Leckie", MetadataProvider: "openlibrary"},
				getAuthorBypass: entered,
				getAuthorGate:   gate,
			}
			rec := &eventRecorder{}
			h := f.handler(stub, rec)

			tryOther := first(t, h, f)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the first sync never reached the provider")
			}
			if !tryOther() {
				t.Fatal("the second sync was not refused while the first was running")
			}
			select {
			case <-entered:
				t.Fatal("the refused sync still reached the provider")
			default:
			}

			release()
			discoveryDone.Wait()
			deadline := time.Now().Add(5 * time.Second)
			for h.runningSyncs.running(f.author.ID) {
				if time.Now().After(deadline) {
					t.Fatal("the first sync never finished")
				}
				time.Sleep(10 * time.Millisecond)
			}

			books, err := f.books.ListByAuthor(context.Background(), f.author.ID)
			if err != nil {
				t.Fatal(err)
			}
			count := map[string]int{}
			for _, b := range books {
				count[b.ForeignID]++
			}
			if len(books) != 2 || count["OL2236W1"] != 1 {
				t.Fatalf("books after the race = %d (%v), want the existing one plus New Work 1 once", len(books), count)
			}
			if events := rec.snapshot(); len(events) != 1 {
				t.Fatalf("sent %d bookAnnounced events, want 1", len(events))
			}
		})
	}
}
