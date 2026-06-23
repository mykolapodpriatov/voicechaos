package transport

import (
	"context"
	"sync"

	"voicechaos/internal/audio"
	"voicechaos/internal/clock"
)

// Scheduler is the subset of clock.ManualClock the loopback transport needs:
// it schedules a delivery callback to fire at a virtual timestamp, ordered
// against all other pending deliveries by (deliverAt, seq, sessionIndex).
type Scheduler interface {
	NowMs() int64
	Schedule(deliverAt, seq int64, sessionIndex int, fn func())
}

// DelayFunc computes the virtual delivery timestamp for a frame sent at sendNow.
// It is the hook through which package impair injects its deterministic,
// per-session delivery queue. A DelayFunc may return (deliverAt, drop=true) to
// signal that the frame was dropped (lost) and must never be delivered. Because
// a DelayFunc owns mutable backlog/queue state it is called under the sending
// end's lock and must not itself block or call back into the transport.
type DelayFunc func(f audio.Frame, sendNow int64) (deliverAt int64, drop bool)

// passthroughDelay delivers every frame immediately at the current time.
func passthroughDelay(_ audio.Frame, sendNow int64) (int64, bool) { return sendNow, false }

// loopEnd is one side of a loopback pair.
//
// Delivery uses a rendezvous so the offline event loop stays deterministic
// under real goroutines: when the clock driver fires a scheduled delivery it
// calls the peer end's deliver, which hands the frame to a Recv that is parked
// on this end and then BLOCKS until that worker has consumed the frame and
// re-parked (i.e. finished any follow-up scheduling). This guarantees the
// driver never observes an "empty" scheduler while a worker is mid-reaction, so
// quiescence (clock.Pending()==0 after an Advance) is well defined and replay is
// byte-identical regardless of goroutine timing.
type loopEnd struct {
	sched        Scheduler
	sessionIndex int
	delay        DelayFunc

	mu      sync.Mutex
	cond    *sync.Cond
	pending *audio.Frame // a handed frame awaiting consumption by Recv
	parked  bool         // worker is blocked in Recv with nothing to consume
	parkSeq int64        // increments each time the worker parks
	closed  bool
	// cancelled is set when a Recv on this end observes its context cancelled. It
	// is broadcast on cond so a deliver blocked on the rendezvous (waiting for the
	// worker to park, or to consume-and-re-park) wakes and returns instead of
	// waiting forever for a re-park that a cancelled Recv will never perform. This
	// keeps the transport self-unblocking on cancellation even independent of the
	// engine closing both ends.
	cancelled bool

	peer *loopEnd
}

// Loopback returns the two ends of a deterministic in-memory transport, like
// net.Pipe but driven by the shared clock sched. The callerSide carries frames
// the synthetic caller sends to the agent; the agentSide carries frames the
// agent sends back. sessionIndex pins this pair's delivery ordering against
// other sessions sharing sched (the third component of the total order).
//
// callerDelay shapes frames the CALLER sends toward the agent (the uplink);
// agentDelay shapes frames the AGENT sends toward the caller (the downlink, the
// path the caller's receive-side metrics measure). Pass nil for an unimpaired,
// immediate-delivery direction.
func Loopback(sched Scheduler, sessionIndex int, callerDelay, agentDelay DelayFunc) (callerSide, agentSide Transport) {
	if callerDelay == nil {
		callerDelay = passthroughDelay
	}
	if agentDelay == nil {
		agentDelay = passthroughDelay
	}
	// Each end's delay applies to what THAT end sends: the caller end shapes the
	// uplink, the agent end shapes the downlink.
	caller := &loopEnd{sched: sched, sessionIndex: sessionIndex, delay: callerDelay}
	agent := &loopEnd{sched: sched, sessionIndex: sessionIndex, delay: agentDelay}
	caller.cond = sync.NewCond(&caller.mu)
	agent.cond = sync.NewCond(&agent.mu)
	caller.peer = agent
	agent.peer = caller
	return caller, agent
}

// Send shapes f through this end's outbound DelayFunc and schedules its delivery
// into the peer's inbox on the shared clock. A dropped frame is silently not
// delivered (the impair layer records the drop separately). Send does not block
// on virtual time; it only enqueues onto the scheduler.
func (e *loopEnd) Send(ctx context.Context, f audio.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	now := e.sched.NowMs()
	deliverAt, drop := e.delay(f, now)
	e.mu.Unlock()
	if drop {
		return nil
	}
	peer := e.peer
	e.sched.Schedule(deliverAt, f.Seq, e.sessionIndex, func() {
		peer.deliver(f)
	})
	return nil
}

// deliver hands one frame to a parked Recv on this end and blocks until the
// worker has consumed it and re-parked (or the end closes). It runs from the
// clock driver goroutine.
func (e *loopEnd) deliver(f audio.Frame) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Wait until the worker is parked and ready to receive (or the end closes, or
	// a Recv observed its context cancelled — in which case no worker will park).
	for !e.parked && !e.closed && !e.cancelled {
		e.cond.Wait()
	}
	if e.closed || e.cancelled {
		return
	}
	before := e.parkSeq
	frame := f
	e.pending = &frame
	e.parked = false
	e.cond.Broadcast()
	// Wait until the worker consumes the frame and re-parks (finished reacting),
	// the end is closed, or a Recv observed its context cancelled. Without the
	// cancelled guard a cancelled Recv (which returns ctx.Err() without
	// re-parking) would strand this wait forever, hanging the clock driver.
	for e.parkSeq == before && !e.closed && !e.cancelled {
		e.cond.Wait()
	}
}

// Recv blocks until a frame is handed to this end, the end is closed, or ctx is
// cancelled. Frames are returned in the scheduler's delivery order.
func (e *loopEnd) Recv(ctx context.Context) (audio.Frame, error) {
	stop := context.AfterFunc(ctx, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer stop()

	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		if e.pending != nil {
			f := *e.pending
			e.pending = nil
			return f, nil
		}
		if e.closed {
			return audio.Frame{}, ErrClosed
		}
		if err := ctx.Err(); err != nil {
			// Mark cancellation and wake any deliver blocked on the rendezvous: it
			// must not keep waiting for a park/re-park this Recv will never do.
			e.cancelled = true
			e.cond.Broadcast()
			return audio.Frame{}, err
		}
		// Park: announce readiness so a blocked deliver can proceed.
		e.parked = true
		e.parkSeq++
		e.cond.Broadcast()
		e.cond.Wait()
		e.parked = false
	}
}

// Close marks this end closed and wakes any blocked Recv/deliver. It is
// idempotent. The peer end is left open; callers close both ends.
func (e *loopEnd) Close() error {
	e.mu.Lock()
	e.closed = true
	e.parkSeq++ // unblock a deliver waiting for a re-park
	e.cond.Broadcast()
	e.mu.Unlock()
	return nil
}

// compile-time assertions.
var (
	_ Transport = (*loopEnd)(nil)
	_ Scheduler = (*clock.ManualClock)(nil)
)
