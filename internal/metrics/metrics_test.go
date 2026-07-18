package metrics

import (
	"testing"

	"voicechaos/internal/audio"
	"voicechaos/internal/eventlog"
)

func agentFrame(seq int64, dur int) audio.Frame {
	return audio.Frame{Seq: seq, DurMs: dur, Kind: audio.KindAgent}
}
func speechFrame(seq int64, dur int) audio.Frame {
	return audio.Frame{Seq: seq, DurMs: dur, Kind: audio.KindSpeech}
}

// build constructs a log from a compact event list.
type ev struct {
	typ  eventlog.EventType
	ts   int64
	turn int
	f    audio.Frame
}

func buildLog(idx int, evs ...ev) eventlog.Log {
	lg := eventlog.Log{SessionIndex: idx}
	for _, e := range evs {
		lg.Append(eventlog.Event{Type: e.typ, TS: e.ts, Turn: e.turn, Frame: e.f})
	}
	return lg
}

// TestTimeToStopBasic: last agent frame before TurnEnd minus barge_in_send_ts.
func TestTimeToStopBasic(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventTurnStart, 100, 1, audio.Frame{}},
		ev{eventlog.EventRecv, 100, 1, agentFrame(0, 20)},
		ev{eventlog.EventBargeIn, 150, 1, speechFrame(0, 20)},
		ev{eventlog.EventRecv, 160, 1, agentFrame(1, 20)}, // after barge-in
		ev{eventlog.EventRecv, 200, 1, agentFrame(2, 20)}, // last after barge-in
		ev{eventlog.EventTurnEnd, 240, 1, audio.Frame{}},
	)
	m := ComputeSession(lg, 60)
	if len(m.TimeToStopMs) != 1 || m.TimeToStopMs[0] != 200-150 {
		t.Fatalf("time-to-stop %v, want [50]", m.TimeToStopMs)
	}
}

// TestTimeToStopZeroWhenNoFramesAfterBargeIn: TurnEnd arrives with no agent frame
// after the barge-in -> 0.
func TestTimeToStopZeroWhenNoFramesAfterBargeIn(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventTurnStart, 100, 1, audio.Frame{}},
		ev{eventlog.EventRecv, 120, 1, agentFrame(0, 20)},
		ev{eventlog.EventBargeIn, 150, 1, speechFrame(0, 20)},
		ev{eventlog.EventTurnEnd, 170, 1, audio.Frame{}}, // no agent frame after 150
	)
	m := ComputeSession(lg, 60)
	if len(m.TimeToStopMs) != 1 || m.TimeToStopMs[0] != 0 {
		t.Fatalf("time-to-stop %v, want [0] (agent stopped immediately)", m.TimeToStopMs)
	}
}

// TestDoubleTalkIntervalOverlap: overlap of caller speech (send-side) and agent
// audio (receive-side) intervals.
func TestDoubleTalkIntervalOverlap(t *testing.T) {
	// Caller speaks [200,260); agent received [180,220) and [220,260).
	// Overlap: caller[200,260) vs agent[180,220) = [200,220) = 20ms;
	//          caller[200,260) vs agent[220,260) = [220,260) = 40ms; total 60.
	lg := buildLog(0,
		ev{eventlog.EventSend, 200, 1, speechFrame(0, 60)},
		ev{eventlog.EventRecv, 180, 1, agentFrame(0, 40)},
		ev{eventlog.EventRecv, 220, 1, agentFrame(1, 40)},
	)
	m := ComputeSession(lg, 60)
	if m.DoubleTalkMs != 60 {
		t.Fatalf("double-talk %d, want 60", m.DoubleTalkMs)
	}
}

// TestDoubleTalkZeroWhenDisjoint: no overlap -> 0.
func TestDoubleTalkZeroWhenDisjoint(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventSend, 0, 1, speechFrame(0, 20)},
		ev{eventlog.EventRecv, 100, 1, agentFrame(0, 20)},
	)
	if m := ComputeSession(lg, 60); m.DoubleTalkMs != 0 {
		t.Fatalf("double-talk %d, want 0", m.DoubleTalkMs)
	}
}

// TestStallWithinTurn: a gap > threshold between consecutive received agent
// frames inside a turn is counted; a gap outside [TurnStart,TurnEnd) is not.
func TestStallWithinTurn(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventTurnStart, 100, 1, audio.Frame{}},
		ev{eventlog.EventRecv, 100, 1, agentFrame(0, 20)},
		ev{eventlog.EventRecv, 300, 1, agentFrame(1, 20)}, // gap 200 > 60 -> stall
		ev{eventlog.EventTurnEnd, 320, 1, audio.Frame{}},
	)
	m := ComputeSession(lg, 60)
	if m.StallCount != 1 || m.StallMs != 200 {
		t.Fatalf("stall count=%d ms=%d, want 1/200", m.StallCount, m.StallMs)
	}
}

// TestNoStallForSmallGap: gaps <= threshold are not stalls.
func TestNoStallForSmallGap(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventTurnStart, 100, 1, audio.Frame{}},
		ev{eventlog.EventRecv, 100, 1, agentFrame(0, 20)},
		ev{eventlog.EventRecv, 140, 1, agentFrame(1, 20)}, // gap 40 <= 60
		ev{eventlog.EventTurnEnd, 160, 1, audio.Frame{}},
	)
	if m := ComputeSession(lg, 60); m.StallCount != 0 {
		t.Fatalf("stall count %d, want 0", m.StallCount)
	}
}

// TestInterTurnSilenceNotStall: a large gap that straddles two turns (natural
// inter-turn silence) is not counted.
func TestInterTurnSilenceNotStall(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventTurnStart, 100, 1, audio.Frame{}},
		ev{eventlog.EventRecv, 100, 1, agentFrame(0, 20)},
		ev{eventlog.EventTurnEnd, 120, 1, audio.Frame{}},
		// long silence here (no stall: different turn)
		ev{eventlog.EventTurnStart, 1000, 2, audio.Frame{}},
		ev{eventlog.EventRecv, 1000, 2, agentFrame(1, 20)},
		ev{eventlog.EventTurnEnd, 1020, 2, audio.Frame{}},
	)
	if m := ComputeSession(lg, 60); m.StallCount != 0 {
		t.Fatalf("inter-turn silence counted as stall: count=%d", m.StallCount)
	}
}

// TestDroppedFrames counts EventDrop entries.
func TestDroppedFrames(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventDrop, 10, 0, agentFrame(0, 20)},
		ev{eventlog.EventDrop, 30, 0, agentFrame(1, 20)},
		ev{eventlog.EventRecv, 50, 1, agentFrame(2, 20)},
	)
	if m := ComputeSession(lg, 60); m.DroppedFrames != 2 {
		t.Fatalf("dropped %d, want 2", m.DroppedFrames)
	}
}

// TestReorderedFrames counts received agent frames whose Seq is below the running
// max already seen. Frames arrive (by receive TS) 0,1,3,2,4,2: seq 2 after 3 and
// seq 2 after 4 are the two out-of-order arrivals. Non-agent recvs and drops are
// ignored.
func TestReorderedFrames(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventRecv, 10, 1, agentFrame(0, 20)},
		ev{eventlog.EventRecv, 20, 1, agentFrame(1, 20)},
		ev{eventlog.EventRecv, 30, 1, agentFrame(3, 20)},
		ev{eventlog.EventRecv, 40, 1, agentFrame(2, 20)}, // 2 < max 3 -> reordered
		ev{eventlog.EventRecv, 50, 1, agentFrame(4, 20)},
		ev{eventlog.EventRecv, 60, 1, agentFrame(2, 20)},  // 2 < max 4 -> reordered
		ev{eventlog.EventRecv, 70, 1, speechFrame(5, 20)}, // non-agent: ignored
		ev{eventlog.EventDrop, 80, 1, agentFrame(6, 20)},  // drop: ignored
	)
	if m := ComputeSession(lg, 60); m.ReorderedFrames != 2 {
		t.Fatalf("reordered %d, want 2", m.ReorderedFrames)
	}
}

// TestReorderedFramesInOrderIsZero: a strictly increasing agent sequence has no
// out-of-order arrivals.
func TestReorderedFramesInOrderIsZero(t *testing.T) {
	lg := buildLog(0,
		ev{eventlog.EventRecv, 10, 1, agentFrame(0, 20)},
		ev{eventlog.EventRecv, 20, 1, agentFrame(1, 20)},
		ev{eventlog.EventRecv, 30, 1, agentFrame(2, 20)},
	)
	if m := ComputeSession(lg, 60); m.ReorderedFrames != 0 {
		t.Fatalf("reordered %d, want 0 (in order)", m.ReorderedFrames)
	}
}

// TestAggregateSumsReorderedFrames: reordered frames are summed across sessions.
func TestAggregateSumsReorderedFrames(t *testing.T) {
	a := SessionMetrics{SessionIndex: 0, ReorderedFrames: 2}
	b := SessionMetrics{SessionIndex: 1, ReorderedFrames: 3}
	if agg := ComputeAggregate([]SessionMetrics{a, b}); agg.ReorderedFrames != 5 {
		t.Fatalf("aggregate reordered %d, want 5", agg.ReorderedFrames)
	}
}

// TestStallsDeterministicAcrossTurns: stall totals are computed by iterating
// turns in sorted order (not map-range order), so a multi-turn log with stalls
// in several turns yields identical StallCount/StallMs on every computation.
// This guards the deterministic-by-construction iteration in stalls().
func TestStallsDeterministicAcrossTurns(t *testing.T) {
	// Three turns, each with a > threshold (60) intra-turn gap counted as a stall.
	lg := buildLog(0,
		ev{eventlog.EventTurnStart, 100, 1, audio.Frame{}},
		ev{eventlog.EventRecv, 100, 1, agentFrame(0, 20)},
		ev{eventlog.EventRecv, 300, 1, agentFrame(1, 20)}, // gap 200 -> stall
		ev{eventlog.EventTurnEnd, 320, 1, audio.Frame{}},
		ev{eventlog.EventTurnStart, 1000, 2, audio.Frame{}},
		ev{eventlog.EventRecv, 1000, 2, agentFrame(2, 20)},
		ev{eventlog.EventRecv, 1100, 2, agentFrame(3, 20)}, // gap 100 -> stall
		ev{eventlog.EventTurnEnd, 1120, 2, audio.Frame{}},
		ev{eventlog.EventTurnStart, 2000, 3, audio.Frame{}},
		ev{eventlog.EventRecv, 2000, 3, agentFrame(4, 20)},
		ev{eventlog.EventRecv, 2400, 3, agentFrame(5, 20)}, // gap 400 -> stall
		ev{eventlog.EventTurnEnd, 2420, 3, audio.Frame{}},
	)
	first := ComputeSession(lg, 60)
	if first.StallCount != 3 || first.StallMs != 200+100+400 {
		t.Fatalf("stalls count=%d ms=%d, want 3/700", first.StallCount, first.StallMs)
	}
	// Recompute many times; results must never vary with map iteration order.
	for i := 0; i < 50; i++ {
		m := ComputeSession(lg, 60)
		if m.StallCount != first.StallCount || m.StallMs != first.StallMs {
			t.Fatalf("non-deterministic stalls on iter %d: count=%d ms=%d, want %d/%d",
				i, m.StallCount, m.StallMs, first.StallCount, first.StallMs)
		}
	}
}

// TestNearestRankPercentiles checks the documented nearest-rank method.
func TestNearestRankPercentiles(t *testing.T) {
	// values 1..10; nearest-rank p50 = value at ceil(0.5*10)=5 -> 5;
	// p95 = ceil(0.95*10)=10 -> 10.
	vals := []int64{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	s := summarize(vals)
	if s.P50 != 5 {
		t.Errorf("p50=%d, want 5", s.P50)
	}
	if s.P95 != 10 {
		t.Errorf("p95=%d, want 10", s.P95)
	}
	if s.Max != 10 || s.Count != 10 || s.Sum != 55 || s.Mean != 5 {
		t.Errorf("summary unexpected: %+v", s)
	}
}

// TestEmptySummary: zero values for an empty input.
func TestEmptySummary(t *testing.T) {
	s := summarize(nil)
	if s.Count != 0 || s.P50 != 0 || s.P95 != 0 || s.Sum != 0 || s.Max != 0 {
		t.Fatalf("empty summary not zero: %+v", s)
	}
}

// TestAggregatePoolsTimeToStop: time-to-stop is pooled across sessions; other
// metrics summed.
func TestAggregatePoolsTimeToStop(t *testing.T) {
	a := SessionMetrics{SessionIndex: 0, TimeToStopMs: []int64{10, 30}, DoubleTalkMs: 5, StallCount: 1, StallMs: 100, DroppedFrames: 2}
	b := SessionMetrics{SessionIndex: 1, TimeToStopMs: []int64{20}, DoubleTalkMs: 15, StallCount: 0, StallMs: 0, DroppedFrames: 1}
	agg := ComputeAggregate([]SessionMetrics{a, b})
	if agg.TimeToStop.Count != 3 || agg.TimeToStop.Sum != 60 {
		t.Fatalf("pooled time-to-stop count=%d sum=%d, want 3/60", agg.TimeToStop.Count, agg.TimeToStop.Sum)
	}
	if agg.StallCount != 1 || agg.StallMs != 100 || agg.DroppedFrames != 3 {
		t.Fatalf("aggregate sums wrong: stallN=%d stallMs=%d dropped=%d", agg.StallCount, agg.StallMs, agg.DroppedFrames)
	}
	if agg.DoubleTalkMs.Sum != 20 {
		t.Fatalf("double-talk sum %d, want 20", agg.DoubleTalkMs.Sum)
	}
}

// TestComputeSessionSortsInput: results are independent of append order.
func TestComputeSessionSortsInput(t *testing.T) {
	// Same events as TestTimeToStopBasic but appended out of order.
	lg := buildLog(0,
		ev{eventlog.EventTurnEnd, 240, 1, audio.Frame{}},
		ev{eventlog.EventRecv, 200, 1, agentFrame(2, 20)},
		ev{eventlog.EventBargeIn, 150, 1, speechFrame(0, 20)},
		ev{eventlog.EventRecv, 160, 1, agentFrame(1, 20)},
		ev{eventlog.EventTurnStart, 100, 1, audio.Frame{}},
		ev{eventlog.EventRecv, 100, 1, agentFrame(0, 20)},
	)
	m := ComputeSession(lg, 60)
	if len(m.TimeToStopMs) != 1 || m.TimeToStopMs[0] != 50 {
		t.Fatalf("time-to-stop %v, want [50] regardless of order", m.TimeToStopMs)
	}
}
