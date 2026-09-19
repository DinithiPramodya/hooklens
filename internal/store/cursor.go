package store

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Cursor marks a position in a request list.
//
// Both fields are needed. received_at alone is not unique -- two webhooks can
// arrive in the same microsecond -- and a cursor that cannot tell two rows
// apart either skips one or returns it forever.
type Cursor struct {
	ReceivedAt time.Time
	ID         string
}

// ErrBadCursor is returned for a cursor that cannot be decoded.
var ErrBadCursor = errors.New("malformed cursor")

// String encodes the cursor as an opaque token.
//
// Opaque rather than "?after_id=01a0b975-...&after_time=..." on purpose. Expose
// the parts and clients start constructing them by hand, at which point the
// internal representation is a public API and can never change. base64 is not
// security -- anyone can decode it in a second -- it is a signal that the value
// is ours to define.
//
// RFC3339 with nanoseconds because the default time.Time text format is lossy
// at that precision, and losing precision in a cursor means skipping rows.
func (c Cursor) String() string {
	raw := c.ReceivedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor decodes a cursor produced by Cursor.String.
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}

	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return Cursor{}, ErrBadCursor
	}

	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	return Cursor{ReceivedAt: at, ID: id}, nil
}
