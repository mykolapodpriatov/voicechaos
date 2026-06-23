package script

import (
	"testing"

	"voicechaos/internal/audio"
	"voicechaos/internal/impair"
)

func validScenario() *Scenario {
	return &Scenario{
		Callers: 2,
		Seed:    1,
		Profile: impair.Profile{AddedLatencyMs: 20, JitterMs: 5, LossProb: 0.1, ReorderProb: 0.1, BandwidthBps: 64000},
		Agent:   AgentBehavior{FramesPerTurn: 10, FrameMs: 20, StopLatencyMs: 40},
		Script:  Script{Turns: []CallerTurn{{AtMs: 0, DurMs: 40, BargeIn: &BargeIn{IntoMs: 100, DurMs: 40}}}},
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	if err := validScenario().Validate(); err != nil {
		t.Fatalf("valid scenario rejected: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Scenario){
		"zero callers":     func(s *Scenario) { s.Callers = 0 },
		"no turns":         func(s *Scenario) { s.Script.Turns = nil },
		"zero frames":      func(s *Scenario) { s.Agent.FramesPerTurn = 0 },
		"loss > 1":         func(s *Scenario) { s.Profile.LossProb = 1.5 },
		"reorder negative": func(s *Scenario) { s.Profile.ReorderProb = -0.1 },
		"latency negative": func(s *Scenario) { s.Profile.AddedLatencyMs = -1 },
		"negative into":    func(s *Scenario) { s.Script.Turns[0].BargeIn.IntoMs = -5 },
	}
	for name, mutate := range cases {
		s := validScenario()
		mutate(s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestStallThresholdDefault(t *testing.T) {
	s := validScenario()
	if got := s.StallThreshold(); got != DefaultStallThresholdMs {
		t.Errorf("default stall threshold %d, want %d", got, DefaultStallThresholdMs)
	}
	s.StallThresholdMs = 99
	if got := s.StallThreshold(); got != 99 {
		t.Errorf("stall threshold %d, want 99", got)
	}
}

func TestFakeConfigMapping(t *testing.T) {
	b := AgentBehavior{FramesPerTurn: 7, FrameMs: 30, PayloadLen: 100, StopLatencyMs: 50, IgnoreBargeIn: true, StallBeforeFrame: 3, StallMs: 60, EndpointMs: 40}
	cfg := b.FakeConfig()
	if cfg.FramesPerTurn != 7 || cfg.FrameMs != 30 || cfg.PayloadLen != 100 || cfg.StopLatencyMs != 50 {
		t.Fatalf("mapping mismatch: %+v", cfg)
	}
	if !cfg.IgnoreBargeIn || cfg.StallBeforeFrame != 3 || cfg.StallMs != 60 || cfg.EndpointMs != 40 {
		t.Fatalf("mapping mismatch (flags): %+v", cfg)
	}
}

func TestDefaultStallThresholdValue(t *testing.T) {
	if DefaultStallThresholdMs != 3*audio.DefaultFrameMs {
		t.Fatalf("DefaultStallThresholdMs=%d, want %d", DefaultStallThresholdMs, 3*audio.DefaultFrameMs)
	}
}
