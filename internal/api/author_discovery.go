package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/vavallee/bindery/internal/models"
	"github.com/vavallee/bindery/internal/notifier"
)

// ErrAuthorSyncRunning is returned by DiscoverAuthorBooks when a catalogue
// sync for the same author is already in flight, typically a manual Refresh
// the user clicked while the scheduled pass reached that author. Discovery
// skips the author instead of running a second sync beside the first: two
// concurrent runs race each other's creates and would announce the same book
// twice.
var ErrAuthorSyncRunning = errors.New("a catalogue sync for this author is already running")

// bookAnnouncedListLimit caps how many books one bookAnnounced payload lists.
// The rest are counted in "more", so a relink that adds a whole catalogue
// still sends one readable message.
const bookAnnouncedListLimit = 10

// Length caps for provider text in the bookAnnounced payload. OpenLibrary is
// publicly editable, so a title is whatever someone typed there, and it goes
// straight into an admin's chat channel.
const (
	announceMaxTitleRunes     = 200
	announceMaxAuthorRunes    = 200
	announceMaxForeignIDRunes = 100
)

// eventSender is the part of the notifier the author handler uses. An
// interface so tests can record events without a webhook server.
type eventSender interface {
	Send(ctx context.Context, eventType string, payload map[string]interface{})
}

// WithNotifier attaches the webhook notifier so catalogue syncs can publish
// bookAnnounced (#2236). Without it nothing is announced.
func (h *AuthorHandler) WithNotifier(n eventSender) *AuthorHandler {
	h.notif = n
	return h
}

// DiscoverAuthorBooks runs one unattended discovery sync for author and
// reports how many books it created, along with the provider error when the
// author's works could not be fetched (the scheduler inspects it for a rate
// limit and backs off).
//
// It is the refresh path in every respect but these: it never searches or
// grabs (a created book reaches an indexer only through the existing
// search-wanted sweep and its auto grab switch); it refuses to start while
// another sync for the same author is running, returning
// ErrAuthorSyncRunning; it enriches covers only for works the author does not
// already have; and it re-reads the author first, so an author deleted,
// unmonitored or set to add no new items since the job picked it is skipped.
// A skipped author returns (0, nil) and so counts as checked. It runs
// synchronously on the caller's goroutine.
func (h *AuthorHandler) DiscoverAuthorBooks(ctx context.Context, author *models.Author) (int, error) {
	if author == nil || author.ID == 0 {
		return 0, errors.New("discover author books: author has no id")
	}
	if !h.runningSyncs.tryStart(author.ID) {
		return 0, ErrAuthorSyncRunning
	}
	h.discovering.Store(author.ID, struct{}{})
	defer h.discovering.Delete(author.ID)

	current, err := h.authors.GetByID(ctx, author.ID)
	if err != nil {
		h.runningSyncs.done(author.ID)
		return 0, fmt.Errorf("discover author books: re-read author %d: %w", author.ID, err)
	}
	if current == nil || !authorAcceptsDiscoveredBooks(current) {
		h.runningSyncs.done(author.ID)
		slog.Info("discovery skipped an author that no longer takes new books",
			"author", author.Name, "authorId", author.ID, "deleted", current == nil)
		return 0, nil
	}
	return h.runCatalogueSync(ctx, current, catalogueSyncOptions{
		mediaType:            h.resolveDefaultMediaType(ctx),
		discovery:            true,
		syncClaimed:          true,
		deferCoverEnrichment: true,
	})
}

// refreshConflictMessage is the 409 text for a manual Refresh that found a
// sync already running for the author. When that sync is scheduled discovery
// the user did not start it, so the message says what it is.
func (h *AuthorHandler) refreshConflictMessage(authorID int64) string {
	if _, ok := h.discovering.Load(authorID); ok {
		return "Bindery is already checking this author for new books. Try again in a minute."
	}
	return "a refresh for this author is already running"
}

// catalogueWasPopulated reports, before a sync creates anything, whether the
// author already had a catalogue: at least one book row, excluded ones
// included. It is the baseline the announce rule needs, because the first
// population of an author lists their whole bibliography, which is not news.
// An author the user emptied counts as not populated for the same reason: its
// refill is a first population again. Only discovery runs need the answer.
func catalogueWasPopulated(opts catalogueSyncOptions, bookCount int) bool {
	return opts.discovery && opts.onlyForeignID == "" && bookCount > 0
}

// shouldAnnounceDiscovered is the whole bookAnnounced rule, in one place.
//
// A run announces only when it is a discovery run (a manual Refresh, bulk
// refresh, Refresh all, relink or the scheduled discovery job), the author's
// catalogue was populated before the run started, and the run created at
// least one book. The add flow and AddBook's single work fallback are not
// discovery runs, and the first population of an author is not a populated
// catalogue, so none of those ever announce.
func shouldAnnounceDiscovered(opts catalogueSyncOptions, populatedBefore bool, created int) bool {
	return opts.discovery && opts.onlyForeignID == "" && populatedBefore && created > 0
}

// announceDiscoveredBooks sends one bookAnnounced event for a finished sync
// when shouldAnnounceDiscovered says so. It is called once per author run,
// from the end of runCatalogueSync.
func (h *AuthorHandler) announceDiscoveredBooks(ctx context.Context, author *models.Author, opts catalogueSyncOptions, populatedBefore bool, created []models.Book) {
	if h.notif == nil || author == nil || !shouldAnnounceDiscovered(opts, populatedBefore, len(created)) {
		return
	}
	h.notif.Send(ctx, notifier.EventBookAnnounced, bookAnnouncedPayload(author, created))
}

// bookAnnouncedPayload builds the event payload: the author, the total count,
// up to bookAnnouncedListLimit books with each one's monitored flag, and a
// "more" count for the rest. Every provider supplied string goes through
// notifier.SafeText, which caps it, strips control and invisible characters,
// and neutralises mentions, Slack escapes and markdown links (S3, #2676).
func bookAnnouncedPayload(author *models.Author, created []models.Book) map[string]interface{} {
	listed := created
	if len(listed) > bookAnnouncedListLimit {
		listed = listed[:bookAnnouncedListLimit]
	}
	books := make([]map[string]interface{}, 0, len(listed))
	titles := make([]string, 0, len(listed))
	for i := range listed {
		b := &listed[i]
		title := notifier.SafeText(b.Title, announceMaxTitleRunes)
		entry := map[string]interface{}{
			"id":        b.ID,
			"title":     title,
			"foreignId": notifier.SafeText(b.ForeignID, announceMaxForeignIDRunes),
			"monitored": b.Monitored,
		}
		if b.ReleaseDate != nil {
			entry["releaseDate"] = b.ReleaseDate.UTC().Format("2006-01-02")
		}
		books = append(books, entry)
		titles = append(titles, title)
	}
	more := len(created) - len(listed)
	message := strings.Join(titles, ", ")
	if more > 0 {
		message += " and " + strconv.Itoa(more) + " more"
	}
	return map[string]interface{}{
		"author":   notifier.SafeText(author.Name, announceMaxAuthorRunes),
		"authorId": author.ID,
		"count":    len(created),
		"books":    books,
		"more":     more,
		"message":  message,
	}
}

// newWorkIndexes returns the positions in candidates of works the author does
// not already have. The create loop exempts a work that resolves to an
// existing book from the MinPages and SkipMissingISBN filters, so fetching
// its editions was wasted (P1): on a 65 book author that was 65 provider calls
// on every refresh. A discovery run also skips cover enrichment for those
// works (#2236). That is a trade: the sync backfills an empty cover on an
// existing book (#1748), so a discovery run leaves existing coverless books
// without one to save the provider calls, and a manual refresh still fills
// them.
//
// A work is known when its foreign id or its Hardcover id is the foreign id of
// one of the author's loaded books (excluded ones included) or one of the
// identifiers recorded against them. Each of those ids makes
// resolveExistingBook return a row, so treating the work as known changes no
// outcome. An identifier read failure falls back to the loaded foreign ids
// alone, which only means a few calls that were not needed.
func (h *AuthorHandler) newWorkIndexes(ctx context.Context, authorID int64, known, candidates []models.Book) []int {
	ids := make(map[string]struct{}, len(known))
	for i := range known {
		if id := strings.TrimSpace(known[i].ForeignID); id != "" {
			ids[id] = struct{}{}
		}
	}
	if h.books != nil && authorID != 0 {
		if byBook, err := h.books.ListBookIdentifiersByAuthor(ctx, authorID); err == nil {
			for _, identifiers := range byBook {
				for _, identifier := range identifiers {
					if id := strings.TrimSpace(identifier.ForeignID); id != "" {
						ids[id] = struct{}{}
					}
				}
			}
		}
	}
	isKnown := func(id string) bool {
		id = strings.TrimSpace(id)
		if id == "" {
			return false
		}
		_, ok := ids[id]
		return ok
	}
	out := make([]int, 0, len(candidates))
	for i := range candidates {
		if isKnown(candidates[i].ForeignID) || isKnown(candidates[i].HardcoverForeignID) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// booksAt copies the books at the given positions.
func booksAt(books []models.Book, positions []int) []models.Book {
	out := make([]models.Book, 0, len(positions))
	for _, i := range positions {
		out = append(out, books[i])
	}
	return out
}

// enrichNewWorkCovers runs the aggregator's cover enrichment over the new
// works that have no cover, writing the enriched works back in place.
func (h *AuthorHandler) enrichNewWorkCovers(ctx context.Context, candidates []models.Book, newWorks []int) {
	if h.meta == nil {
		return
	}
	targets := make([]int, 0, len(newWorks))
	for _, i := range newWorks {
		if candidates[i].ImageURL == "" {
			targets = append(targets, i)
		}
	}
	if len(targets) == 0 {
		return
	}
	books := booksAt(candidates, targets)
	h.meta.EnrichMissingCovers(ctx, books)
	for j, i := range targets {
		candidates[i] = books[j]
	}
}

// lockCatalogueWrites takes the per author catalogue lock for a sync and
// returns an idempotent release, so the caller can release early and still
// defer it.
//
// AddBook's single work fallback (onlyForeignID) takes no lock. Its caller
// polls for the book for 15 seconds and a discovery run can hold the lock for
// minutes. What it writes is confined to the one work: at most one book row
// (foreign_id is UNIQUE, so a concurrent sync that creates the same work
// loses the insert and counts it as matched), that book's identifiers and
// series links, and, when the work already exists, that row's ratings, cover
// and author, plus the author's write once catalogue_populated_at stamp. It
// skips the profile refresh, the Calibre relink and the sync summary, and
// never announces. What the lock would still prevent is a sibling sync
// creating the same title under a different foreign id in the same instant,
// leaving two rows for one book; that needs the direct insert to have missed
// and a sync of the same author to be writing that title at that moment.
func (h *AuthorHandler) lockCatalogueWrites(ctx context.Context, author *models.Author, opts catalogueSyncOptions) (func(), error) {
	if opts.onlyForeignID != "" {
		return func() {}, nil
	}
	unlock, err := h.catalogueWrites.lock(ctx, author.ID)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(unlock) }, nil
}

// authorCatalogueLocks hands out one lock per author for the write half of a
// catalogue sync. Entries are reference counted and removed when unused, so
// the map holds only authors with a sync holding or waiting for the lock.
type authorCatalogueLocks struct {
	mu    sync.Mutex
	locks map[int64]*authorCatalogueLock
}

// authorCatalogueLock is a one slot channel rather than a mutex so a waiter
// can give up when its context ends, and a shutdown never queues behind a
// holder.
type authorCatalogueLock struct {
	slot chan struct{}
	refs int
}

// lock blocks until authorID's lock is held or ctx is done, and returns the
// release. An author id of 0 (a sync on an unsaved author) takes no lock.
func (l *authorCatalogueLocks) lock(ctx context.Context, authorID int64) (func(), error) {
	if authorID == 0 {
		return func() {}, nil
	}
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[int64]*authorCatalogueLock)
	}
	entry := l.locks[authorID]
	if entry == nil {
		entry = &authorCatalogueLock{slot: make(chan struct{}, 1)}
		l.locks[authorID] = entry
	}
	entry.refs++
	l.mu.Unlock()

	unref := func() {
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.locks, authorID)
		}
		l.mu.Unlock()
	}
	select {
	case entry.slot <- struct{}{}:
		return func() {
			<-entry.slot
			unref()
		}, nil
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
}
