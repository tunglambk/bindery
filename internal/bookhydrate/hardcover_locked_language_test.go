package bookhydrate

import (
	"context"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A manual language edit locks the field (#1237), and locked has to mean locked
// even when the value the user saved is empty: clearing a wrong language is a
// decision, not a gap for the provider to fill. Hardcover edition hydration
// tested only `book.Language == ""`, so the audio edition's language was written
// straight over the lock (#2757). The other three cases pin the boundary: an
// unlocked empty language must still be filled, and a value that is already
// there must be left alone whether or not it is locked.
func TestHydrateHardcoverEditionsRespectsLanguageLock(t *testing.T) {
	tests := []struct {
		name     string
		language string
		locked   bool
		want     string
	}{
		{name: "locked empty survives", locked: true, want: ""},
		{name: "locked non-empty survives", language: "fre", locked: true, want: "fre"},
		{name: "unlocked empty is filled", want: "ger"},
		{name: "unlocked non-empty is kept", language: "eng", want: "eng"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			books, editions, book, ctx := newHydrateBook(t, "hc:locked-language", "hardcover", models.MediaTypeAudiobook)
			audioASIN := "b222222222"
			book.Language = tc.language
			if tc.locked {
				book.LockField(models.BookFieldLanguage)
			}
			if err := books.Update(ctx, book); err != nil {
				t.Fatal(err)
			}

			result := HydrateHardcoverEditions(ctx, Options{
				Book:     book,
				Provider: "hardcover",
				Editions: editions,
				Books:    books,
				FetchEditions: func(context.Context, string) ([]models.Edition, error) {
					return []models.Edition{{
						ForeignID: "hc:audio",
						Title:     "Audio",
						ASIN:      &audioASIN,
						Format:    "Audiobook",
						Language:  "ger",
						Monitored: true,
					}}, nil
				},
				Enricher: &fakeAudiobookEnricher{},
			})
			if result.Err != nil {
				t.Fatalf("hydrate err = %v", result.Err)
			}
			// The audio edition was accepted and its ASIN promoted, so a pass
			// here can't come from hydration not running at all.
			if !result.ASINPromoted || book.ASIN != "B222222222" {
				t.Fatalf("hydration did not process the audio edition: %+v", result)
			}
			if book.Language != tc.want {
				t.Errorf("language after hydration = %q, want %q", book.Language, tc.want)
			}
			stored, err := books.GetByID(ctx, book.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Language != tc.want {
				t.Errorf("persisted language = %q, want %q", stored.Language, tc.want)
			}
		})
	}
}
