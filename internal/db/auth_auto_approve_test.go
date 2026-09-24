package db

import (
	"context"
	"testing"
)

// The per-account request auto approval flag (#2718) must default off, survive
// a round trip through GetByID, and be settable back off. A new account must
// never start approving its own requests just because the column exists.
func TestUserRepo_RequestsAutoApprove(t *testing.T) {
	database, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	repo := NewUserRepo(database)

	u, err := repo.Create(ctx, "reader", "pw")
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestsAutoApprove {
		t.Fatal("a new account defaults to auto approving its requests")
	}

	if err := repo.SetRequestsAutoApprove(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.GetByID(ctx, u.ID); !got.RequestsAutoApprove {
		t.Fatal("auto approve did not persist")
	}
	// List shares scanUser with GetByID, but it is a different query, so check
	// the flag survives there too.
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].RequestsAutoApprove {
		t.Fatalf("List did not carry auto approve: %+v", list)
	}

	if err := repo.SetRequestsAutoApprove(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.GetByID(ctx, u.ID); got.RequestsAutoApprove {
		t.Fatal("turning auto approve off did not persist")
	}
}
