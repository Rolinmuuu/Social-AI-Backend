// Package pagecursor encodes keyset-pagination cursors.
//
// Every list endpoint pages by a key that is unique and ordered (for example
// (created_at, id)), never by OFFSET: OFFSET re-reads and discards every skipped row, and rows
// inserted meanwhile shift the window so a page repeats or skips items. The cursor is the
// key of the last item returned, as opaque URL-safe base64 JSON, so clients cannot depend on
// its shape.
package pagecursor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrBad is returned for a cursor that cannot be decoded (a client error, 400).
var ErrBad = errors.New("invalid cursor")

// Encode returns the cursor for v.
func Encode(v interface{}) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode fills v from s. An empty s leaves v untouched (first page).
func Decode(s string, v interface{}) error {
	if s == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBad, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: %v", ErrBad, err)
	}
	return nil
}

// Limit clamps a requested page size to [1, max], using def when it is not set.
func Limit(requested, def, max int) int {
	if requested <= 0 {
		return def
	}
	if requested > max {
		return max
	}
	return requested
}
