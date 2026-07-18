package agentproto_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"voicechaos/internal/agentproto"
	"voicechaos/internal/audio"
	"voicechaos/internal/clock"
	"voicechaos/internal/transport"
)

// collect drives the agent against a caller end, injecting the given caller
// speech sends (at the given virtual times) and returning the frames the caller
// receives.
func collect(t *testing.T, cfg agentproto.FakeConfig, sends []struct {
	at  int64
	seq int64
}) []audio.Frame {
	t.Helper()
	mc := clock.NewManualClock(1_000_000)
	caller, agentEnd := transport.Loopback(mc, 0, nil, nil)
	ag := agentproto.NewFakeAgent(cfg, mc, agentEnd, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ag.Run(ctx) }()

	var got []audio.Frame
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := caller.Recv(ctx)
			if err != nil {
				return
			}
			got = append(got, f)
		}
	}()

	for _, s := range sends {
		s := s
		mc.Schedule(s.at, s.seq, 0, func() {
			_ = caller.Send(ctx, audio.Frame{Seq: s.seq, TS: s.at, DurMs: cfg.FrameMs, Kind: audio.KindSpeech, PayloadLen: 100})
		})
	}
	// Drive to quiescence.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !mc.Advance() {
			break
		}
	}
	_ = caller.Close()
	_ = agentEnd.Close()
	<-done
	return got
}

func countKind(frames []audio.Frame, k audio.FrameKind) int {
	n := 0
	for _, f := range frames {
		if f.Kind == k {
			n++
		}
	}
	return n
}

// TestFullTurnEmitsAllFrames: an uninterrupted prompt yields TurnStart + N agent
// frames + TurnEnd.
func TestFullTurnEmitsAllFrames(t *testing.T) {
	cfg := agentproto.FakeConfig{FramesPerTurn: 8, FrameMs: 20, PayloadLen: 100, StopLatencyMs: 40, EndpointMs: 20}
	// One prompt frame at the base time.
	got := collect(t, cfg, []struct{ at, seq int64 }{{1_000_000, 0}})
	if countKind(got, audio.KindTurnStart) != 1 {
		t.Fatalf("turn_start count %d, want 1", countKind(got, audio.KindTurnStart))
	}
	if countKind(got, audio.KindTurnEnd) != 1 {
		t.Fatalf("turn_end count %d, want 1", countKind(got, audio.KindTurnEnd))
	}
	if n := countKind(got, audio.KindAgent); n != 8 {
		t.Fatalf("agent frame count %d, want 8", n)
	}
}

// TestMultiFramePromptNotSelfBargeIn: a multi-frame prompt (endpointing) starts a
// single turn and is not treated as a barge-in, so all frames are emitted.
func TestMultiFramePromptNotSelfBargeIn(t *testing.T) {
	cfg := agentproto.FakeConfig{FramesPerTurn: 10, FrameMs: 20, PayloadLen: 100, StopLatencyMs: 40, EndpointMs: 20}
	// Three prompt frames 20ms apart.
	got := collect(t, cfg, []struct{ at, seq int64 }{
		{1_000_000, 0}, {1_000_020, 1}, {1_000_040, 2},
	})
	if countKind(got, audio.KindTurnStart) != 1 {
		t.Fatalf("expected exactly one turn, got %d turn_starts", countKind(got, audio.KindTurnStart))
	}
	if n := countKind(got, audio.KindAgent); n != 10 {
		t.Fatalf("agent frames %d, want 10 (prompt must not self-barge-in)", n)
	}
}

// TestBargeInStopsWithinLatency: a barge-in mid-turn stops the agent; fewer than
// the full frame count are emitted.
func TestBargeInStopsWithinLatency(t *testing.T) {
	cfg := agentproto.FakeConfig{FramesPerTurn: 50, FrameMs: 20, PayloadLen: 100, StopLatencyMs: 40, EndpointMs: 20}
	// Prompt at base; barge-in well into the turn (the agent starts ~1_000_020).
	got := collect(t, cfg, []struct{ at, seq int64 }{
		{1_000_000, 0}, // prompt
		{1_000_200, 1}, // barge-in (agent talking by now)
	})
	agentFrames := countKind(got, audio.KindAgent)
	if agentFrames >= 50 {
		t.Fatalf("agent emitted %d frames; barge-in should have stopped it early", agentFrames)
	}
	if agentFrames == 0 {
		t.Fatal("agent emitted no frames before the barge-in")
	}
	if countKind(got, audio.KindTurnEnd) != 1 {
		t.Fatal("expected a TurnEnd after the interrupted turn")
	}
}

// TestIgnoreBargeInKeepsTalking: with IgnoreBargeIn, the agent emits the full
// turn despite a barge-in.
func TestIgnoreBargeInKeepsTalking(t *testing.T) {
	cfg := agentproto.FakeConfig{FramesPerTurn: 20, FrameMs: 20, PayloadLen: 100, StopLatencyMs: 40, EndpointMs: 20, IgnoreBargeIn: true}
	got := collect(t, cfg, []struct{ at, seq int64 }{
		{1_000_000, 0},
		{1_000_200, 1}, // barge-in ignored
	})
	if n := countKind(got, audio.KindAgent); n != 20 {
		t.Fatalf("agent frames %d, want 20 (barge-in ignored)", n)
	}
}

// recordingScheduler wraps a real ManualClock, delegating NowMs/Schedule so the
// agent runs exactly as in production, while recording every (deliverAt, seq)
// pair the agent schedules. It lets a test inspect the heap KEYS the agent
// emits, independent of how container/heap happens to break ties — which is the
// only way to observe the determinism bug, since a single-threaded Advance loop
// pops equal keys reproducibly run-to-run regardless of the bug.
type recordingScheduler struct {
	mc   *clock.ManualClock
	mu   sync.Mutex
	keys []schedKey
}

type schedKey struct {
	deliverAt int64
	seq       int64
}

func (r *recordingScheduler) NowMs() int64 { return r.mc.NowMs() }

func (r *recordingScheduler) Schedule(deliverAt, seq int64, sessionIndex int, fn func()) {
	r.mu.Lock()
	r.keys = append(r.keys, schedKey{deliverAt: deliverAt, seq: seq})
	r.mu.Unlock()
	r.mc.Schedule(deliverAt, seq, sessionIndex, fn)
}

// TestEqualDeliverAtSchedulingSeqStrictTotalOrder is the regression guard for the
// determinism fix. When two agent frames within a turn are scheduled for the
// SAME deliverAt, the clock's (deliverAt, seq, sessionIndex) key must already be
// a STRICT total order — i.e. the seqs must differ — so the firing order does
// not fall back to heap-arbitrary tie-breaking (which differs across heaps with
// different internal state and across implementations, breaking byte-identical
// replay). Before the fix, every per-turn timer shared one seq value, so frames
// at equal deliverAt collided on the full key. We drive a real turn through a
// recording scheduler and assert no two scheduled callbacks share a (deliverAt,
// seq) pair. This fails on the old shared-seq scheduling and passes on the fix.
func TestEqualDeliverAtSchedulingSeqStrictTotalOrder(t *testing.T) {
	cfg := agentproto.FakeConfig{FramesPerTurn: 8, FrameMs: 20, PayloadLen: 100, StopLatencyMs: 40, EndpointMs: 20}
	mc := clock.NewManualClock(1_000_000)
	rec := &recordingScheduler{mc: mc}
	// A downlink that snaps every deliverAt down to a 100ms bucket forces several
	// agent frames (sent 20ms apart) to share a deliverAt on the wire; but the
	// agent's OWN scheduling seqs (recorded here) are what must already be unique.
	bucket := func(_ audio.Frame, sendNow int64) (int64, bool) { return sendNow - (sendNow % 100), false }
	caller, agentEnd := transport.Loopback(mc, 0, nil, bucket)
	ag := agentproto.NewFakeAgent(cfg, rec, agentEnd, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ag.Run(ctx) }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := caller.Recv(ctx); err != nil {
				return
			}
		}
	}()
	mc.Schedule(1_000_000, 0, 0, func() {
		_ = caller.Send(ctx, audio.Frame{Seq: 0, TS: 1_000_000, DurMs: 20, Kind: audio.KindSpeech, PayloadLen: 100})
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !mc.Advance() {
			break
		}
	}
	_ = caller.Close()
	_ = agentEnd.Close()
	<-done

	rec.mu.Lock()
	keys := append([]schedKey(nil), rec.keys...)
	rec.mu.Unlock()

	// Every scheduled callback must own a unique (deliverAt, seq) so the clock's
	// total order never depends on heap tie-breaking.
	seen := map[schedKey]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("duplicate scheduling key %+v: two callbacks tie fully on (deliverAt, seq) — non-deterministic order", k)
		}
		seen[k] = true
	}

	// And confirm the scenario actually scheduled >=2 callbacks at one deliverAt
	// (otherwise the tie path is untested).
	perDeliverAt := map[int64]int{}
	for _, k := range keys {
		perDeliverAt[k.deliverAt]++
	}
	tied := false
	for _, n := range perDeliverAt {
		if n >= 2 {
			tied = true
		}
	}
	if !tied {
		t.Fatal("no two callbacks shared a deliverAt; equal-deliverAt tie path not exercised")
	}
}

// TestEqualDeliverAtReplayByteIdentical confirms the same scenario+seed replays
// byte-identically when agent frames share a deliverAt: it runs the equal-
// deliverAt scenario twice and compares the full received-frame sequences.
func TestEqualDeliverAtReplayByteIdentical(t *testing.T) {
	cfg := agentproto.FakeConfig{FramesPerTurn: 8, FrameMs: 20, PayloadLen: 100, StopLatencyMs: 40, EndpointMs: 20}
	bucket := func(_ audio.Frame, sendNow int64) (int64, bool) { return sendNow - (sendNow % 100), false }
	run := func() []audio.Frame {
		mc := clock.NewManualClock(1_000_000)
		caller, agentEnd := transport.Loopback(mc, 0, nil, bucket)
		ag := agentproto.NewFakeAgent(cfg, mc, agentEnd, 0)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = ag.Run(ctx) }()
		var got []audio.Frame
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				f, err := caller.Recv(ctx)
				if err != nil {
					return
				}
				got = append(got, f)
			}
		}()
		mc.Schedule(1_000_000, 0, 0, func() {
			_ = caller.Send(ctx, audio.Frame{Seq: 0, TS: 1_000_000, DurMs: 20, Kind: audio.KindSpeech, PayloadLen: 100})
		})
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !mc.Advance() {
				break
			}
		}
		_ = caller.Close()
		_ = agentEnd.Close()
		<-done
		return got
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("frame counts differ across replays: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("replay diverged at frame %d: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestDeterministicAgentOutput: identical inputs produce identical frame streams.
func TestDeterministicAgentOutput(t *testing.T) {
	cfg := agentproto.FakeConfig{FramesPerTurn: 12, FrameMs: 20, PayloadLen: 100, StopLatencyMs: 40, EndpointMs: 20}
	sends := []struct{ at, seq int64 }{{1_000_000, 0}, {1_000_200, 1}}
	a := collect(t, cfg, sends)
	b := collect(t, cfg, sends)
	if len(a) != len(b) {
		t.Fatalf("frame counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("frame %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}
