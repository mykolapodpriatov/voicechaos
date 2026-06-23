package session_test

import (
	"context"
	"testing"

	"voicechaos/internal/engine"
	"voicechaos/internal/impair"
	"voicechaos/internal/metrics"
	"voicechaos/internal/script"
)

// runOne runs a single-caller scenario through the deterministic engine and
// returns the one session's metrics.
func runOne(t *testing.T, sc *script.Scenario) metrics.SessionMetrics {
	t.Helper()
	sc.Callers = 1
	res, err := engine.Run(context.Background(), sc, nil)
	if err != nil {
		t.Fatalf("engine run: %v", err)
	}
	if len(res.Logs) != 1 {
		t.Fatalf("want 1 log, got %d", len(res.Logs))
	}
	return metrics.ComputeSession(res.Logs[0], sc.StallThreshold())
}

func baseScenario() *script.Scenario {
	return &script.Scenario{
		Callers:          1,
		Seed:             1,
		StallThresholdMs: 60,
		Profile:          impair.Profile{}, // clean transport: deterministic exact numbers
		Agent: script.AgentBehavior{
			FramesPerTurn: 20, FrameMs: 20, PayloadLen: 160, StopLatencyMs: 40, EndpointMs: 20,
		},
		Script: script.Script{Turns: []script.CallerTurn{
			{AtMs: 0, DurMs: 40, PayloadLen: 160, BargeIn: &script.BargeIn{IntoMs: 100, DurMs: 40, PayloadLen: 160}},
		}},
	}
}

// TestScriptedBargeInYieldsExpectedTimeToStop: with a clean transport, the
// time-to-stop equals StopLatencyMs rounded to the frame cadence.
func TestScriptedBargeInYieldsExpectedTimeToStop(t *testing.T) {
	m := runOne(t, baseScenario())
	if len(m.TimeToStopMs) != 1 {
		t.Fatalf("want exactly one interrupted turn, got %v", m.TimeToStopMs)
	}
	// Clean transport: agent stops StopLatencyMs (40) after the barge-in; the last
	// delivered frame lands within one frame period of the 40ms deadline.
	tts := m.TimeToStopMs[0]
	if tts < 40 || tts > 60 {
		t.Fatalf("time-to-stop %dms, want within [40,60]", tts)
	}
}

// TestIgnoreBargeInProducesDoubleTalk: an agent that ignores the barge-in keeps
// talking over the caller -> measurable double-talk.
func TestIgnoreBargeInProducesDoubleTalk(t *testing.T) {
	sc := baseScenario()
	sc.Agent.IgnoreBargeIn = true
	// Make the barge-in longer so the overlap is unambiguous.
	sc.Script.Turns[0].BargeIn.DurMs = 200
	m := runOne(t, sc)
	if m.DoubleTalkMs <= 0 {
		t.Fatalf("expected double-talk with ignored barge-in, got %d", m.DoubleTalkMs)
	}
}

// TestStallingAgentRecordsStall: an agent scripted to stall mid-turn produces a
// recorded stall.
func TestStallingAgentRecordsStall(t *testing.T) {
	sc := baseScenario()
	sc.Script.Turns[0].BargeIn = nil // no barge-in; let the full turn play with a stall
	sc.Agent.StallBeforeFrame = 5
	sc.Agent.StallMs = 200 // well above the 60ms threshold
	m := runOne(t, sc)
	if m.StallCount != 1 {
		t.Fatalf("expected 1 stall, got count=%d ms=%d", m.StallCount, m.StallMs)
	}
	if m.StallMs < 200 {
		t.Fatalf("stall ms %d, want >= 200", m.StallMs)
	}
}

// TestNoBargeInNoTimeToStop: a turn with no barge-in records no time-to-stop.
func TestNoBargeInNoTimeToStop(t *testing.T) {
	sc := baseScenario()
	sc.Script.Turns[0].BargeIn = nil
	m := runOne(t, sc)
	if len(m.TimeToStopMs) != 0 {
		t.Fatalf("time-to-stop %v, want empty", m.TimeToStopMs)
	}
}
