package ops

import (
	"errors"
	"slices"
	"sync"

	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
)

// lane serializes the operations that share a lane key. Its admit mutex is
// held across Data Flow steps 8 to 10 of one request, so the conflict check,
// the requested record and the record creation of two requests on one lane
// never interleave, while requests on other lanes proceed (NFR-2). Every
// other field is guarded by Manager.mu.
type lane struct {
	key    string
	policy provider.LanePolicy
	limit  int

	admit sync.Mutex

	refs   int      // Admissions holding or waiting for admit
	active []*entry // Holding a slot, in ID order
	queue  []*entry // Waiting for a slot (LaneQueue), in ID order
}

// occupied counts the operations that hold or wait for a slot.
func (l *lane) occupied() int { return len(l.active) + len(l.queue) }

// idle reports whether nothing refers to the lane, so it can be forgotten.
func (l *lane) idle() bool { return l.refs == 0 && l.occupied() == 0 }

// Admission is one request's hold on its lane, from lane resolution until
// the operation is registered on it (Data Flow steps 7 to 10). A NoLane
// admission holds nothing. Release must be called exactly once.
type Admission struct {
	m        *Manager
	lane     *lane // Nil for NoLane
	spec     provider.Lane
	released bool
}

// Admit takes the mutex of the lane l names; requests on other lanes are
// not blocked by it. NoLane takes no mutex.
func (m *Manager) Admit(l provider.Lane) *Admission {
	a := &Admission{m: m, spec: l}
	if l.Key == "" {
		return a
	}
	m.mu.Lock()
	ln, ok := m.lanes[l.Key]
	if !ok {
		policy := l.Policy
		if policy == "" {
			policy = provider.LaneQueue
		}
		// The first request to open a lane sets its policy and limit; the
		// lane keeps them while any operation occupies it.
		ln = &lane{key: l.Key, policy: policy, limit: l.Slots()}
		m.lanes[l.Key] = ln
	}
	ln.refs++
	m.mu.Unlock()
	ln.admit.Lock()
	a.lane = ln
	return a
}

// Blocker reports the operation that keeps a LaneReject lane at its limit:
// the oldest one holding it. A LaneQueue lane and NoLane never block.
func (a *Admission) Blocker() (string, bool) {
	if a.lane == nil || a.lane.policy != provider.LaneReject {
		return "", false
	}
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	if a.lane.occupied() < a.lane.limit {
		return "", false
	}
	if len(a.lane.active) > 0 {
		return a.lane.active[0].id, true
	}
	return a.lane.queue[0].id, true
}

// Release lets the next request on the lane through. Calling it again is a
// no-op.
func (a *Admission) Release() {
	if a.released {
		return
	}
	a.released = true
	if a.lane == nil {
		return
	}
	a.lane.admit.Unlock()
	a.m.mu.Lock()
	a.lane.refs--
	a.m.forgetIfIdle(a.lane)
	a.m.mu.Unlock()
}

// Lane is the lane as stored in OpRecord.Lane, so recovery never re-resolves
// it.
func (a *Admission) Lane() store.Lane {
	if a.spec.Key == "" {
		return store.Lane{}
	}
	policy := a.spec.Policy
	if policy == "" {
		policy = provider.LaneQueue
	}
	return store.Lane{Key: a.spec.Key, Policy: store.LanePolicy(policy), Limit: a.spec.Slots()}
}

var errReleased = errors.New("ops: admission already released")

// forgetIfIdle drops a lane nothing refers to; m.mu is held.
func (m *Manager) forgetIfIdle(l *lane) {
	if l.idle() && m.lanes[l.key] == l {
		delete(m.lanes, l.key)
	}
}

// enqueue registers e on its lane, or starts it at once on NoLane; m.mu is
// held.
func (m *Manager) enqueue(e *entry) {
	if e.lane == nil {
		m.start(e)
		return
	}
	e.lane.queue = append(e.lane.queue, e)
	m.pump(e.lane)
}

// pump starts queued operations, oldest first, while the lane has a free
// slot; m.mu is held. A LaneReject lane never queues beyond its limit, so
// for it this starts the new operation at once.
func (m *Manager) pump(l *lane) {
	for len(l.active) < l.limit && len(l.queue) > 0 && !m.closed {
		e := l.queue[0]
		l.queue = slices.Delete(l.queue, 0, 1)
		l.active = append(l.active, e)
		m.start(e)
	}
}

// dequeue removes a still-queued e from its lane, reporting whether it was
// queued; m.mu is held. A removed entry never takes a slot.
func (m *Manager) dequeue(e *entry) bool {
	if e.lane == nil {
		return false
	}
	i := slices.Index(e.lane.queue, e)
	if i < 0 {
		return false
	}
	e.lane.queue = slices.Delete(e.lane.queue, i, i+1)
	m.forgetIfIdle(e.lane)
	return true
}

// releaseSlot frees e's slot and starts the next queued operation; m.mu is
// held.
func (m *Manager) releaseSlot(e *entry) {
	if e.lane == nil {
		return
	}
	if i := slices.Index(e.lane.active, e); i >= 0 {
		e.lane.active = slices.Delete(e.lane.active, i, i+1)
	}
	m.pump(e.lane)
	m.forgetIfIdle(e.lane)
}
