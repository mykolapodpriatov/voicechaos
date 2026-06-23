// Package clock provides an injectable clock abstraction so the harness can run
// either against the real wall clock (real-endpoint path) or against a single
// shared deterministic ManualClock (offline loopback path).
//
// The deterministic path is the core value of voicechaos. A ManualClock owns a
// single min-heap of pending deliveries across ALL sessions and exposes a
// global Advance() that pops the next scheduled delivery, advances the clock to
// its delivery time, and fires its callback. This event-loop scheduler replaces
// time.Sleep everywhere in pure logic: nothing blocks on real time, and the
// clock simply jumps to the next scheduled event. Combined with per-session
// seeded RNG that gives byte-identical replays of an N-session scenario.
package clock

import (
	"container/heap"
	"sync"
	"time"
)

// Clock is the minimal time abstraction the harness depends on. The real
// implementation delegates to package time; the ManualClock implementation is
// fully deterministic and never touches the wall clock.
type Clock interface {
	// NowMs returns the current time in integer milliseconds on the clock's
	// base. For the real clock this is Unix milliseconds; for ManualClock it is
	// a virtual counter starting at the configured base.
	NowMs() int64
}

// RealClock implements Clock against the wall clock. It is used only on the
// real-endpoint path; determinism guarantees do not apply when it is in use.
type RealClock struct{}

// NowMs returns the current Unix time in milliseconds.
func (RealClock) NowMs() int64 { return time.Now().UnixMilli() }

// event is one scheduled delivery on a ManualClock. The triple
// (deliverAt, seq, sessionIndex) is the single documented total order used for
// both heap ordering and delivery ordering, so replay is independent of
// goroutine scheduling.
type event struct {
	deliverAt    int64
	seq          int64
	sessionIndex int
	fn           func()
	// index is maintained by container/heap; unused beyond Push/Pop bookkeeping.
	index int
}

// eventHeap is a min-heap of events ordered by (deliverAt, seq, sessionIndex).
type eventHeap []*event

func (h eventHeap) Len() int { return len(h) }

func (h eventHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.deliverAt != b.deliverAt {
		return a.deliverAt < b.deliverAt
	}
	if a.seq != b.seq {
		return a.seq < b.seq
	}
	return a.sessionIndex < b.sessionIndex
}

func (h eventHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *eventHeap) Push(x any) {
	e := x.(*event)
	e.index = len(*h)
	*h = append(*h, e)
}

func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}

// ManualClock is a deterministic clock with an embedded event-loop scheduler.
//
// It is the single shared clock for an offline run: every session schedules its
// frame deliveries onto the same ManualClock, and a single goroutine repeatedly
// calls Advance to drain the heap. Because all pending deliveries across all
// sessions live in one heap ordered by (deliverAt, seq, sessionIndex), the
// global order in which callbacks fire is fully determined by enqueue inputs,
// not by goroutine scheduling — the basis for byte-identical replay.
//
// ManualClock is safe for concurrent use; callers Schedule from session
// goroutines while a driver goroutine calls Advance.
type ManualClock struct {
	mu   sync.Mutex
	now  int64
	heap eventHeap
}

// NewManualClock returns a ManualClock whose virtual time starts at baseMs.
func NewManualClock(baseMs int64) *ManualClock {
	mc := &ManualClock{now: baseMs}
	heap.Init(&mc.heap)
	return mc
}

// NowMs returns the current virtual time in milliseconds.
func (mc *ManualClock) NowMs() int64 {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return mc.now
}

// Schedule enqueues fn to fire when the clock reaches deliverAt. The
// (seq, sessionIndex) pair pins the firing order against other events with the
// same deliverAt. If deliverAt is in the past it is clamped to the current
// virtual time so callbacks never fire "before now"; the clock never moves
// backward.
func (mc *ManualClock) Schedule(deliverAt, seq int64, sessionIndex int, fn func()) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if deliverAt < mc.now {
		deliverAt = mc.now
	}
	heap.Push(&mc.heap, &event{
		deliverAt:    deliverAt,
		seq:          seq,
		sessionIndex: sessionIndex,
		fn:           fn,
	})
}

// Pending reports how many scheduled deliveries have not yet fired.
func (mc *ManualClock) Pending() int {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return len(mc.heap)
}

// Advance pops the single next-scheduled delivery across all sessions, advances
// the virtual clock to its delivery time, and fires its callback. It returns
// false when the heap is empty (nothing left to deliver). The callback runs
// after the clock has been advanced and the lock released, so callbacks may
// schedule further events (e.g. an agent replying to a delivered frame).
func (mc *ManualClock) Advance() bool {
	mc.mu.Lock()
	if len(mc.heap) == 0 {
		mc.mu.Unlock()
		return false
	}
	e := heap.Pop(&mc.heap).(*event)
	if e.deliverAt > mc.now {
		mc.now = e.deliverAt
	}
	mc.mu.Unlock()
	e.fn()
	return true
}

// AdvanceUntilEmpty repeatedly calls Advance until the heap is fully drained
// and returns the number of events fired. Because callbacks may enqueue new
// events, this drains the transitive closure of scheduled work.
func (mc *ManualClock) AdvanceUntilEmpty() int {
	n := 0
	for mc.Advance() {
		n++
	}
	return n
}
