package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/vzdump"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// This file holds the backup reads of DF-3: storage.backups.list and
// storage.backups.summary, the bounded scans and file reads they share
// with the health read, and the log-status memo (D-13). Every filesystem
// access of a configured path runs under bounded (D-15), keyed "scan
// <path>" for the listing and "read <path>/<file>" for one notes or log
// read, each in a root opened and closed inside its flight.

// timestampLayout is BackupProperties.Timestamp: the name's time without a
// zone (A-13).
const timestampLayout = "2006-01-02T15:04:05"

// readWorkers is how many archives' (or bundles') reads of one configured
// path one request runs at once. An item's reads run in turn on one worker,
// and the items of a path are taken in order by the workers; with two, a
// read stalled in the kernel holds only its own worker until commandTimeout
// ends it while the rest of the path's items finish on the other (D-15: the
// stall makes only that item's field unavailable), and a request takes at
// most two of the path's spec.MaxPathFlights slots.
const readWorkers = 2

// Reasons of the backup reads.
const (
	reasonNoNotes = "no notes file"
	reasonNoLog   = "no log file"
	reasonTmpSize = "an archive still being written in a directory has no size yet"
)

// archiveRef is one listed archive and where it is: the configured path it
// was found under and its directory (the path or its dump/).
type archiveRef struct {
	path string // Configured path: the bounded gate's path and the root
	dir  string // The directory field: path, or path + "/dump"
	a    vzdump.Archive
}

// rel is the archive's companion name relative to the root.
func (r archiveRef) rel(name string) string {
	if r.a.Dir == "." {
		return name
	}
	return r.a.Dir + "/" + name
}

// dirScan is one configured path's listing, or why it could not be read.
type dirScan struct {
	path   string
	res    vzdump.DirResult
	reason string // "" when the path was listed
}

// scanBackups lists every configured path concurrently, each under bounded
// keyed "scan <path>". A path that fails, times out or is stuck carries its
// reason; the others are listed.
func (p *Provider) scanBackups(ctx context.Context, paths []string) []dirScan {
	out := make([]dirScan, len(paths))
	fns := make([]func(), len(paths))
	for i, path := range paths {
		fns[i] = func() {
			out[i] = dirScan{path: path}
			res, err := bounded(ctx, p.fl, path, "scan "+path, func() (vzdump.DirResult, error) {
				root, err := p.openRoot(path)
				if err != nil {
					return vzdump.DirResult{}, err
				}
				defer func() { _ = root.Close() }() // read-only; a close error loses nothing
				return vzdump.Scan(root, path, spec.MaxDirEntries), nil
			})
			switch {
			case err != nil:
				out[i].reason = fsReason(path, err)
			case res.Err != nil:
				out[i].res, out[i].reason = res, boundReason(res.Err.Error())
			default:
				out[i].res = res
			}
		}
	}
	if len(fns) > 0 {
		parallel(fns...)
	}
	return out
}

// refs is every archive of the scans, with where it is.
func refs(scans []dirScan) []archiveRef {
	var out []archiveRef
	for _, sc := range scans {
		for _, a := range sc.res.Archives {
			dir := sc.path
			if a.Dir != "." {
				dir = strings.TrimSuffix(sc.path, "/") + "/" + a.Dir
			}
			out = append(out, archiveRef{path: sc.path, dir: dir, a: a})
		}
	}
	return out
}

// directories is the per-directory status of the scans.
func directories(scans []dirScan) spec.Array[spec.BackupDirectory] {
	out := make(spec.Array[spec.BackupDirectory], 0, len(scans))
	for _, sc := range scans {
		out = append(out, spec.BackupDirectory{Path: sc.path, Available: sc.reason == "", Reason: sc.reason,
			Archives: len(sc.res.Archives), UnrecognizedFiles: sc.res.Unrecognized, Truncated: sc.res.Truncated})
	}
	return out
}

// noDirectoryRead is the 503 of a backup read when no configured path could
// be listed: every path's reason, joined in configuration order.
func noDirectoryRead(scans []dirScan) error {
	reasons := make([]string, 0, len(scans))
	for _, sc := range scans {
		if sc.reason == "" {
			return nil
		}
		reasons = append(reasons, sc.reason)
	}
	return unavailable(featureBackups, "no configured directory could be read: "+strings.Join(reasons, "; "))
}

// fsReason is the reason a bounded filesystem access of path failed: the
// timeout or pending error as it is (it names the key), else the path and
// the error without its operation.
func fsReason(path string, err error) string {
	var timeout zfs.TimeoutError
	var pending zfs.PendingError
	if errors.As(err, &timeout) || errors.As(err, &pending) {
		return boundReason(err.Error())
	}
	return boundReason(path + ": " + errText(err))
}

// compareArchives is DF-3's total order: modifiedAt descending, then file,
// then directory, ascending (TA-2).
func compareArchives(x, y archiveRef) int {
	if c := y.a.ModifiedAt.Compare(x.a.ModifiedAt); c != 0 {
		return c
	}
	if c := strings.Compare(x.a.File, y.a.File); c != 0 {
		return c
	}
	return strings.Compare(x.dir, y.dir)
}

// maxArchiveKey bounds a backup cursor's key so its base64url stays within
// spec.MaxCursorLength.
const maxArchiveKey = spec.MaxCursorLength / 4 * 3

// cutMark ends an archiveKey whose directory was cut to fit maxArchiveKey.
// A path holds no NUL, so a whole directory never ends in it.
const cutMark = "\x00"

// archiveKey is an archive's sort key in a cursor: its mtime in Unix
// nanoseconds, a space, its file name (which holds no space), a space and
// its directory. A key over maxArchiveKey is cut and ends in cutMark, so the
// cursor knows its directory is a prefix and not the whole.
func archiveKey(r archiveRef) string {
	k := strconv.FormatInt(r.a.ModifiedAt.UnixNano(), 10) + " " + r.a.File + " " + r.dir
	if len(k) <= maxArchiveKey {
		return k
	}
	return k[:maxArchiveKey-len(cutMark)] + cutMark
}

// archiveCursor is a decoded archiveKey. cut is true when its directory is
// only the prefix of the one the key was made from.
type archiveCursor struct {
	nanos     int64
	file, dir string
	cut       bool
}

// cursorFileRE is a file name a backup cursor may carry: the characters
// of the vzdump grammar.
var cursorFileRE = regexp.MustCompile(`^vzdump-[a-z]+-[0-9]+-[0-9_-]+\.[a-z.]+$`)

func splitArchiveKey(k string) (archiveCursor, bool) {
	ns, rest, ok := strings.Cut(k, " ")
	if !ok || ns == "" || strings.TrimLeft(ns, "-0123456789") != "" {
		return archiveCursor{}, false
	}
	file, dir, ok := strings.Cut(rest, " ")
	n, err := strconv.ParseInt(ns, 10, 64)
	dir, cut := strings.CutSuffix(dir, cutMark)
	if !ok || err != nil || !cursorFileRE.MatchString(file) || !strings.HasPrefix(dir, "/") {
		return archiveCursor{}, false
	}
	return archiveCursor{nanos: n, file: file, dir: dir, cut: cut}, true
}

// atOrBefore reports whether r sorts at or before the cursor. The cursor's
// directory is compared whole, unless the key cut it: then both are cut to
// the same length, which keeps the test monotone over the sorted archives.
// A whole directory that is a proper prefix of r's sorts before it.
func (c archiveCursor) atOrBefore(r archiveRef) bool {
	n := r.a.ModifiedAt.UnixNano()
	if n != c.nanos {
		return n > c.nanos
	}
	if r.a.File != c.file {
		return r.a.File < c.file
	}
	if !c.cut {
		return r.dir <= c.dir
	}
	return r.dir[:min(len(r.dir), len(c.dir))] <= c.dir
}

// backupFillChunk is how many archives of a page backupsList reads the notes
// and logs of before it checks the page against its byte budget.
const backupFillChunk = 50

// backupID is an archive's resource ID: the same file name can be listed
// under two directories (a path and its dump/, a mirror), so the ID names
// the directory too, path-escaped like the other IDs.
func backupID(r archiveRef) string {
	return apitypes.ResourceID(spec.Name, spec.BackupsType, url.PathEscape(r.dir+"/"+r.a.File))
}

// backupsList is storage.backups.list (DF-3): every configured path's
// archives, filtered, in DF-3's order, paged; notes and logs are read only
// for the returned page.
func (p *Provider) backupsList(ctx context.Context, in spec.BackupListInput) (apitypes.List[spec.Backup], error) {
	out := apitypes.List[spec.Backup]{Items: []spec.Backup{}}
	if in.VMID != nil && (*in.VMID < spec.MinVMID || *in.VMID > spec.MaxVMID) {
		return out, invalidInput("/vmid", fmt.Sprintf("must be %d to %d", spec.MinVMID, spec.MaxVMID))
	}
	if in.GuestType != "" && in.GuestType != spec.GuestQemu && in.GuestType != spec.GuestLXC {
		return out, invalidInput("/guestType", "must be qemu or lxc")
	}
	var since time.Time
	if in.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, in.Since)
		if err != nil || !sinceRE.MatchString(in.Since) {
			return out, invalidInput("/since", "is not an RFC 3339 time")
		}
		since = t
	}
	limit, err := pageLimit(in.Limit)
	if err != nil {
		return out, err
	}
	var cur archiveCursor
	if _, err := decodeCursor(in.Cursor, func(k string) bool {
		c, ok := splitArchiveKey(k)
		cur = c
		return ok
	}); err != nil {
		return out, err
	}
	if err := p.require(ctx, featureBackups); err != nil {
		return out, err
	}
	s := p.Settings()
	scans := p.scanBackups(ctx, s.Backups.VzdumpPaths)
	if err := noDirectoryRead(scans); err != nil {
		return out, err
	}
	all := slices.DeleteFunc(refs(scans), func(r archiveRef) bool {
		return in.VMID != nil && r.a.VMID != *in.VMID || in.GuestType != "" && r.a.GuestType != in.GuestType ||
			in.Since != "" && r.a.ModifiedAt.Before(since)
	})
	slices.SortFunc(all, compareArchives)
	atOrBefore := func(archiveRef) bool { return false }
	if in.Cursor != "" {
		atOrBefore = cur.atOrBefore
	}
	page, next := paginate(all, limit, atOrBefore, archiveKey)
	// A-64: notes and logs are untrusted text, so the count alone does not
	// bound the body. The page is filled backupFillChunk archives at a time
	// and ends before the item that would cross the budget, which starts the
	// next page, and nextCursor is the last kept item's as when limit is
	// reached. The archives past the chunk that crossed it are never read.
	for lo := 0; lo < len(page); lo += backupFillChunk {
		chunk := page[lo:min(lo+backupFillChunk, len(page))]
		items := make([]spec.BackupProperties, len(chunk))
		jobs := map[string][]func(){}
		for i, r := range chunk {
			items[i] = archiveProps(r)
			jobs[r.path] = append(jobs[r.path], func() {
				p.fillNotes(ctx, r, &items[i])
				p.fillLog(ctx, r, &items[i])
			})
		}
		runPerPath(ctx, jobs)
		for i, r := range chunk {
			out.Items = append(out.Items, apitypes.Resource[spec.BackupProperties]{
				ID: backupID(r), Type: spec.BackupsType, Name: r.a.File, Host: p.host, Properties: items[i]})
		}
		if n := withinBudget(out.Items, spec.BackupsPageBudgetBytes); n < len(out.Items) {
			out.Items = out.Items[:n:n]
			next = encodeCursor(archiveKey(page[n-1]))
			break
		}
	}
	out.NextCursor = next
	return out, nil
}

// withinBudget is how many of the leading items the JSON array of them
// holds within budget bytes: the items' encodings and the commas between
// them. It is at least 1 for any items, so a page never comes back empty
// (A-64), and len(items) when all fit.
func withinBudget[T any](items []T, budget int) int {
	used := 0
	for i, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			return len(items) // The response's own encoding reports it
		}
		if i > 0 {
			used++ // The comma
		}
		used += len(b)
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// archiveProps is an archive's fields from its name and its stat, before
// its notes and log are read.
func archiveProps(r archiveRef) spec.BackupProperties {
	a := r.a
	bp := spec.BackupProperties{
		File: a.File, Directory: r.dir, GuestType: a.GuestType, VMID: a.VMID, ArchiveFormat: a.Format,
		Compression: a.Compression, Timestamp: a.Timestamp.Format(timestampLayout),
		TimestampZone: spec.TimestampZoneNodeLocal, ModifiedAt: a.ModifiedAt.UTC(), Protected: a.Protected,
		InProgress: a.InProgress, LogStatus: vzdump.LogUnknown,
	}
	if a.InProgress && strings.HasSuffix(a.File, ".tmp") {
		setReason(&bp.Unavailable, "sizeBytes", reasonTmpSize)
	} else {
		bp.SizeBytes = ptrTo(a.Size)
	}
	return bp
}

// fillNotes reads r's notes into bp, or leaves notes null with the reason.
func (p *Provider) fillNotes(ctx context.Context, r archiveRef, bp *spec.BackupProperties) {
	text, truncated, reason := p.readNotes(ctx, r)
	if reason != "" {
		setReason(&bp.Unavailable, "notes", reason)
		return
	}
	bp.Notes, bp.NotesTruncated = &text, truncated
}

// fillLog reads r's log verdict into bp, or leaves it unknown with the
// reason.
func (p *Provider) fillLog(ctx context.Context, r archiveRef, bp *spec.BackupProperties) {
	v, reason := p.readLog(ctx, r)
	if reason != "" {
		setReason(&bp.Unavailable, "logStatus", reason)
		setReason(&bp.Unavailable, "logErrors", reason)
		return
	}
	bp.LogStatus, bp.LogErrors = v.Status, spec.Array[string](v.Errors)
}

// notesText is one notes read.
type notesText struct {
	text      string
	truncated bool
}

// readNotes is the first spec.NotesMaxBytes of r's notes, under bounded
// keyed "read <path>/<file>", or the reason they could not be read.
func (p *Provider) readNotes(ctx context.Context, r archiveRef) (string, bool, string) {
	if r.a.NotesFile == "" {
		return "", false, reasonNoNotes
	}
	rel := r.rel(r.a.NotesFile)
	n, err := bounded(ctx, p.fl, r.path, "read "+r.path+"/"+rel, func() (notesText, error) {
		root, err := p.openRoot(r.path)
		if err != nil {
			return notesText{}, err
		}
		defer func() { _ = root.Close() }() // read-only; a close error loses nothing
		text, truncated, err := vzdump.Notes(root, rel, spec.NotesMaxBytes)
		return notesText{text: text, truncated: truncated}, err
	})
	if err != nil {
		return "", false, fsReason(r.path, err)
	}
	return n.text, n.truncated, ""
}

// readLog is r's log verdict, under bounded keyed "read <path>/<file>":
// the memo's when the log's size and mtime are those of a verdict already
// read (D-13), else read from the log's last spec.LogTailBytes.
func (p *Provider) readLog(ctx context.Context, r archiveRef) (vzdump.LogVerdict, string) {
	if r.a.LogFile == "" {
		return vzdump.LogVerdict{}, reasonNoLog
	}
	rel := r.rel(r.a.LogFile)
	v, err := bounded(ctx, p.fl, r.path, "read "+r.path+"/"+rel, func() (vzdump.LogVerdict, error) {
		root, err := p.openRoot(r.path)
		if err != nil {
			return vzdump.LogVerdict{}, err
		}
		defer func() { _ = root.Close() }() // read-only; a close error loses nothing
		fi, err := root.Stat(rel)
		if err != nil {
			return vzdump.LogVerdict{}, err
		}
		key := logKey{dir: r.dir, file: r.a.LogFile, size: fi.Size(), mtime: fi.ModTime().UnixNano()}
		if v, ok := p.logs.get(key); ok {
			return v, nil
		}
		v, err := vzdump.ReadLog(root, rel, spec.LogTailBytes, spec.MaxLogErrorLines, spec.LogLineMaxBytes)
		if err != nil {
			return vzdump.LogVerdict{}, err
		}
		p.logs.put(key, v)
		return v, nil
	})
	if err != nil {
		return vzdump.LogVerdict{}, fsReason(r.path, err)
	}
	return v, ""
}

// runPerPath runs each configured path's jobs, the paths concurrently and
// each path's jobs in order on at most readWorkers workers. A worker takes
// no further job once ctx is done: the jobs left would only start flights
// nobody waits for. A panic in a job is raised again on the caller's
// goroutine once every job is done, so it reaches the request's recovery.
func runPerPath(ctx context.Context, jobs map[string][]func()) {
	var mu sync.Mutex
	var first any
	var wg sync.WaitGroup
	for _, list := range jobs {
		next := make(chan func(), len(list))
		for _, j := range list {
			next <- j
		}
		close(next)
		for range min(readWorkers, len(list)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range next {
					if ctx.Err() != nil {
						return
					}
					func() {
						defer func() {
							if r := recover(); r != nil {
								mu.Lock()
								if first == nil {
									first = r
								}
								mu.Unlock()
							}
						}()
						j()
					}()
				}
			}()
		}
	}
	wg.Wait()
	if first != nil {
		panic(first)
	}
}

// guestKey is a guest of the summary.
type guestKey struct {
	guestType string
	vmid      int
}

// backupReport is what one read of the backup directories gives the
// summary and the health read: the guests, sorted by guestType then vmid,
// and each directory's status.
type backupReport struct {
	guests      []spec.GuestBackups
	directories spec.Array[spec.BackupDirectory]
	maxAge      time.Duration
}

// readBackupReport lists every configured path, groups every archive by
// guest, reads each guest's latest log, and its notes when wantNames (the
// summary's guestName; health uses none, so it reads none), and adds the expected
// guests with no archive (DF-3). An archive still being written counts in
// its guest's count and totalBytes but is never its latest (A-63): latestAt,
// latestSizeBytes, latestLogStatus, ageHours, guestName and stale come from
// the newest archive that is not in progress, and a guest whose archives are
// all in progress has latestAt null and is stale. It answers 503 when no
// configured path could be listed.
func (p *Provider) readBackupReport(ctx context.Context, s Settings, wantNames bool) (backupReport, error) {
	scans := p.scanBackups(ctx, s.Backups.VzdumpPaths)
	if err := noDirectoryRead(scans); err != nil {
		return backupReport{}, err
	}
	now := p.now()
	maxAge := s.Backups.MaxAge
	type group struct {
		latest *archiveRef // Nil while every archive is in progress
		count  int
		total  int64
	}
	groups := map[guestKey]*group{}
	for _, r := range refs(scans) {
		k := guestKey{r.a.GuestType, r.a.VMID}
		g, ok := groups[k]
		if !ok {
			g = &group{}
			groups[k] = g
		}
		if !r.a.InProgress && (g.latest == nil || compareArchives(r, *g.latest) < 0) {
			g.latest = &r
		}
		g.count++
		g.total += r.a.Size
	}
	expected := map[int]bool{}
	for _, v := range s.Backups.ExpectedGuests {
		expected[v] = true
	}

	keys := make([]guestKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	guests := make([]spec.GuestBackups, len(keys))
	jobs := map[string][]func(){}
	seen := map[int]bool{}
	for i, k := range keys {
		g := groups[k]
		seen[k.vmid] = true
		guests[i] = spec.GuestBackups{GuestType: k.guestType, VMID: k.vmid, LatestLogStatus: vzdump.LogUnknown,
			Count: g.count, TotalBytes: g.total, Stale: true, Expected: expected[k.vmid]}
		if g.latest == nil {
			continue
		}
		gb, r := &guests[i], *g.latest
		a := r.a
		age := now.Sub(a.ModifiedAt).Hours()
		gb.LatestFile, gb.LatestAt = ptrTo(a.File), ptrTo(a.ModifiedAt.UTC())
		gb.LatestTimestamp, gb.LatestSizeBytes = ptrTo(a.Timestamp.Format(timestampLayout)), ptrTo(a.Size)
		gb.AgeHours, gb.Stale = &age, now.Sub(a.ModifiedAt) > maxAge
		jobs[r.path] = append(jobs[r.path], func() {
			if wantNames {
				if text, _, reason := p.readNotes(ctx, r); reason == "" {
					gb.GuestName = guestName(text)
				}
			}
			if v, reason := p.readLog(ctx, r); reason == "" {
				gb.LatestLogStatus = v.Status
			}
		})
	}
	runPerPath(ctx, jobs)
	for _, v := range s.Backups.ExpectedGuests {
		if !seen[v] {
			seen[v] = true
			guests = append(guests, spec.GuestBackups{VMID: v, LatestLogStatus: vzdump.LogUnknown, Stale: true,
				Expected: true})
		}
	}
	slices.SortFunc(guests, func(x, y spec.GuestBackups) int {
		if c := strings.Compare(x.GuestType, y.GuestType); c != 0 {
			return c
		}
		return x.VMID - y.VMID
	})
	return backupReport{guests: guests, directories: directories(scans), maxAge: maxAge}, nil
}

// guestName is the first line of a notes text, nil when it is empty.
func guestName(notes string) *string {
	line, _, _ := strings.Cut(notes, "\n")
	line = strings.TrimSuffix(line, "\r")
	if line == "" {
		return nil
	}
	return &line
}

// backupsSummary is storage.backups.summary (DF-3): one item, current.
func (p *Provider) backupsSummary(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.BackupSummary], error) {
	out := apitypes.List[spec.BackupSummary]{Items: []spec.BackupSummary{}}
	if err := p.require(ctx, featureBackups); err != nil {
		return out, err
	}
	rep, err := p.readBackupReport(ctx, p.Settings(), true)
	if err != nil {
		return out, err
	}
	props := spec.BackupSummaryProperties{Directories: rep.directories, GuestCount: len(rep.guests),
		MaxAgeHours: rep.maxAge.Hours()}
	for _, g := range rep.guests {
		if g.Stale {
			props.StaleCount++
		}
		if g.LatestLogStatus == vzdump.LogError {
			props.FailedCount++
		}
	}
	shown := rep.guests
	if len(shown) > spec.MaxSummaryGuests {
		shown, props.GuestsTruncated = shown[:spec.MaxSummaryGuests:spec.MaxSummaryGuests], true
	}
	props.Guests = append(spec.Array[spec.GuestBackups]{}, shown...)
	out.Items = append(out.Items, resource(p, spec.BackupSummaryType, spec.CurrentName, props))
	return out, nil
}

// logKey is a log verdict's memo key (D-13): the log's directory, name,
// size and mtime, so a log that grows or is rewritten is read again.
type logKey struct {
	dir, file string
	size      int64
	mtime     int64
}

// maxLogMemo bounds the log-status memo (D-13).
const maxLogMemo = 10000

// logMemo is the log-status memo: at most maxLogMemo verdicts, the oldest
// entry evicted first. Its zero value is ready.
type logMemo struct {
	mu    sync.Mutex
	m     map[logKey]vzdump.LogVerdict
	order []logKey // Insertion order, oldest first
}

func (m *logMemo) get(k logKey) (vzdump.LogVerdict, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[k]
	return v, ok
}

func (m *logMemo) put(k logKey, v vzdump.LogVerdict) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = make(map[logKey]vzdump.LogVerdict)
	}
	if _, ok := m.m[k]; ok {
		m.m[k] = v
		return
	}
	if len(m.m) >= maxLogMemo {
		delete(m.m, m.order[0])
		m.order = m.order[1:] // append moves the live entries to a new array as it grows
	}
	m.m[k] = v
	m.order = append(m.order, k)
}

func (m *logMemo) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}
