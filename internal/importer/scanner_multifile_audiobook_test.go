package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// seedAudiobookInsideBookFolder writes n tracks into <libDir>/<author>/<title>/
// and returns the folder. The tracks carry unreadable tags, so the scan falls
// back to the folder hierarchy for the title, which is what the reporter's
// library looks like (#2716).
func seedAudiobookInsideBookFolder(t *testing.T, libDir, author, title string, tracks int) string {
	t.Helper()
	bookDir := filepath.Join(libDir, author, title)
	if err := os.MkdirAll(bookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= tracks; i++ {
		writeFileAt(t, filepath.Join(bookDir, fmt.Sprintf("%s - %02d of %02d.mp3", title, i, tracks)))
	}
	return bookDir
}

// TestScanLibrary_MultiFileAudiobookRegistersFolder is the #2716 regression:
// a book folder holding several tracks must be recorded as the audiobook, not
// as the one track that happened to match first. book_files is the book's
// on-disk inventory, and the delete/move paths act on it, so a first-file-only
// row means the other tracks are never moved or deleted.
func TestScanLibrary_MultiFileAudiobookRegistersFolder(t *testing.T) {
	libDir := t.TempDir()
	bookDir := seedAudiobookInsideBookFolder(t, libDir, "Amy Tan", "The Kitchen God's Wife", 3)

	s, books, authors, ctx := scannerFixture(t, libDir)
	author := &models.Author{ForeignID: "OL-at", Name: "Amy Tan", SortName: "Tan, Amy"}
	if err := authors.Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	book := &models.Book{ForeignID: "OL-kgw", AuthorID: author.ID, Title: "The Kitchen God's Wife", Status: models.BookStatusWanted}
	if err := books.Create(ctx, book); err != nil {
		t.Fatal(err)
	}

	s.ScanLibrary(ctx)

	files, err := books.ListFiles(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != bookDir {
		got := make([]string, 0, len(files))
		for _, f := range files {
			got = append(got, f.Path)
		}
		t.Fatalf("multi-file audiobook must be recorded as its folder %q, got %d row(s): %v", bookDir, len(files), got)
	}
	if files[0].Format != models.MediaTypeAudiobook {
		t.Errorf("folder row format = %q, want %q", files[0].Format, models.MediaTypeAudiobook)
	}
}

// TestScanLibrary_LooseAudiobookKeepsItsFilePath pins the boundary: a track
// that has no book folder of its own keeps its own book_files path. Its parent
// is an author folder (or the library root) shared with other books, so
// recording the folder would hand those books' material to this one.
func TestScanLibrary_LooseAudiobookKeepsItsFilePath(t *testing.T) {
	libDir := t.TempDir()
	authorDir := filepath.Join(libDir, "Brandon Sanderson")
	if err := os.MkdirAll(authorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	track := filepath.Join(authorDir, "Mistborn - 01.mp3")
	writeFileAt(t, track)

	s, books, authors, ctx := scannerFixture(t, libDir)
	author := &models.Author{ForeignID: "OL-bs", Name: "Brandon Sanderson", SortName: "Sanderson, Brandon"}
	if err := authors.Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	book := &models.Book{ForeignID: "OL-mb", AuthorID: author.ID, Title: "Mistborn", Status: models.BookStatusWanted}
	if err := books.Create(ctx, book); err != nil {
		t.Fatal(err)
	}

	s.ScanLibrary(ctx)

	files, err := books.ListFiles(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != track {
		got := make([]string, 0, len(files))
		for _, f := range files {
			got = append(got, f.Path)
		}
		t.Fatalf("loose audiobook must keep its own path %q, got %d row(s): %v", track, len(files), got)
	}
}

// TestScanLibrary_SeparateBookFoldersStaySeparate pins the other direction:
// two books that share an author folder but each own a book folder must resolve
// to their own folder, so managing one book never touches the other's tracks.
// This is the library-scan counterpart of the mis-grouping in #2672.
func TestScanLibrary_SeparateBookFoldersStaySeparate(t *testing.T) {
	libDir := t.TempDir()
	firstDir := seedAudiobookInsideBookFolder(t, libDir, "Brandon Sanderson", "Mistborn", 2)
	secondDir := seedAudiobookInsideBookFolder(t, libDir, "Brandon Sanderson", "The Well of Ascension", 2)

	s, books, authors, ctx := scannerFixture(t, libDir)
	author := &models.Author{ForeignID: "OL-bs2", Name: "Brandon Sanderson", SortName: "Sanderson, Brandon"}
	if err := authors.Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	first := &models.Book{ForeignID: "OL-mb2", AuthorID: author.ID, Title: "Mistborn", Status: models.BookStatusWanted}
	second := &models.Book{ForeignID: "OL-woa2", AuthorID: author.ID, Title: "The Well of Ascension", Status: models.BookStatusWanted}
	for _, b := range []*models.Book{first, second} {
		if err := books.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	s.ScanLibrary(ctx)

	for _, c := range []struct {
		name string
		book *models.Book
		dir  string
	}{{"Mistborn", first, firstDir}, {"The Well of Ascension", second, secondDir}} {
		files, err := books.ListFiles(ctx, c.book.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0].Path != c.dir {
			got := make([]string, 0, len(files))
			for _, f := range files {
				got = append(got, f.Path)
			}
			t.Errorf("%s must own just its folder %q, got %d row(s): %v", c.name, c.dir, len(files), got)
		}
	}
}
