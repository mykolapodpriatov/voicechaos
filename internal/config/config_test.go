package config_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voicechaos/internal/config"
	"voicechaos/internal/engine"
	"voicechaos/internal/eventlog"
	"voicechaos/internal/script"
)

const validScenario = `{
  "callers": 3,
  "seed": 11,
  "stall_threshold_ms": 60,
  "profile": { "added_latency_ms": 30, "jitter_ms": 8, "loss_prob": 0.05, "bandwidth_bps": 64000 },
  "agent": { "frames_per_turn": 20, "frame_ms": 20, "payload_len": 160, "stop_latency_ms": 40, "endpoint_ms": 20 },
  "script": { "turns": [ { "at_ms": 0, "dur_ms": 60, "payload_len": 160, "barge_in": { "into_ms": 100, "dur_ms": 60, "payload_len": 160 } } ] }
}`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scenario.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadScenarioValid loads and validates a well-formed scenario.
func TestLoadScenarioValid(t *testing.T) {
	sc, err := config.LoadScenario(writeFile(t, validScenario))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sc.Callers != 3 || sc.Seed != 11 || sc.Agent.FramesPerTurn != 20 {
		t.Fatalf("parsed scenario unexpected: %+v", sc)
	}
	if sc.Script.Turns[0].BargeIn == nil || sc.Script.Turns[0].BargeIn.IntoMs != 100 {
		t.Fatalf("barge-in not parsed: %+v", sc.Script.Turns[0])
	}
}

// TestLoadScenarioUnknownFieldRejected: an unknown field is a parse error.
func TestLoadScenarioUnknownFieldRejected(t *testing.T) {
	bad := strings.Replace(validScenario, `"callers": 3,`, `"callers": 3, "bogus": 1,`, 1)
	if _, err := config.LoadScenario(writeFile(t, bad)); err == nil {
		t.Fatal("expected error on unknown field")
	}
}

// TestLoadScenarioInvalidValues: validation rejects out-of-range values.
func TestLoadScenarioInvalidValues(t *testing.T) {
	cases := map[string]string{
		"zero callers":  strings.Replace(validScenario, `"callers": 3`, `"callers": 0`, 1),
		"bad loss prob": strings.Replace(validScenario, `"loss_prob": 0.05`, `"loss_prob": 1.5`, 1),
	}
	for name, body := range cases {
		if _, err := config.LoadScenario(writeFile(t, body)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

// TestScenarioReplayDeterminism: the same loaded scenario+seed yields an
// identical merged event log across runs.
func TestScenarioReplayDeterminism(t *testing.T) {
	sc, err := config.LoadScenario(writeFile(t, validScenario))
	if err != nil {
		t.Fatal(err)
	}
	a, err := engine.Run(context.Background(), sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := engine.Run(context.Background(), sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key(a.Merged()) != key(b.Merged()) {
		t.Fatal("replay of loaded scenario not byte-identical")
	}
}

// TestWriteScenarioRoundTrips: writing then loading a scenario preserves it.
func TestWriteScenarioRoundTrips(t *testing.T) {
	sc := &script.Scenario{
		Callers: 2, Seed: 5, StallThresholdMs: 60,
		Agent:  script.AgentBehavior{FramesPerTurn: 10, FrameMs: 20, StopLatencyMs: 40},
		Script: script.Script{Turns: []script.CallerTurn{{AtMs: 0, DurMs: 40}}},
	}
	p := filepath.Join(t.TempDir(), "out.json")
	if err := config.WriteScenario(p, sc); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadScenario(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Callers != 2 || got.Agent.FramesPerTurn != 10 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

// TestLoadBudgetDefault: empty path yields the default budget.
func TestLoadBudgetDefault(t *testing.T) {
	b, err := config.LoadBudget("")
	if err != nil {
		t.Fatal(err)
	}
	if b.MaxTimeToStopRegressionPct != 10 {
		t.Fatalf("default budget pct %v, want 10", b.MaxTimeToStopRegressionPct)
	}
}

func key(merged []eventlog.MergedEvent) string {
	var b strings.Builder
	for _, me := range merged {
		b.WriteString(me.Event.Type.String())
		b.WriteByte('|')
	}
	return b.String()
}
