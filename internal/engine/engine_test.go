package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"voicechaos/internal/eventlog"
	"voicechaos/internal/impair"
	"voicechaos/internal/script"
)

func scenario(callers int) *script.Scenario {
	return &script.Scenario{
		Callers:          callers,
		Seed:             3,
		StallThresholdMs: 60,
		Profile:          impair.Profile{AddedLatencyMs: 30, JitterMs: 6, LossProb: 0.02, BandwidthBps: 64000},
		Agent:            script.AgentBehavior{FramesPerTurn: 20, FrameMs: 20, PayloadLen: 160, StopLatencyMs: 40, EndpointMs: 20},
		Script: script.Script{Turns: []script.CallerTurn{
			{AtMs: 0, DurMs: 60, PayloadLen: 160, BargeIn: &script.BargeIn{IntoMs: 120, DurMs: 60, PayloadLen: 160}},
		}},
	}
}

// TestEngineProducesLogsAndDrains: a run returns one log per session and reaches
// quiescence (no leaked goroutines via the instrumentation).
func TestEngineProducesLogsAndDrains(t *testing.T) {
	var live, peak int64
	sc := scenario(4)
	inst := &Instrumentation{Live: &live, Peak: &peak, Sem: make(chan struct{}, 2*sc.Callers)}
	res, err := Run(context.Background(), sc, inst)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Logs) != 4 {
		t.Fatalf("logs %d, want 4", len(res.Logs))
	}
	if atomic.LoadInt64(&live) != 0 {
		t.Fatalf("live goroutines %d after run, want 0", live)
	}
	if peak == 0 || peak > int64(2*sc.Callers) {
		t.Fatalf("peak %d out of (0, %d]", peak, 2*sc.Callers)
	}
	// Each session log should contain at least one turn boundary.
	for _, lg := range res.Logs {
		if !hasType(lg, eventlog.EventTurnStart) {
			t.Fatalf("session %d log missing TurnStart", lg.SessionIndex)
		}
	}
}

// TestEngineByteIdenticalReplay: the merged log is identical across two runs.
func TestEngineByteIdenticalReplay(t *testing.T) {
	sc := scenario(6)
	a, err := Run(context.Background(), sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(context.Background(), sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mk(a.Merged()) != mk(b.Merged()) {
		t.Fatal("engine replay not byte-identical")
	}
}

// TestEnginePerSessionRNGIsolationViaDrops: each session's drop trace is its own;
// summing per-session EventDrop counts is stable across runs.
func TestEnginePerSessionRNGIsolationViaDrops(t *testing.T) {
	sc := scenario(8)
	sc.Profile.LossProb = 0.3 // many drops
	r1, _ := Run(context.Background(), sc, nil)
	r2, _ := Run(context.Background(), sc, nil)
	for i := range r1.Logs {
		if dropCount(r1.Logs[i]) != dropCount(r2.Logs[i]) {
			t.Fatalf("session %d drop count not reproducible: %d vs %d", i, dropCount(r1.Logs[i]), dropCount(r2.Logs[i]))
		}
	}
}

// TestEngineCancelMidRunTearsDownNoHang is the regression guard for the
// loopback rendezvous deadlock: cancelling the context while frames are being
// delivered must let Run() (and its clock driver) return within a bounded time
// and leave zero live session goroutines. Before the fix, a cancel landing
// while the clock driver was parked inside a peer end's deliver() — waiting for
// a Recv that then returned ctx.Err() without re-parking — hung the driver
// forever, so Run() never reached its Close() calls or wg.Wait().
//
// The mid-delivery window is tiny (a large run drains in a few ms on the virtual
// clock), so a single cancel rarely lands inside a parked deliver(). We instead
// SWEEP the cancel delay across the whole run window in microsecond steps over
// many attempts, so at least one cancel reliably interrupts an active
// rendezvous. Each attempt has a HARD per-run timeout that turns the old
// permanent hang into a failure rather than a stuck suite; -race covers the
// teardown. With the fix every attempt tears down cleanly with no leaked
// goroutine.
func TestEngineCancelMidRunTearsDownNoHang(t *testing.T) {
	const attempts = 300
	for a := 0; a < attempts; a++ {
		sc := scenario(8)
		// Long turns => many in-flight deliveries, so the run spends real time in
		// rendezvous waits and the swept cancel can land inside one.
		sc.Agent.FramesPerTurn = 400

		var live, peak int64
		inst := &Instrumentation{Live: &live, Peak: &peak, Sem: make(chan struct{}, 2*sc.Callers)}

		ctx, cancel := context.WithCancel(context.Background())
		// Sweep the cancel offset across the run window (~0..5ms) in 10µs steps.
		delay := time.Duration(a%500) * 10 * time.Microsecond
		go func() {
			time.Sleep(delay)
			cancel()
		}()

		done := make(chan struct{})
		go func() { _, _ = Run(ctx, sc, inst); close(done) }()

		select {
		case <-done:
			// Returned within the bound: the run tore down on cancel.
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatalf("attempt %d (cancel delay %v): engine.Run did not return within 10s after cancel (clock-driver deadlock)", a, delay)
		}
		cancel()

		if got := atomic.LoadInt64(&live); got != 0 {
			t.Fatalf("attempt %d: %d live session goroutines after cancelled run, want 0 (leak)", a, got)
		}
	}
}

func hasType(lg eventlog.Log, ty eventlog.EventType) bool {
	for _, e := range lg.Events {
		if e.Type == ty {
			return true
		}
	}
	return false
}

func dropCount(lg eventlog.Log) int {
	n := 0
	for _, e := range lg.Events {
		if e.Type == eventlog.EventDrop {
			n++
		}
	}
	return n
}

func mk(merged []eventlog.MergedEvent) string {
	var b strings.Builder
	for _, me := range merged {
		fmt.Fprintf(&b, "%d|%s|%d|%d|%s|%d;", me.SessionIndex, me.Event.Type, me.Event.TS, me.Event.Turn, me.Event.Frame.Kind, me.Event.Frame.Seq)
	}
	return b.String()
}
