package core

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// ProcessIDPrefix namespaces execution IDs so that they are recognisable in
// logs and never confused with a PID.
const ProcessIDPrefix = "proc_"

// NewProcessID returns a lexicographically sortable execution ID.
func NewProcessID() string {
	id := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)
	return ProcessIDPrefix + id.String()
}
