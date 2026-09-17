package abs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/metadata"
	"github.com/vavallee/bindery/internal/models"
)

// bindingStubProvider is a minimal provider whose author search and ISBN
// lookup can be made to fail, which stubABSMetadataProvider cannot do.
type bindingStubProvider struct {
	name        string
	authors     []models.Author
	err         error
	full        map[string]*models.Author
	isbnErr     error
	booksByISBN map[string]*models.Book
}

func (p *bindingStubProvider) Name() string { return p.name }
func (p *bindingStubProvider) SearchAuthors(context.Context, string) ([]models.Author, error) {
	if p.err != nil {
		return nil, p.err
	}
	return append([]models.Author(nil), p.authors...), nil
}
func (p *bindingStubProvider) SearchBooks(context.Context, string) ([]models.Book, error) {
	return nil, nil
}
func (p *bindingStubProvider) GetAuthor(_ context.Context, foreignID string) (*models.Author, error) {
	return p.full[foreignID], nil
}
func (p *bindingStubProvider) GetBook(context.Context, string) (*models.Book, error) {
	return nil, nil
}
func (p *bindingStubProvider) GetEditions(context.Context, string) ([]models.Edition, error) {
	return nil, nil
}
func (p *bindingStubProvider) GetBookByISBN(_ context.Context, isbn string) (*models.Book, error) {
	if p.isbnErr != nil {
		return nil, p.isbnErr
	}
	return p.booksByISBN[isbn], nil
}

const rateLimit429 = "HTTP 429: API rate limit exceeded for tier 'Free'. Try again in 1 seconds."

// TestABSImportRefusesToBindOnPrimaryFailure is the exact sequence reported in
// #2271: primary_provider = hardcover, a valid token, Hardcover 429s during
// one import, OpenLibrary answers, and the author is bound to OpenLibrary
// forever because providerForForeignID reads the provider back off that id.
func TestABSImportRefusesToBindOnPrimaryFailure(t *testing.T) {
	hc := &bindingStubProvider{name: "hardcover", err: errors.New(rateLimit429)}
	ol := &bindingStubProvider{
		name:    "openlibrary",
		authors: []models.Author{{Name: "Adrian Tchaikovsky", ForeignID: "OL7468980A"}},
		full:    map[string]*models.Author{"OL7468980A": {Name: "Adrian Tchaikovsky", ForeignID: "OL7468980A"}},
	}
	imp := (&Importer{}).WithMetadata(metadata.NewAggregator(hc, ol))

	author, ambiguous, err := imp.lookupUpstreamAuthor(context.Background(), "Adrian Tchaikovsky")
	if author != nil {
		t.Errorf("no author may be returned for binding while the primary is throttled, got %+v", author)
	}
	if ambiguous {
		t.Error("this is not an ambiguous match, it is an unavailable provider")
	}
	var unavailable *PrimaryProviderUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("want PrimaryProviderUnavailableError so enrichAuthor can report it, got %v", err)
	}
	if unavailable.Failed != "hardcover" {
		t.Errorf("the failing provider should be named for the operator, got %q", unavailable.Failed)
	}
	if !errors.Is(err, unavailable.Err) {
		t.Error("the upstream error should stay in the chain for the log")
	}
}

// TestABSImportStillBindsWhenPrimaryMerelyMisses guards the other half. #2237
// is the case where Hardcover answers and simply does not have the record;
// there the OpenLibrary link is the right answer and refusing it would stop
// imports working on any author Hardcover has never heard of.
func TestABSImportStillBindsWhenPrimaryMerelyMisses(t *testing.T) {
	hc := &bindingStubProvider{name: "hardcover"} // answers, empty
	ol := &bindingStubProvider{
		name:    "openlibrary",
		authors: []models.Author{{Name: "Adrian Tchaikovsky", ForeignID: "OL7468980A"}},
		full:    map[string]*models.Author{"OL7468980A": {Name: "Adrian Tchaikovsky", ForeignID: "OL7468980A"}},
	}
	imp := (&Importer{}).WithMetadata(metadata.NewAggregator(hc, ol))

	author, _, err := imp.lookupUpstreamAuthor(context.Background(), "Adrian Tchaikovsky")
	if err != nil {
		t.Fatalf("a primary that answers with nothing is not a failure: %v", err)
	}
	if author == nil || author.ForeignID != "OL7468980A" {
		t.Fatalf("expected the fallback match to be usable, got %+v", author)
	}
}

// TestABSImportBindsPrimaryRecordEvenWhenAnotherProviderFails: only the
// PRIMARY failing matters. An enricher dropping out cannot cause a downgrade,
// because the record being bound is the primary's own.
func TestABSImportBindsPrimaryRecordEvenWhenAnotherProviderFails(t *testing.T) {
	hc := &bindingStubProvider{
		name:    "hardcover",
		authors: []models.Author{{Name: "Adrian Tchaikovsky", ForeignID: "hc:adrian-tchaikovsky"}},
		full:    map[string]*models.Author{"hc:adrian-tchaikovsky": {Name: "Adrian Tchaikovsky", ForeignID: "hc:adrian-tchaikovsky"}},
	}
	ol := &bindingStubProvider{name: "openlibrary", err: errors.New("HTTP 503")}
	imp := (&Importer{}).WithMetadata(metadata.NewAggregator(hc, ol))

	author, _, err := imp.lookupUpstreamAuthor(context.Background(), "Adrian Tchaikovsky")
	if err != nil {
		t.Fatalf("an enricher failing must not block a primary match: %v", err)
	}
	if author == nil || author.ForeignID != "hc:adrian-tchaikovsky" {
		t.Fatalf("expected the primary's record, got %+v", author)
	}
}

// bookBindingISBN is the ISBN the book half of the #2271 guard works with.
const bookBindingISBN = "9780593135204"

// bookBindingRelinkFixture wires an OpenLibrary primary and a DNB enricher,
// creates the local book an ABS import would have on hand, and returns it with
// the item it came from. The row starts on its ABS identity, which is the
// identity the importer must not overwrite while the primary is down.
func bookBindingRelinkFixture(t *testing.T, primary, dnb *bindingStubProvider) (*Importer, NormalizedLibraryItem, *models.Book) {
	t.Helper()
	importer, _, bookRepo, _, _, _, _, _, _, _ := newABSImporterFixture(t)
	author := asinTestAuthor(t, importer)
	importer.meta = metadata.NewAggregator(primary, dnb)

	item := sampleABSItem()
	item.ISBN = bookBindingISBN
	book := &models.Book{
		ForeignID:        "abs:book:" + item.LibraryID + ":" + item.ItemID,
		AuthorID:         author.ID,
		Title:            item.Title,
		SortTitle:        item.Title,
		Status:           models.BookStatusWanted,
		MetadataProvider: providerAudiobookshelf,
	}
	if err := bookRepo.Create(context.Background(), book); err != nil {
		t.Fatalf("create book: %v", err)
	}
	return importer, item, book
}

// TestABSImportRefusesBookRelinkOnPrimaryFailure is #2642, the book half of
// #2271: primary_provider = openlibrary times out, DNB answers the ISBN, and
// mergeUpstreamBook rewrote book.ForeignID and book.MetadataProvider to DNB's.
// Once the primary is back, a lookup by its key misses the relinked row, which
// is where the duplicates come from (#2117, #2271, #2332).
func TestABSImportRefusesBookRelinkOnPrimaryFailure(t *testing.T) {
	t.Parallel()

	primary := &bindingStubProvider{name: "openlibrary", isbnErr: errors.New("openlibrary: context deadline exceeded")}
	dnb := &bindingStubProvider{name: "dnb", booksByISBN: map[string]*models.Book{
		bookBindingISBN: {
			ForeignID:        "dnb:1305873874",
			Title:            "Project Hail Mary",
			Description:      "A lone astronaut wakes with no memory of who he is and no idea why he is the only survivor.",
			MetadataProvider: "dnb",
		},
	}}
	importer, item, book := bookBindingRelinkFixture(t, primary, dnb)
	absForeignID := book.ForeignID

	result, err := importer.enrichBook(context.Background(), asinTestConfig(), item, nil, book)
	if err != nil {
		t.Fatalf("enrichBook: %v", err)
	}
	if book.ForeignID != absForeignID {
		t.Errorf("book.ForeignID = %q, want the ABS id %q it came in with, not the fallback's", book.ForeignID, absForeignID)
	}
	if book.MetadataProvider != providerAudiobookshelf {
		t.Errorf("book.MetadataProvider = %q, want %q", book.MetadataProvider, providerAudiobookshelf)
	}
	if result.Relinked != 0 {
		t.Errorf("Relinked = %d, want 0 while the primary is down", result.Relinked)
	}
	if msg := strings.Join(result.Messages, "; "); !strings.Contains(msg, "book relink skipped") || !strings.Contains(msg, "did not answer") {
		t.Errorf("messages = %q, want a book relink skipped reason naming the primary outage", msg)
	}
}

// TestABSImportStillRelinksBookWhenPrimaryMerelyMisses guards the other half.
// #2237 is the case where the primary answers and simply has no record for the
// ISBN; the fallback's record is then the right one to relink to, and refusing
// it would stop imports working for anything the primary has never heard of.
func TestABSImportStillRelinksBookWhenPrimaryMerelyMisses(t *testing.T) {
	t.Parallel()

	primary := &bindingStubProvider{name: "openlibrary"} // answers, no record
	dnb := &bindingStubProvider{name: "dnb", booksByISBN: map[string]*models.Book{
		bookBindingISBN: {
			ForeignID:        "dnb:1305873874",
			Title:            "Project Hail Mary",
			Description:      "A lone astronaut wakes with no memory of who he is and no idea why he is the only survivor.",
			MetadataProvider: "dnb",
		},
	}}
	importer, item, book := bookBindingRelinkFixture(t, primary, dnb)

	result, err := importer.enrichBook(context.Background(), asinTestConfig(), item, nil, book)
	if err != nil {
		t.Fatalf("enrichBook: %v", err)
	}
	if book.ForeignID != "dnb:1305873874" {
		t.Errorf("book.ForeignID = %q, want the fallback's dnb:1305873874", book.ForeignID)
	}
	if result.Relinked == 0 {
		t.Errorf("Relinked = 0, want the fallback relink when the primary answered without the ISBN")
	}
}
