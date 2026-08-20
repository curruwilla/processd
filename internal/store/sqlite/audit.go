package sqlite

import (
	"context"
	"fmt"

	"github.com/curruwilla/processd/internal/store"
)

// AppendAudit records a state-changing API call, so that every execution can be
// traced back to the token that asked for it.
func (s *Store) AppendAudit(ctx context.Context, entry store.AuditEntry) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	const query = `INSERT INTO audit_log (at, token_name, action, process_id, detail)
		VALUES (?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		formatTime(entry.At), entry.TokenName, entry.Action, entry.ProcessID, entry.Detail,
	)
	if err != nil {
		return fmt.Errorf("appending audit entry: %w", err)
	}

	return nil
}
