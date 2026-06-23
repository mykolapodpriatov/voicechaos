// Package engine assembles and drives the deterministic offline pipeline:
// a shared ManualClock, per-session loopback transports with a seeded
// downlink impairment queue, a FakeAgent on the agent end, and a Session on
// the caller end. It primes all sessions, launches their read pumps, drives the
// single global event loop to quiescence, and returns the per-session event
// logs (with EventDrop entries appended for impair-dropped agent frames).
//
// The impairment profile shapes the DOWNLINK (agent -> caller), the path whose
// frames the caller's metrics observe; the uplink (caller -> speech reaching
// the agent) is clean so a barge-in always reaches the agent and time-to-stop
// measures the agent's stop behavior under a degraded downlink. Each session's
// downlink queue is seeded seed+sessionIndex, so re-running session k alone
// reproduces its exact loss/jitter/reorder trace.
//
// Because every delivery rendezvouses with its consuming Recv (see package
// transport) and the clock orders all pending deliveries by
// (deliverAt, seq, sessionIndex), a run is byte-identical across replays of the
// same scenario+seed regardless of goroutine scheduling.
package engine

import (
	"context"
	"sync"
	"sync/atomic"

	"voicechaos/internal/agentproto"
	"voicechaos/internal/clock"
	"voicechaos/internal/eventlog"
	"voicechaos/internal/impair"
	"voicechaos/internal/script"
	"voicechaos/internal/session"
	"voicechaos/internal/transport"
)

// Instrumentation lets the runner observe the run's goroutine lifecycle for its
// ownership-based leak assertion and bounded-concurrency check. All fields are
// optional (nil-safe).
type Instrumentation struct {
	// Live is incremented when a session-owned goroutine starts and decremented
	// when it exits. The runner asserts it returns to zero after the run.
	Live *int64
	// Peak records the maximum observed value of Live (the peak number of
	// concurrently live session-owned goroutines), for the bounded-concurrency
	// assertion.
	Peak *int64
	// Sem, if non-nil, bounds how many session goroutines may be live at once;
	// the engine acquires before launching and releases on exit. For the shared
	// clock path it must admit all session goroutines (see Run).
	Sem chan struct{}
}

// BaseClockMs is the virtual time at which every offline run's clock starts. A
// fixed, non-zero base keeps timestamps positive and identical across runs.
const BaseClockMs = 1_000_000

// rig bundles one session's wiring.
type rig struct {
	sess     *session.Session
	agent    *agentproto.FakeAgent
	caller   transport.Transport
	agentEnd transport.Transport
	downlink *impair.Queue
}

// Result is the output of a run: one event log per session, in session-index
// order.
type Result struct {
	Logs []eventlog.Log
}

// Merged returns all sessions' events combined into one cross-session log in the
// canonical total order. Two runs of the same scenario+seed produce a
// byte-identical Merged slice.
func (r Result) Merged() []eventlog.MergedEvent {
	return eventlog.Merge(r.Logs)
}

// Run builds N = sc.Callers sessions on one shared clock, drives the pipeline to
// completion, and returns each session's event log. The optional Instrumentation
// observes the goroutine lifecycle. ctx cancels the run (e.g. on timeout); a
// clean run drains naturally before ctx matters.
//
// Because every session shares the one clock whose driver hands a frame to its
// consumer and waits for that consumer to react before advancing, the session
// read pumps are effectively serialized and must all be co-resident; an
// Instrumentation.Sem must therefore admit at least 2*sc.Callers tokens.
func Run(ctx context.Context, sc *script.Scenario, inst *Instrumentation) (Result, error) {
	mc := clock.NewManualClock(BaseClockMs)
	rigs := make([]rig, sc.Callers)
	for i := 0; i < sc.Callers; i++ {
		dq := impair.NewQueue(sc.Profile, sc.Seed, i)
		caller, agentEnd := transport.Loopback(mc, i, nil, dq.Delay)
		ag := agentproto.NewFakeAgent(sc.Agent.FakeConfig(), mc, agentEnd, i)
		se := session.New(i, mc, caller, sc)
		rigs[i] = rig{sess: se, agent: ag, caller: caller, agentEnd: agentEnd, downlink: dq}
	}

	// Prime all sessions before advancing the clock so the heap is non-empty.
	for i := range rigs {
		rigs[i].sess.Prime(ctx)
	}

	// Launch read pumps: each session owns exactly two goroutines (agent driver
	// + session read pump), both scoped to ctx and counted for leak detection.
	var wg sync.WaitGroup
	track := func(fn func()) {
		if inst != nil && inst.Sem != nil {
			inst.Sem <- struct{}{}
		}
		wg.Add(1)
		if inst != nil && inst.Live != nil {
			n := atomic.AddInt64(inst.Live, 1)
			if inst.Peak != nil {
				for {
					p := atomic.LoadInt64(inst.Peak)
					if n <= p || atomic.CompareAndSwapInt64(inst.Peak, p, n) {
						break
					}
				}
			}
		}
		go func() {
			defer wg.Done()
			defer func() {
				if inst != nil && inst.Live != nil {
					atomic.AddInt64(inst.Live, -1)
				}
				if inst != nil && inst.Sem != nil {
					<-inst.Sem
				}
			}()
			fn()
		}()
	}
	for i := range rigs {
		r := rigs[i]
		track(func() { _ = r.agent.Run(ctx) })
		track(func() { _ = r.sess.Serve(ctx) })
	}

	// On cancellation, close every transport end promptly. This is what breaks
	// the delivery rendezvous: the clock driver may be parked inside a peer end's
	// deliver() waiting for its Recv to consume a frame and re-park; if that Recv
	// instead returns ctx.Err() (cancellation), the deliver would otherwise wait
	// forever. Closing both ends trips deliver()'s !closed guard so it returns,
	// the driver unblocks, drive() observes ctx.Err() and stops, and every read
	// pump unblocks too — so the run always tears down on cancel with no goroutine
	// left blocked. A clean run drains naturally before ctx ever fires; this
	// watcher then exits on the closeOnce signal below.
	closeAllEnds := func() {
		for i := range rigs {
			_ = rigs[i].caller.Close()
			_ = rigs[i].agentEnd.Close()
		}
	}
	closeOnce := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			closeAllEnds()
		case <-closeOnce:
		}
	}()

	// Drive the single global event loop to quiescence. Thanks to the delivery
	// rendezvous, an empty scheduler after an Advance means no worker is
	// mid-reaction, so the conversation is complete.
	drive(ctx, mc)

	// Close both ends of every session to unblock the read pumps, stop the
	// cancellation watcher, then wait. Close is idempotent, so a double close
	// (driver path here + watcher path on cancel) is safe.
	closeAllEnds()
	close(closeOnce)
	<-watchDone
	wg.Wait()

	// Collect logs and fold in impair drops (downlink agent frames the caller
	// never received) as EventDrop entries.
	logs := make([]eventlog.Log, sc.Callers)
	for i := range rigs {
		log := rigs[i].sess.Log()
		for _, df := range rigs[i].downlink.Dropped() {
			log.Append(eventlog.Event{Type: eventlog.EventDrop, TS: df.TS, Frame: df})
		}
		log.Sort()
		logs[i] = log
	}
	return Result{Logs: logs}, ctx.Err()
}

// drive advances the shared clock until the scheduler is empty or ctx is
// cancelled. With the delivery rendezvous in place, an empty scheduler is a true
// quiescence condition.
func drive(ctx context.Context, mc *clock.ManualClock) {
	for {
		if ctx.Err() != nil {
			return
		}
		if !mc.Advance() {
			return
		}
	}
}
