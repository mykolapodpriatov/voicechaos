package runner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"voicechaos/internal/eventlog"
	"voicechaos/internal/impair"
	"voicechaos/internal/runner"
	"voicechaos/internal/script"
)

func scenario(callers int) *script.Scenario {
	return &script.Scenario{
		Callers:          callers,
		Seed:             7,
		StallThresholdMs: 60,
		Profile:          impair.Profile{AddedLatencyMs: 30, JitterMs: 8, ReorderProb: 0.05, LossProb: 0.03, BandwidthBps: 64000},
		Agent: script.AgentBehavior{
			FramesPerTurn: 25, FrameMs: 20, PayloadLen: 160, StopLatencyMs: 40, EndpointMs: 20,
		},
		Script: script.Script{Turns: []script.CallerTurn{
			{AtMs: 0, DurMs: 60, PayloadLen: 160, BargeIn: &script.BargeIn{IntoMs: 120, DurMs: 80, PayloadLen: 160}},
		}},
	}
}

// TestRunnerCompletesAllSessionsLeakFree: N sessions all complete and the
// ownership leak counter returns to zero (run under -race).
func TestRunnerCompletesAllSessionsLeakFree(t *testing.T) {
	rn := &runner.Runner{}
	rep, err := rn.Run(context.Background(), scenario(8))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Sessions) != 8 || rep.Aggregate.Sessions != 8 {
		t.Fatalf("want 8 sessions, got %d/%d", len(rep.Sessions), rep.Aggregate.Sessions)
	}
	if rep.LeakedGoroutines != 0 {
		t.Fatalf("ownership leak counter = %d, want 0", rep.LeakedGoroutines)
	}
}

// TestRunnerBoundedConcurrency: the peak number of concurrently live
// session-owned goroutines never exceeds the bound (2 per session).
func TestRunnerBoundedConcurrency(t *testing.T) {
	const n = 6
	rn := &runner.Runner{}
	rep, err := rn.Run(context.Background(), scenario(n))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.PeakGoroutines > int64(2*n) {
		t.Fatalf("peak goroutines %d exceeds bound %d", rep.PeakGoroutines, 2*n)
	}
	if rep.PeakGoroutines == 0 {
		t.Fatal("peak goroutines not recorded")
	}
}

// TestRunnerByteIdenticalReplay: the SAME N-session scenario run twice yields a
// byte-identical merged event log AND identical aggregate metrics — the core
// determinism guarantee, holding under concurrent goroutine scheduling.
func TestRunnerByteIdenticalReplay(t *testing.T) {
	sc := scenario(10)
	rn := &runner.Runner{}
	a, err := rn.Run(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := rn.Run(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mergedKey(a.Merged()), mergedKey(b.Merged()); got != want {
		t.Fatalf("merged event logs differ between runs:\nrun1=%s\nrun2=%s", got, want)
	}
	if fmt.Sprint(a.Aggregate) != fmt.Sprint(b.Aggregate) {
		t.Fatalf("aggregates differ:\n%+v\n%+v", a.Aggregate, b.Aggregate)
	}
}

// TestRunnerReplayStableAcrossManyRuns hammers determinism: 5 runs must all match.
func TestRunnerReplayStableAcrossManyRuns(t *testing.T) {
	sc := scenario(5)
	rn := &runner.Runner{}
	var key string
	for i := 0; i < 5; i++ {
		rep, err := rn.Run(context.Background(), sc)
		if err != nil {
			t.Fatal(err)
		}
		k := mergedKey(rep.Merged())
		if i == 0 {
			key = k
		} else if k != key {
			t.Fatalf("run %d diverged from run 0", i)
		}
	}
}

// TestRunnerCtxCancelStopsAll: a cancelled context returns promptly with no leak.
func TestRunnerCtxCancelStopsAll(t *testing.T) {
	sc := scenario(8)
	sc.Agent.FramesPerTurn = 2000 // long run so cancel lands mid-flight
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	rn := &runner.Runner{}
	rep, err := rn.Run(ctx, sc)
	// On cancel the run may return context.Canceled; either way it must drain.
	_ = err
	if rep.LeakedGoroutines != 0 {
		t.Fatalf("leak counter %d after cancel, want 0", rep.LeakedGoroutines)
	}
}

// TestRunnerMaxConcurrencyTooSmall: a bound below 2*callers is rejected clearly.
func TestRunnerMaxConcurrencyTooSmall(t *testing.T) {
	rn := &runner.Runner{MaxConcurrency: 3}
	_, err := rn.Run(context.Background(), scenario(8))
	if err == nil || !strings.Contains(err.Error(), "MaxConcurrency too small") {
		t.Fatalf("expected MaxConcurrency-too-small error, got %v", err)
	}
}

// TestRunnerValidatesScenario: an invalid scenario is rejected.
func TestRunnerValidatesScenario(t *testing.T) {
	sc := scenario(0) // zero callers
	rn := &runner.Runner{}
	if _, err := rn.Run(context.Background(), sc); err == nil {
		t.Fatal("expected validation error for zero callers")
	}
}

// mergedKey renders a merged log to a stable string for byte-comparison.
func mergedKey(merged []eventlog.MergedEvent) string {
	var b strings.Builder
	for _, me := range merged {
		fmt.Fprintf(&b, "%d|%s|%d|%d|%s|%d|%d;",
			me.SessionIndex, me.Event.Type, me.Event.TS, me.Event.Turn,
			me.Event.Frame.Kind, me.Event.Frame.Seq, me.Event.Frame.PayloadLen)
	}
	return b.String()
}
