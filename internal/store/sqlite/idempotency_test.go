package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
)

func TestStore_Idempotency(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	if _, err := db.FindIdempotency(ctx, "absent"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("FindIdempotency() returned %v, want core.ErrNotFound", err)
	}

	record := store.Idempotency{
		Key:         "key-1",
		RequestHash: "hash-1",
		ProcessID:   "proc_1",
		CreatedAt:   time.Now().UTC(),
	}

	if err := db.SaveIdempotency(ctx, record); err != nil {
		t.Fatalf("SaveIdempotency() returned %v, want nil", err)
	}

	got, err := db.FindIdempotency(ctx, "key-1")
	if err != nil {
		t.Fatalf("FindIdempotency() returned %v, want nil", err)
	}

	if got.ProcessID != "proc_1" || got.RequestHash != "hash-1" {
		t.Errorf("record = %+v, want the saved one", got)
	}
}

// The claim is what makes the key useful: two copies of the same request
// cannot both take it, so they cannot both start the work.
func TestStore_Idempotency_claim(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	first := store.Idempotency{Key: "key-1", RequestHash: "hash-1", ProcessID: "proc_1", CreatedAt: time.Now().UTC()}

	if err := db.SaveIdempotency(ctx, first); err != nil {
		t.Fatalf("SaveIdempotency() returned %v, want nil", err)
	}

	// The same execution re-claiming its own key succeeds.
	if err := db.SaveIdempotency(ctx, first); err != nil {
		t.Errorf("re-claiming the same key returned %v, want nil", err)
	}

	second := store.Idempotency{Key: "key-1", RequestHash: "hash-1", ProcessID: "proc_2", CreatedAt: time.Now().UTC()}

	if err := db.SaveIdempotency(ctx, second); !errors.Is(err, core.ErrIdempotencyClaimed) {
		t.Errorf("SaveIdempotency() returned %v, want core.ErrIdempotencyClaimed", err)
	}

	// Releasing is scoped to the holder: a loser cannot free the winner's claim.
	if err := db.DeleteIdempotency(ctx, "key-1", "proc_2"); err != nil {
		t.Fatalf("DeleteIdempotency() returned %v, want nil", err)
	}

	if _, err := db.FindIdempotency(ctx, "key-1"); err != nil {
		t.Errorf("FindIdempotency() returned %v, want the winner's claim intact", err)
	}

	if err := db.DeleteIdempotency(ctx, "key-1", "proc_1"); err != nil {
		t.Fatalf("DeleteIdempotency() returned %v, want nil", err)
	}

	if _, err := db.FindIdempotency(ctx, "key-1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("FindIdempotency() returned %v, want core.ErrNotFound", err)
	}
}

func TestStore_AppendAudit(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	entry := store.AuditEntry{
		At:        time.Now().UTC(),
		TokenName: "billing",
		Action:    "create",
		ProcessID: "proc_1",
		Detail:    "invoice",
	}

	if err := db.AppendAudit(ctx, entry); err != nil {
		t.Fatalf("AppendAudit() returned %v, want nil", err)
	}

	var (
		token  string
		action string
	)

	row := db.db.QueryRowContext(ctx, `SELECT token_name, action FROM audit_log WHERE process_id = ?`, "proc_1")
	if err := row.Scan(&token, &action); err != nil {
		t.Fatalf("reading audit entry: %v", err)
	}

	if token != "billing" || action != "create" {
		t.Errorf("audit entry = (%s, %s), want (billing, create)", token, action)
	}
}
