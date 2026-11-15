package host

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// The list operations page as nginx's and apt's do (internal/providers/nginx
// reads.go paginate and decodeCursor): a page holds up to limit items after
// the cursor's, and nextCursor is the unpadded base64url of the page's last
// item name, empty on the last page. Two things differ:
//   - A list may be sorted by a key other than its item names
//     (filesystems: by mount point, named by the path-escaped mount point,
//     whose byte order differs), so a cursor's name is mapped back to its
//     key and the page starts after that key.
//   - A cursor never exceeds spec.MaxCursorLength. A name whose base64url
//     would (a mount point can be 4095 bytes) gets the long form instead:
//     base64url of a 0x00 byte, the first cursorDigestLen bytes of the
//     key's SHA-256, then as much of the key as fits. No item name holds a
//     NUL, so the forms cannot be confused.

var cursorRE = regexp.MustCompile(spec.CursorPattern)

const (
	longCursorMark  = 0x00
	cursorDigestLen = 16
	// maxCursorBytes is the most bytes whose unpadded base64url fits
	// spec.MaxCursorLength.
	maxCursorBytes = spec.MaxCursorLength / 4 * 3
)

// cursorPos is where a page starts: after the item whose key is key, or for
// a long cursor (digest set) after the item whose key starts with key and
// has digest. The zero value is the start of the list.
type cursorPos struct {
	set    bool
	key    string
	digest []byte
}

// keyOfName maps a cursor's item name back to its sort key. ok is false
// for a name this list never issues, which refuses the cursor.
type keyOfName func(name string) (key string, ok bool)

// nameIsKey is keyOfName for a list sorted by its item names.
func nameIsKey(name string) (string, bool) { return name, name != "" }

// paginate returns the page of items (sorted ascending by key) after
// after, of up to limit items (spec.DefaultLimit when 0 or less, at most
// spec.MaxLimit), and the next page's cursor, empty on the last page.
//
// After a normal cursor the page starts after every item with its key;
// when that item is gone, at the first greater key, so nothing is skipped
// or repeated. After a long cursor whose item is gone it starts at the
// first key with the cursor's prefix: items sharing that prefix may repeat,
// none is skipped.
func paginate[T any](items []T, limit int, after cursorPos, key, name func(T) string) ([]T, string) {
	if limit <= 0 {
		limit = spec.DefaultLimit
	}
	limit = min(limit, spec.MaxLimit)
	start := 0
	if after.set {
		cmpKey := func(it T, k string) int { return strings.Compare(key(it), k) }
		lo, found := slices.BinarySearchFunc(items, after.key, cmpKey)
		switch {
		case after.digest != nil:
			start = lo
			for i := lo; i < len(items) && strings.HasPrefix(key(items[i]), after.key); i++ {
				if sum := sha256.Sum256([]byte(key(items[i]))); string(sum[:cursorDigestLen]) == string(after.digest) {
					start = i + 1
					break
				}
			}
		case found:
			start = lo + 1
			for start < len(items) && key(items[start]) == after.key {
				start++
			}
		default:
			start = lo
		}
	}
	end := min(start+limit, len(items))
	page := items[start:end:end]
	next := ""
	if end < len(items) {
		last := page[len(page)-1]
		next = encodeCursor(name(last), key(last))
	}
	return page, next
}

// encodeCursor is the cursor after the item named name with key: the
// unpadded base64url of name, or the long form when that would exceed
// spec.MaxCursorLength or start with "-" or "_" (spec.CursorPattern).
func encodeCursor(name, key string) string {
	if c := base64.RawURLEncoding.EncodeToString([]byte(name)); len(c) <= spec.MaxCursorLength && cursorRE.MatchString(c) {
		return c
	}
	sum := sha256.Sum256([]byte(key))
	b := make([]byte, 0, maxCursorBytes)
	b = append(b, longCursorMark)
	b = append(b, sum[:cursorDigestLen]...)
	b = append(b, key[:min(len(key), maxCursorBytes-len(b))]...)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor is the position a cursor from paginate carries. A cursor
// this list never issues is refused as validation-failed at /cursor.
func decodeCursor(c string, keyOf keyOfName) (cursorPos, error) {
	if c == "" {
		return cursorPos{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(c) > spec.MaxCursorLength || !cursorRE.MatchString(c) || len(b) == 0 {
		return cursorPos{}, invalidCursor()
	}
	if b[0] == longCursorMark {
		if len(b) < 1+cursorDigestLen {
			return cursorPos{}, invalidCursor()
		}
		return cursorPos{set: true, digest: b[1 : 1+cursorDigestLen], key: string(b[1+cursorDigestLen:])}, nil
	}
	key, ok := keyOf(string(b))
	if !ok {
		return cursorPos{}, invalidCursor()
	}
	return cursorPos{set: true, key: key}, nil
}

func invalidCursor() *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: "/cursor", Message: "is not a cursor this server issued"}})
}
