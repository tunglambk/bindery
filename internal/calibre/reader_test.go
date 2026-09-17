package calibre

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// buildFixtureLibrary builds a minimal Calibre-shaped library under dir:
//
//	dir/
//	  metadata.db              -- SQLite with Calibre's relevant tables
//	  Alice Author/
//	    Book One (1)/cover.jpg, bookone.epub
//	    Book Two (2)/cover.jpg, booktwo.epub, booktwo.mobi
//	  Bob Baker/
//	    No Cover (3)/nocover.pdf
//
// The schema mirrors the subset of Calibre's metadata.db that Reader
// queries. Reader only selects columns it needs, so extra columns on
// real Calibre installs are a compatibility no-op.
func buildFixtureLibrary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// Create per-book folders + format files + covers.
	layout := []struct {
		folder  string
		files   []string
		noCover bool
	}{
		{folder: "Alice Author/Book One (1)", files: []string{"bookone.epub"}},
		{folder: "Alice Author/Book Two (2)", files: []string{"booktwo.epub", "booktwo.mobi"}},
		{folder: "Bob Baker/No Cover (3)", files: []string{"nocover.pdf"}, noCover: true},
	}
	for _, l := range layout {
		dir := filepath.Join(root, l.folder)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if !l.noCover {
			if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("cover"), 0o644); err != nil {
				t.Fatalf("write cover: %v", err)
			}
		}
		for _, f := range l.files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("book"), 0o644); err != nil {
				t.Fatalf("write %s: %v", f, err)
			}
		}
	}

	dbPath := filepath.Join(root, metadataDB)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range fixtureSchema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec schema %q: %v", stmt, err)
		}
	}
	for _, stmt := range fixtureSeed {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return root
}

// fixtureSchema is the subset of Calibre's metadata.db schema that Reader
// touches. Column sets match what real Calibre ships (verified against
// Calibre 7.x); table definitions omit indexes and triggers because Reader
// doesn't depend on them.
var fixtureSchema = []string{
	`CREATE TABLE books (
		id INTEGER PRIMARY KEY,
		title TEXT NOT NULL DEFAULT 'Unknown',
		sort TEXT,
		timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		pubdate TIMESTAMP DEFAULT '0101-01-01 00:00:00+00:00',
		series_index REAL NOT NULL DEFAULT 1.0,
		author_sort TEXT,
		isbn TEXT DEFAULT '',
		path TEXT NOT NULL DEFAULT '',
		flags INTEGER NOT NULL DEFAULT 1,
		uuid TEXT,
		has_cover BOOL DEFAULT 0,
		last_modified TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE authors (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		sort TEXT,
		link TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE books_authors_link (
		id INTEGER PRIMARY KEY,
		book INTEGER NOT NULL,
		author INTEGER NOT NULL,
		UNIQUE(book, author)
	)`,
	`CREATE TABLE series (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		sort TEXT
	)`,
	`CREATE TABLE books_series_link (
		id INTEGER PRIMARY KEY,
		book INTEGER NOT NULL,
		series INTEGER NOT NULL
	)`,
	`CREATE TABLE data (
		id INTEGER PRIMARY KEY,
		book INTEGER NOT NULL,
		format TEXT NOT NULL,
		uncompressed_size INTEGER NOT NULL,
		name TEXT NOT NULL
	)`,
	`CREATE TABLE identifiers (
		id INTEGER PRIMARY KEY,
		book INTEGER NOT NULL,
		type TEXT NOT NULL DEFAULT 'isbn',
		val TEXT NOT NULL
	)`,
}

// fixtureSeed inserts a deterministic tiny library the tests can assert on.
// Book 1 (Book One): single author (Alice), single format (epub), one series
//
//	position, ISBN.
//
// Book 2 (Book Two): two authors (Alice primary, Carol secondary), two
//
//	formats (epub + mobi), same series position 2, no ISBN.
//
// Book 3 (No Cover): different author (Bob), single PDF, no series, no
//
//	cover file on disk.
var fixtureSeed = []string{
	`INSERT INTO authors (id, name, sort) VALUES
		(1, 'Alice Author', 'Author, Alice'),
		(2, 'Bob Baker', 'Baker, Bob'),
		(3, 'Carol Coauthor', 'Coauthor, Carol')`,
	`INSERT INTO series (id, name) VALUES (1, 'Example Saga')`,
	`INSERT INTO books (id, title, sort, pubdate, path, series_index) VALUES
		(1, 'Book One',   'Book One',   '2021-03-04 00:00:00+00:00', 'Alice Author/Book One (1)',   1.0),
		(2, 'Book Two',   'Book Two',   '2022-06-15 00:00:00+00:00', 'Alice Author/Book Two (2)',   2.0),
		(3, 'No Cover',   'No Cover',   '0101-01-01 00:00:00+00:00', 'Bob Baker/No Cover (3)',      1.0)`,
	`INSERT INTO books_authors_link (book, author) VALUES (1, 1), (2, 1), (2, 3), (3, 2)`,
	`INSERT INTO books_series_link (book, series) VALUES (1, 1), (2, 1)`,
	`INSERT INTO data (book, format, uncompressed_size, name) VALUES
		(1, 'EPUB', 12345, 'bookone'),
		(2, 'EPUB', 23456, 'booktwo'),
		(2, 'MOBI', 34567, 'booktwo'),
		(3, 'PDF',  45678, 'nocover')`,
	`INSERT INTO identifiers (book, type, val) VALUES (1, 'isbn', '9781234567890')`,
}

func TestOpenReader_MissingMetadataDB(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenReader(dir)
	if err == nil {
		t.Fatal("expected error for dir without metadata.db")
	}
}

func TestOpenReader_EmptyPath(t *testing.T) {
	_, err := OpenReader("")
	if err == nil {
		t.Fatal("expected error for empty library_path")
	}
}

func TestReader_Count(t *testing.T) {
	r := mustOpenFixture(t)
	defer r.Close()
	n, err := r.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}
}

func TestReader_Books_FullShape(t *testing.T) {
	r := mustOpenFixture(t)
	defer r.Close()

	var got []CalibreBook
	err := r.Books(context.Background(), func(b CalibreBook) error {
		got = append(got, b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d books, want 3", len(got))
	}

	// Book 1: one author, one EPUB, series position 1.0, ISBN set, cover exists.
	b := got[0]
	if b.CalibreID != 1 || b.Title != "Book One" {
		t.Errorf("book 1 = %+v", b)
	}
	if len(b.Authors) != 1 || b.Authors[0].Name != "Alice Author" {
		t.Errorf("book 1 authors = %+v", b.Authors)
	}
	if b.Series == nil || b.Series.Name != "Example Saga" || b.Series.Position != 1.0 {
		t.Errorf("book 1 series = %+v", b.Series)
	}
	if b.ISBN != "9781234567890" {
		t.Errorf("book 1 isbn = %q", b.ISBN)
	}
	if len(b.Formats) != 1 || b.Formats[0].Format != "EPUB" {
		t.Errorf("book 1 formats = %+v", b.Formats)
	}
	if _, err := os.Stat(b.Formats[0].AbsolutePath); err != nil {
		t.Errorf("book 1 format path %q not resolvable: %v", b.Formats[0].AbsolutePath, err)
	}
	if b.CoverPath == "" {
		t.Error("book 1 cover should be populated")
	}
	if b.PublishDate == nil || b.PublishDate.Year() != 2021 {
		t.Errorf("book 1 pubdate = %v", b.PublishDate)
	}

	// Book 2: multi-author (ordering preserved), two formats.
	b = got[1]
	if len(b.Authors) != 2 {
		t.Fatalf("book 2 authors = %+v", b.Authors)
	}
	names := []string{b.Authors[0].Name, b.Authors[1].Name}
	sort.Strings(names)
	if names[0] != "Alice Author" || names[1] != "Carol Coauthor" {
		t.Errorf("book 2 author set = %v", names)
	}
	if len(b.Formats) != 2 {
		t.Fatalf("book 2 formats = %+v", b.Formats)
	}
	formats := []string{b.Formats[0].Format, b.Formats[1].Format}
	sort.Strings(formats)
	if formats[0] != "EPUB" || formats[1] != "MOBI" {
		t.Errorf("book 2 formats = %v", formats)
	}
	if b.ISBN != "" {
		t.Errorf("book 2 should have no ISBN, got %q", b.ISBN)
	}
	if b.Series == nil || b.Series.Position != 2.0 {
		t.Errorf("book 2 series position = %+v", b.Series)
	}

	// Book 3: no cover, no series, sentinel pubdate becomes nil.
	b = got[2]
	if b.Series != nil {
		t.Errorf("book 3 should have no series, got %+v", b.Series)
	}
	if b.PublishDate != nil {
		t.Errorf("book 3 pubdate should be nil, got %v", b.PublishDate)
	}
	if b.CoverPath != "" {
		t.Errorf("book 3 should have no cover, got %q", b.CoverPath)
	}
	if len(b.Formats) != 1 || b.Formats[0].Format != "PDF" {
		t.Errorf("book 3 formats = %+v", b.Formats)
	}
}

// TestReader_Books_UnescapesAuthorCommas is the regression for #2666. Calibre
// writes a literal comma in an author name as "|" because a comma separates
// authors in its comma-joined author columns, and its own readers turn the
// pipe back into a comma (calibre/db/write.py get_adapter). Both the name and
// the sort column carry the escaped form, and the unescaping has to stay
// inside one author row so a co-author pair is still two authors.
func TestReader_Books_UnescapesAuthorCommas(t *testing.T) {
	root := buildFixtureLibrary(t)
	db, err := sql.Open("sqlite", filepath.Join(root, metadataDB))
	if err != nil {
		t.Fatalf("open fixture writer: %v", err)
	}
	extra := []string{
		// One escaped comma, in the name and in the sort.
		`INSERT INTO authors (id, name, sort) VALUES (4, 'Scalzi| John', 'Scalzi| John')`,
		// No pipe: the name and the real comma in the sort must be left alone.
		`INSERT INTO authors (id, name, sort) VALUES (5, 'Ursula K. Le Guin', 'Le Guin, Ursula K.')`,
		// Two authors on one book, both escaped.
		`INSERT INTO authors (id, name, sort) VALUES (6, 'Gaiman| Neil', 'Gaiman| Neil')`,
		`INSERT INTO authors (id, name, sort) VALUES (7, 'McKean| Dave', 'McKean| Dave')`,
		`INSERT INTO books (id, title, sort, path) VALUES (4, 'Old Man''s War', 'Old Man''s War', '')`,
		`INSERT INTO books (id, title, sort, path) VALUES (5, 'Good Omens', 'Good Omens', '')`,
		`INSERT INTO books (id, title, sort, path) VALUES (6, 'The Dispossessed', 'The Dispossessed', '')`,
		`INSERT INTO books_authors_link (book, author) VALUES (4, 4), (5, 6), (5, 7), (6, 5)`,
	}
	for _, stmt := range extra {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture writer: %v", err)
	}

	r, err := OpenReader(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	byID := map[int64]CalibreBook{}
	if err := r.Books(context.Background(), func(b CalibreBook) error {
		byID[b.CalibreID] = b
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Book 4: the escaped comma becomes a comma again in both columns.
	authors := byID[4].Authors
	if len(authors) != 1 {
		t.Fatalf("book 4 authors = %+v, want 1 author", authors)
	}
	if authors[0].Name != "Scalzi, John" || authors[0].Sort != "Scalzi, John" {
		t.Errorf("book 4 author = %+v, want name and sort %q", authors[0], "Scalzi, John")
	}
	// Book 6: no pipe to unescape, so the name is unchanged and the comma the
	// sort form already has stays put.
	authors = byID[6].Authors
	if len(authors) != 1 || authors[0].Name != "Ursula K. Le Guin" || authors[0].Sort != "Le Guin, Ursula K." {
		t.Errorf("book 6 author = %+v, want the stored name and sort unchanged", authors)
	}
	// Book 5: two escaped authors stay two authors, each unescaped.
	authors = byID[5].Authors
	if len(authors) != 2 {
		t.Fatalf("book 5 authors = %+v, want 2 separate authors", authors)
	}
	names := []string{authors[0].Name, authors[1].Name}
	sort.Strings(names)
	if names[0] != "Gaiman, Neil" || names[1] != "McKean, Dave" {
		t.Errorf("book 5 authors = %v, want the two unescaped names", names)
	}
}

// TestReader_Books_StopsOnError: returning an error from the visitor must
// abort the walk — the importer relies on this to honour context cancel.
func TestReader_Books_StopsOnError(t *testing.T) {
	r := mustOpenFixture(t)
	defer r.Close()

	var seen int
	abort := &stopErr{}
	err := r.Books(context.Background(), func(b CalibreBook) error {
		seen++
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("expected abort sentinel, got %v", err)
	}
	if seen != 1 {
		t.Errorf("visitor should have stopped after first call, saw %d", seen)
	}
}

func TestParseCalibreDate(t *testing.T) {
	cases := []struct {
		in   string
		want *time.Time
	}{
		{"", nil},
		{"0101-01-01 00:00:00+00:00", nil},
		{"2021-03-04 00:00:00+00:00", ptrTime(2021, 3, 4)},
		{"2021-03-04", ptrTime(2021, 3, 4)},
		{"not a date", nil},
	}
	for _, tc := range cases {
		got := parseCalibreDate(tc.in)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("parseCalibreDate(%q) = %v, want nil", tc.in, got)
		case tc.want != nil && got == nil:
			t.Errorf("parseCalibreDate(%q) = nil, want %v", tc.in, *tc.want)
		case tc.want != nil && got != nil && !got.Equal(*tc.want):
			t.Errorf("parseCalibreDate(%q) = %v, want %v", tc.in, *got, *tc.want)
		}
	}
}

type stopErr struct{}

func (stopErr) Error() string { return "stop" }

func ptrTime(y, m, d int) *time.Time {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return &t
}

func mustOpenFixture(t *testing.T) *Reader {
	t.Helper()
	root := buildFixtureLibrary(t)
	r, err := OpenReader(root)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	return r
}

// openWALWriter switches the fixture's metadata.db to WAL mode and returns a
// pinned connection with automatic checkpointing disabled, so anything
// written through it stays in metadata.db-wal for as long as the connection
// is open. This is the shape a long running Calibre (content server,
// Calibre-Web-Automated) leaves the library in between checkpoints.
func openWALWriter(t *testing.T, root string) *sql.Conn {
	t.Helper()
	ctx := context.Background()
	w, err := sql.Open("sqlite", filepath.Join(root, metadataDB))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	conn, err := w.Conn(ctx)
	if err != nil {
		t.Fatalf("pin writer conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0"} {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	return conn
}

// TestReader_SeesUncheckpointedWAL is the regression for #2631: a row that
// Calibre has committed but not yet checkpointed must be visible to the
// reader. Opening with immutable=1 made SQLite ignore the -wal file, so
// Bindery read a snapshot as of the last checkpoint.
func TestReader_SeesUncheckpointedWAL(t *testing.T) {
	root := buildFixtureLibrary(t)
	ctx := context.Background()
	w := openWALWriter(t, root)
	if _, err := w.ExecContext(ctx, `INSERT INTO books (id, title, sort, path) VALUES (4, 'Book Four', 'Book Four', 'Alice Author/Book Four (4)')`); err != nil {
		t.Fatalf("insert into wal: %v", err)
	}
	if st, err := os.Stat(filepath.Join(root, metadataDB+"-wal")); err != nil || st.Size() == 0 {
		t.Fatalf("precondition: expected a non-empty metadata.db-wal, stat err=%v", err)
	}

	r, err := OpenReader(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n, err := r.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("Count = %d, want 4 (row committed to the WAL but not checkpointed is invisible)", n)
	}
}

// TestOpenReader_ReadOnlyDirFallsBackToImmutable covers the one case where
// a plain `mode=ro` open cannot work: a WAL mode metadata.db in a directory
// Bindery cannot write to, with no -shm file present (Calibre not running).
// SQLite refuses that open outright, so the reader must fall back to
// immutable=1 rather than fail the import.
func TestOpenReader_ReadOnlyDirFallsBackToImmutable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := buildFixtureLibrary(t)
	w, err := sql.Open("sqlite", filepath.Join(root, metadataDB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	// Closing the last connection checkpoints and removes -wal and -shm.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, metadataDB+"-shm")); !os.IsNotExist(err) {
		t.Fatalf("precondition: -shm should be absent, stat err=%v", err)
	}
	if err := os.Chmod(filepath.Join(root, metadataDB), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	r, err := OpenReader(root)
	if err != nil {
		t.Fatalf("OpenReader should fall back to immutable=1, got: %v", err)
	}
	defer r.Close()
	n, err := r.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}
}

// TestOpenReader_NetworkFSProbeFailureFallsBack simulates a library on NFS
// or SMB, where the plain read-only open of a WAL database fails with an
// IOERR_SHM family code rather than READONLY or CANTOPEN. Any probe failure
// must retry immutable, which then reads the last checkpoint: the row still
// sitting in the WAL is invisible, proving the immutable handle was used.
func TestOpenReader_NetworkFSProbeFailureFallsBack(t *testing.T) {
	root := buildFixtureLibrary(t)
	ctx := context.Background()
	w := openWALWriter(t, root)
	if _, err := w.ExecContext(ctx, `INSERT INTO books (id, title, sort, path) VALUES (4, 'Book Four', 'Book Four', 'Alice Author/Book Four (4)')`); err != nil {
		t.Fatalf("insert into wal: %v", err)
	}

	var probed []bool
	probe := func(ctx context.Context, conn *sql.DB, immutable bool) error {
		probed = append(probed, immutable)
		if !immutable {
			return errors.New("disk I/O error (4618)")
		}
		return probeRead(ctx, conn, immutable)
	}
	conn, err := openReadOnlyWith(filepath.Join(root, metadataDB), probe)
	if err != nil {
		t.Fatalf("expected immutable fallback, got: %v", err)
	}
	defer conn.Close()
	if len(probed) != 2 || probed[0] || !probed[1] {
		t.Fatalf("probe calls (immutable flag) = %v, want [false true]", probed)
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3 (immutable handle ignores the WAL row)", n)
	}
}

// TestOpenReader_NotASQLiteFile makes sure the widened fallback does not
// swallow real breakage: a metadata.db that is not a database fails both
// probes and OpenReader returns an error.
func TestOpenReader_NotASQLiteFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, metadataDB), []byte("this is not a sqlite database, just some text padding it out past the header size"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(root)
	if err == nil {
		r.Close()
		t.Fatal("expected an error opening a non SQLite metadata.db")
	}
}

// TestOpenReadOnlyWaitsOutABusyLock is the v1.36.2 review finding on #2631:
// the mode=ro open carried no busy timeout, so a probe that landed while
// Calibre held a lock got SQLITE_BUSY at once and the run fell back to
// immutable=1, the stale read #2631 fixed. With the timeout the probe waits
// for the lock and no fallback happens, which the fallback's warning shows.
func TestOpenReadOnlyWaitsOutABusyLock(t *testing.T) {
	root := buildFixtureLibrary(t)
	ctx := context.Background()
	w, err := sql.Open("sqlite", filepath.Join(root, metadataDB))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	conn, err := w.Conn(ctx)
	if err != nil {
		t.Fatalf("pin writer conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = conn.ExecContext(ctx, "COMMIT")
	}()

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r, err := OpenReader(root)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()
	if logs.Len() > 0 {
		t.Errorf("the reader fell back while Calibre held a lock for 300ms: %s", logs.String())
	}
}
