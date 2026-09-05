package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"voicechaos/internal/runner"
)

// writeLiveScenario writes a one-caller live scenario with the given
// max_duration_ms and returns its path.
func writeLiveScenario(t *testing.T, maxDurationMs int) string {
	t.Helper()
	data, err := json.Marshal(liveScenario(maxDurationMs))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readReport(t *testing.T, path string) runner.Report {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report not written: %v", err)
	}
	var rep runner.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	return rep
}

// An unbounded live scenario (max_duration_ms: 0) used to run until someone
// killed it. --timeout ends it, and the report is still written and marked.
func TestTimeoutBoundsAnUnboundedLiveRun(t *testing.T) {
	null := devNull(t)
	// The stub opens a turn and never closes it, so nothing but the bound can
	// end this run.
	ts := liveWSServer(t, `{"type":"response.created"}`)
	scPath := writeLiveScenario(t, 0)
	repPath := filepath.Join(t.TempDir(), "report.json")

	start := time.Now()
	code := run([]string{"run", scPath, "--endpoint", wsURLOf(ts), "--codec", "openai-realtime",
		"--timeout", "300ms", "--out", repPath}, null, null)
	elapsed := time.Since(start)

	if code != exitTimeout {
		t.Fatalf("exit %d, want %d (timeout)", code, exitTimeout)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("run took %s; the timeout did not bound it", elapsed)
	}
	rep := readReport(t, repPath)
	if !rep.Truncated {
		t.Error("report is not marked truncated")
	}
	if rep.Callers != 1 || len(rep.Sessions) != 1 {
		t.Errorf("callers=%d sessions=%d, want 1/1; a truncated run still reports what it measured", rep.Callers, len(rep.Sessions))
	}
}

// --timeout wins over max_duration_ms on a live run, in both directions: a
// shorter timeout cuts a long scenario bound short, and a longer timeout
// replaces a short scenario bound rather than being clamped by it.
func TestTimeoutTakesPrecedenceOverMaxDurationMs(t *testing.T) {
	null := devNull(t)

	t.Run("shorter timeout cuts a long scenario bound", func(t *testing.T) {
		ts := liveWSServer(t, `{"type":"response.created"}`)
		scPath := writeLiveScenario(t, 60_000)
		repPath := filepath.Join(t.TempDir(), "report.json")

		start := time.Now()
		code := run([]string{"run", scPath, "--endpoint", wsURLOf(ts), "--codec", "openai-realtime",
			"--timeout", "300ms", "--out", repPath}, null, null)
		elapsed := time.Since(start)

		if code != exitTimeout {
			t.Fatalf("exit %d, want %d", code, exitTimeout)
		}
		if elapsed > 30*time.Second {
			t.Fatalf("run took %s; the scenario's 60s bound was applied instead of --timeout", elapsed)
		}
		if !readReport(t, repPath).Truncated {
			t.Error("report is not marked truncated")
		}
	})

	t.Run("longer timeout replaces a short scenario bound", func(t *testing.T) {
		ts := liveWSServer(t, `{"type":"response.created"}`)
		// 50ms would have ended this run almost immediately, and successfully.
		scPath := writeLiveScenario(t, 50)
		repPath := filepath.Join(t.TempDir(), "report.json")

		start := time.Now()
		code := run([]string{"run", scPath, "--endpoint", wsURLOf(ts), "--codec", "openai-realtime",
			"--timeout", "700ms", "--out", repPath}, null, null)
		elapsed := time.Since(start)

		if code != exitTimeout {
			t.Fatalf("exit %d, want %d; the scenario's 50ms bound was applied instead of --timeout", code, exitTimeout)
		}
		if elapsed < 500*time.Millisecond {
			t.Fatalf("run took %s; it ended on the scenario's 50ms bound, not the 700ms --timeout", elapsed)
		}
	})
}

// Without --timeout, max_duration_ms still ends a live run normally: exit 0
// and an untruncated report. The new flag changes nothing when it is unset.
func TestNoTimeoutKeepsMaxDurationBehavior(t *testing.T) {
	null := devNull(t)
	ts := liveWSServer(t, `{"type":"response.created"}`)
	scPath := writeLiveScenario(t, 150)
	repPath := filepath.Join(t.TempDir(), "report.json")

	if code := run([]string{"run", scPath, "--endpoint", wsURLOf(ts), "--codec", "openai-realtime",
		"--out", repPath}, null, null); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if readReport(t, repPath).Truncated {
		t.Error("a run that reached max_duration_ms was marked truncated; that bound is a normal end of run")
	}
}

// An offline run that finishes well inside its timeout is untouched by it.
func TestTimeoutDoesNotAffectAnOfflineRunThatFinishes(t *testing.T) {
	null := devNull(t)
	scPath := writeScenario(t)
	repPath := filepath.Join(t.TempDir(), "report.json")

	if code := run([]string{"run", scPath, "--timeout", "60s", "--out", repPath}, null, null); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	rep := readReport(t, repPath)
	if rep.Truncated {
		t.Error("a completed offline run was marked truncated")
	}
	if rep.Callers != 3 {
		t.Errorf("callers = %d, want 3", rep.Callers)
	}
}

// A malformed or negative --timeout is a usage error, not a silently ignored
// flag and not a crash.
func TestBadTimeoutIsUsageError(t *testing.T) {
	null := devNull(t)
	scPath := writeScenario(t)
	for _, value := range []string{"soon", "-5s", "10"} {
		t.Run(value, func(t *testing.T) {
			if code := run([]string{"run", scPath, "--timeout", value}, null, null); code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
		})
	}
}
