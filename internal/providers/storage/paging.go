package storage

import (
	"encoding/base64"
	"regexp"
	"sort"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
)

// The ZFS lists page as the other providers' lists do (the host provider's
// paging.go, nginx's and apt's decodeCursor): a page holds up to limit items
// after the cursor's position, and nextCursor is the unpadded base64url of
// the page's last item's sort key, empty on the last page (DF-2 step 5).
// Pools and datasets sort by name, so the key is the name; snapshots sort by
// createdAt descending then name, so the key is both (snapshotKey). Every
// key is at most 276 bytes (a 255-byte snapshot name, its creation time and
// a separator), whose base64url fits spec.MaxCursorLength, and starts with
// an ASCII byte, so its encoding never starts with "-" or "_".

var (
	cursorRE      = regexp.MustCompile(spec.CursorPattern)
	poolNameRE    = regexp.MustCompile(spec.PoolNamePattern)
	datasetNameRE = regexp.MustCompile(spec.DatasetNamePattern)
	datasetNotRE  = regexp.MustCompile(spec.DatasetNameNotPattern)
)

// pageLimit is the page size of limit: spec.DefaultLimit for 0, else limit
// when it is 1 to spec.MaxLimit. The schema refuses anything else before a
// handler runs; the handler refuses it again for direct callers.
func pageLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return spec.DefaultLimit, nil
	case limit < 1 || limit > spec.MaxLimit:
		return 0, invalidInput("/limit", "must be 1 to 500")
	}
	return limit, nil
}

// encodeCursor is the cursor after the item whose sort key is key.
func encodeCursor(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

// decodeCursor is the sort key a cursor carries, "" for no cursor. A cursor
// this list never issues, one whose key valid refuses, is validation-failed
// at /cursor.
func decodeCursor(c string, valid func(key string) bool) (string, error) {
	if c == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(c) > spec.MaxCursorLength || !cursorRE.MatchString(c) || len(b) == 0 || !valid(string(b)) {
		return "", invalidInput("/cursor", "is not a cursor this server issued")
	}
	return string(b), nil
}

// paginate returns the page of up to limit items of the sorted items that
// sort after the cursor, and the next page's cursor, empty on the last page.
// atOrBefore reports whether an item sorts at or before the cursor's key
// (always false without a cursor); it is monotone over items, so an item
// added or removed between pages neither repeats nor skips another.
func paginate[T any](items []T, limit int, atOrBefore func(T) bool, key func(T) string) ([]T, string) {
	start := sort.Search(len(items), func(i int) bool { return !atOrBefore(items[i]) })
	end := min(start+limit, len(items))
	page := items[start:end:end]
	next := ""
	if end < len(items) {
		next = encodeCursor(key(page[len(page)-1]))
	}
	return page, next
}

// invalidInput is validation-failed naming one input field by its JSON
// Pointer.
func invalidInput(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}

// listedNameRE and listedSnapshotRE match the names ZFS itself accepts,
// which add a space to the spec patterns' characters (OpenZFS
// zfs_component_namecheck's valid_char), so a cursor issued for any listed
// pool, dataset or snapshot decodes again.
var (
	listedNameRE     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.: /-]*$`)
	listedSnapshotRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.: /-]*@[A-Za-z0-9_.: -]+$`)
)

// validListedName is the cursor check of the pool and dataset lists.
func validListedName(name string) bool {
	return len(name) <= spec.MaxNameLength && listedNameRE.MatchString(name) && !datasetNotRE.MatchString(name)
}

// validPoolName reports whether name is a pool name (spec.PoolNamePattern).
func validPoolName(name string) bool { return poolNameRE.MatchString(name) }

// validDatasetName reports whether name is a dataset path: the pattern, at
// most spec.MaxNameLength bytes and no "." or ".." component.
func validDatasetName(name string) bool {
	return len(name) <= spec.MaxNameLength && datasetNameRE.MatchString(name) && !datasetNotRE.MatchString(name)
}
