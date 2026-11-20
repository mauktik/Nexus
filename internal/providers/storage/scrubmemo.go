package storage

import (
	"fmt"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// The lastScrubSource words (D-14).
const (
	scrubSourceStatus     = "status"
	scrubSourceRemembered = "remembered"
)

// reasonNoScrub is why a pool whose scan shows function none has no last
// scrub: none was ever recorded, which health reports as scrub-overdue.
const reasonNoScrub = "no scrub has been recorded on this pool"

// scrubMemo is the last-scrub memo of D-14: each pool's end time of the last
// scrub a zpool status showed finished, fed by every status the provider
// parses. It lives in memory only, so a new nexusd starts without it. The
// zero value is empty and ready.
type scrubMemo struct {
	mu   sync.Mutex
	last map[string]time.Time // pool → end of its last finished scrub
}

// observe records s when it is a finished scrub with an end time, keeping
// the later of that and what is already remembered. Every other scan is
// ignored: a scrub scanning, paused or canceled (whose end time is the
// cancel time), a resilver, an error scrub, none or unknown.
func (m *scrubMemo) observe(pool string, s *zfs.Scan) {
	if s == nil || s.Function != zfs.ScanScrub || s.State != zfs.ScanFinished || s.End == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[string]time.Time{}
	}
	if prev, ok := m.last[pool]; !ok || s.End.After(prev) {
		m.last[pool] = *s.End
	}
}

// get is pool's remembered last finished scrub.
func (m *scrubMemo) get(pool string) (time.Time, bool) {
	if m == nil {
		return time.Time{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.last[pool]
	return t, ok
}

// lastScrub fills out's lastScrubFinishedAt, lastScrubAgeDays and
// lastScrubSource from the scan shown, s, which observe has already seen
// (DF-2 step 6, D-14):
//   - a finished scrub: its end time, source status;
//   - function none: null, with reasonNoScrub, source "";
//   - anything else (a scrub scanning, paused or canceled, a resilver, an
//     error scrub, an unknown word): the memo's time, source remembered, or
//     null with the D-14 reason naming the scan shown, source "".
//
// The age is (now − lastScrubFinishedAt) in days of 24 h.
func (m *scrubMemo) lastScrub(pool string, s *zfs.Scan, now time.Time, out *spec.Scan) {
	var at time.Time
	reason := ""
	switch {
	case s.Function == zfs.ScanScrub && s.State == zfs.ScanFinished && s.End != nil:
		at, out.LastScrubSource = *s.End, scrubSourceStatus
	case s.Function == zfs.ScanNone:
		reason = reasonNoScrub
	default:
		if t, ok := m.get(pool); ok {
			at, out.LastScrubSource = t, scrubSourceRemembered
		} else {
			reason = fmt.Sprintf("the last completed scrub is not shown (%s %s) and none was observed since nexusd started",
				s.Function, s.State)
		}
	}
	if reason != "" {
		out.LastScrubSource = ""
		setReason(&out.Unavailable, "lastScrubFinishedAt", reason)
		setReason(&out.Unavailable, "lastScrubAgeDays", reason)
		return
	}
	out.LastScrubFinishedAt = &at
	age := now.Sub(at).Hours() / 24
	out.LastScrubAgeDays = &age
}

// setReason adds key's reason to an Unavailable map, making it on first use.
// Every reason passes through boundReason: a reason can carry text from a
// parser, a tool or a file that the host or a share user controls (B-SEC-1).
func setReason(m *spec.Unavailable, key, reason string) {
	if *m == nil {
		*m = spec.Unavailable{}
	}
	(*m)[key] = boundReason(reason)
}
