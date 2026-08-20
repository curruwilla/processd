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
