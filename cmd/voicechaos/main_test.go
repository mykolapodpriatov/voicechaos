package main

import (
	"os"
	"path/filepath"
	"testing"
)

const scenarioJSON = `{
  "callers": 3,
  "seed": 7,
  "stall_threshold_ms": 60,
  "profile": { "added_latency_ms": 30, "jitter_ms": 8, "loss_prob": 0.02, "bandwidth_bps": 64000 },
  "agent": { "frames_per_turn": 20, "frame_ms": 20, "payload_len": 160, "stop_latency_ms": 40, "endpoint_ms": 20 },
  "script": { "turns": [ { "at_ms": 0, "dur_ms": 60, "payload_len": 160, "barge_in": { "into_ms": 100, "dur_ms": 60, "payload_len": 160 } } ] }
}`

// devNull returns an *os.File for /dev/null and a cleanup; CLI output during
// tests is discarded.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func writeScenario(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scenario.json")
	if err := os.WriteFile(p, []byte(scenarioJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunReportHappyPath: `run --out` then `report` both succeed (exit 0).
func TestRunReportHappyPath(t *testing.T) {
	null := devNull(t)
	scenario := writeScenario(t)
	repPath := filepath.Join(t.TempDir(), "report.json")

	if code := run([]string{"run", scenario, "--out", repPath}, null, null); code != 0 {
		t.Fatalf("run exit %d, want 0", code)
	}
	if _, err := os.Stat(repPath); err != nil {
		t.Fatalf("report not written: %v", err)
	}
	if code := run([]string{"report", repPath}, null, null); code != 0 {
		t.Fatalf("report exit %d, want 0", code)
	}
}

// TestBaselineSaveThenCheckPasses: baseline save then check on the same scenario
// passes (exit 0) — determinism makes the gate stable.
func TestBaselineSaveThenCheckPasses(t *testing.T) {
	null := devNull(t)
	scenario := writeScenario(t)
	basePath := filepath.Join(t.TempDir(), "baseline.json")

	if code := run([]string{"baseline", "save", scenario, "--out", basePath}, null, null); code != 0 {
		t.Fatalf("baseline save exit %d, want 0", code)
	}
	if code := run([]string{"check", scenario, "--baseline", basePath}, null, null); code != 0 {
		t.Fatalf("check exit %d, want 0 (same scenario+seed)", code)
	}
}

// TestCheckFailsOnStrictBaseline: a check against an unrealistically strict
// baseline fails with exit 1.
func TestCheckFailsOnStrictBaseline(t *testing.T) {
	null := devNull(t)
	scenario := writeScenario(t)
	basePath := filepath.Join(t.TempDir(), "strict.json")
	strict := `{"callers":3,"seed":7,"aggregate":{"sessions":3,"time_to_stop_ms":{"count":3,"sum":3,"mean":1,"p50":1,"p95":1,"max":1},"double_talk_ms":{"count":3,"sum":3,"mean":1,"p50":1,"p95":1,"max":1},"stall_count":0,"stall_ms":0,"dropped_frames":0}}`
	if err := os.WriteFile(basePath, []byte(strict), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"check", scenario, "--baseline", basePath}, null, null); code != 1 {
		t.Fatalf("check exit %d, want 1 (regression)", code)
	}
}

// TestValidateValidScenario: `validate` on a well-formed scenario returns exit 0.
func TestValidateValidScenario(t *testing.T) {
	null := devNull(t)
	scenario := writeScenario(t)
	if code := run([]string{"validate", scenario}, null, null); code != 0 {
		t.Fatalf("validate valid exit %d, want 0", code)
	}
}

// TestValidateInvalidScenario: `validate` on an invalid scenario (zero callers)
// returns exit 1.
func TestValidateInvalidScenario(t *testing.T) {
	null := devNull(t)
	p := filepath.Join(t.TempDir(), "bad.json")
	bad := `{"callers":0,"seed":1,"profile":{},"agent":{"frames_per_turn":1},"script":{"turns":[{"at_ms":0}]}}`
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"validate", p}, null, null); code != 1 {
		t.Fatalf("validate invalid exit %d, want 1", code)
	}
}

// TestValidateMissingPath: `validate` with no scenario path returns exit 2.
func TestValidateMissingPath(t *testing.T) {
	null := devNull(t)
	if code := run([]string{"validate"}, null, null); code != 2 {
		t.Fatalf("validate missing-path exit %d, want 2", code)
	}
}

// TestUnknownSubcommand returns exit 2.
func TestUnknownSubcommand(t *testing.T) {
	null := devNull(t)
	if code := run([]string{"frobnicate"}, null, null); code != 2 {
		t.Fatalf("unknown subcommand exit %d, want 2", code)
	}
}

// TestNoArgsUsage returns exit 2.
func TestNoArgsUsage(t *testing.T) {
	null := devNull(t)
	if code := run(nil, null, null); code != 2 {
		t.Fatalf("no-args exit %d, want 2", code)
	}
}

// TestRunMissingScenario returns a non-zero exit.
func TestRunMissingScenario(t *testing.T) {
	null := devNull(t)
	if code := run([]string{"run", filepath.Join(t.TempDir(), "absent.json")}, null, null); code == 0 {
		t.Fatal("expected non-zero exit for missing scenario")
	}
}
