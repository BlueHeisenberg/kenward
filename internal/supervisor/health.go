package supervisor

import (
	"errors"
	"sync"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// ErrNotEnrolled marks a configured member who has not yet claimed their invite.
//
// Such a member has no unit at all — nothing was started, nothing failed — so their
// UnitHealth carries StateUnknown, zero restarts, and this error so a renderer can say
// "not enrolled" rather than guessing. It is information, never a failure: a household
// where half the members have not claimed their codes yet is healthy, not degraded.
var ErrNotEnrolled = errors.New("supervisor: member is not enrolled; no unit to run")

// unitKey identifies one unit within a supervisor: a member's assistant, or the
// household group's when group is true.
type unitKey struct {
	member domain.MemberID
	group  bool
}

// unitRecord is one unit's condition as the tracker last heard it.
type unitRecord struct {
	state    State
	since    time.Time
	restarts int
	err      error
	// virtual marks a record with no unit behind it — a configured member who has
	// not enrolled. Virtual records never change state: there is nothing running
	// to change.
	virtual bool
}

// tracker keeps the supervisor's view of its units for Health.
//
// It is the one piece both modes share, and it is deliberately only a record-keeper:
// it holds no transport, no memory, no sandbox handle, nothing a unit could reach
// back through. Both supervisors write observations into it and Health reads a
// snapshot out, which is what lets Health be callable before Start, after Stop, and
// during any failure, without ever blocking on anything external.
type tracker struct {
	now func() time.Time

	mu    sync.Mutex
	order []unitKey
	units map[unitKey]*unitRecord
}

func newTracker(now func() time.Time) *tracker {
	if now == nil {
		now = time.Now
	}
	return &tracker{now: now, units: make(map[unitKey]*unitRecord)}
}

// add registers a unit that will run, in StateUnknown until the supervisor moves it.
func (t *tracker) add(k unitKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.units[k]; ok {
		return
	}
	t.order = append(t.order, k)
	t.units[k] = &unitRecord{state: StateUnknown, since: t.now()}
}

// addNotEnrolled registers a configured member with no unit. The record reports
// StateUnknown with ErrNotEnrolled and never moves.
func (t *tracker) addNotEnrolled(id domain.MemberID) {
	k := unitKey{member: id}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.units[k]; ok {
		return
	}
	t.order = append(t.order, k)
	t.units[k] = &unitRecord{state: StateUnknown, since: t.now(), err: ErrNotEnrolled, virtual: true}
}

// promote turns a not-enrolled record into a live one, for a member who claimed
// their invite while the supervisor was running. Their unit exists now, so the
// record starts behaving like every other unit's.
func (t *tracker) promote(id domain.MemberID) {
	k := unitKey{member: id}
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.units[k]
	if !ok {
		t.order = append(t.order, k)
		r = &unitRecord{}
		t.units[k] = r
	}
	r.virtual = false
	r.err = nil
	r.state = StateStarting
	r.since = t.now()
}

// set moves a unit to a new state. Err is left alone on purpose: it is the last
// unexpected failure and is retained across a successful restart. Virtual records
// are never moved.
func (t *tracker) set(k unitKey, s State) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.units[k]
	if !ok || r.virtual || r.state == s {
		return
	}
	r.state = s
	r.since = t.now()
}

// fail records an unexpected failure: StateFailed, the error retained, and the
// restart counter advanced, because every unexpected exit counts whether or not
// the restart that follows succeeds.
func (t *tracker) fail(k unitKey, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.units[k]
	if !ok || r.virtual {
		return
	}
	r.state = StateFailed
	r.since = t.now()
	r.err = err
	r.restarts++
}

// snapshot renders every unit, in registration order. It copies; the caller can
// hold the result as long as it likes.
func (t *tracker) snapshot() []UnitHealth {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]UnitHealth, 0, len(t.order))
	for _, k := range t.order {
		r := t.units[k]
		out = append(out, UnitHealth{
			Member:   k.member,
			Group:    k.group,
			State:    r.state,
			Since:    r.since,
			Restarts: r.restarts,
			Err:      r.err,
		})
	}
	return out
}

// stopAll marks every live unit stopped. Called once a drain has completed; the
// not-enrolled records keep saying not enrolled.
func (t *tracker) stopAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.units {
		if r.virtual || r.state == StateStopped {
			continue
		}
		r.state = StateStopped
		r.since = t.now()
	}
}

// backoff is a doubling delay with a ceiling, one per unit, so one member
// crash-looping burns their own schedule and nobody else's.
type backoff struct {
	base, max, cur time.Duration
}

func newBackoff(base, max time.Duration) *backoff {
	return &backoff{base: base, max: max}
}

// next returns the delay to wait before the coming restart attempt and doubles
// the one after it.
func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.base
	}
	d := b.cur
	b.cur *= 2
	if b.cur > b.max {
		b.cur = b.max
	}
	return d
}

// reset returns the schedule to its base, for a unit that has stayed up long
// enough to be believed.
func (b *backoff) reset() { b.cur = 0 }
