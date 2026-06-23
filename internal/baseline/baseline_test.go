package baseline

import (
	"path/filepath"
	"testing"

	"voicechaos/internal/metrics"
)

func sampleAggregate() metrics.Aggregate {
	return metrics.Aggregate{
		Sessions:      4,
		TimeToStop:    metrics.Summary{Count: 4, Sum: 200, Mean: 50, P50: 50, P95: 60, Max: 70},
		DoubleTalkMs:  metrics.Summary{Count: 4, Sum: 100, Mean: 25, P50: 25, P95: 30, Max: 40},
		StallCount:    1,
		StallMs:       120,
		DroppedFrames: 3,
	}
}

// TestSaveLoadRoundTrip: a saved baseline reloads identically.
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	b := Baseline{Callers: 4, Seed: 7, Aggregate: sampleAggregate()}
	if err := Save(path, b); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Callers != b.Callers || got.Seed != b.Seed {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if got.Aggregate.TimeToStop.P95 != 60 || got.Aggregate.StallMs != 120 || got.Aggregate.DroppedFrames != 3 {
		t.Fatalf("aggregate mismatch after roundtrip: %+v", got.Aggregate)
	}
}

// TestCheckPassesWithinBudget: a current run within budget passes.
func TestCheckPassesWithinBudget(t *testing.T) {
	base := Baseline{Callers: 4, Seed: 7, Aggregate: sampleAggregate()}
	cur := sampleAggregate()
	cur.TimeToStop.P95 = 63    // +5% of 60 -> within 10%
	cur.DoubleTalkMs.Sum = 105 // +5% of 100 -> within 10%
	res := Check(base, cur, DefaultBudget)
	if !res.OK {
		t.Fatalf("expected pass, got violations: %+v", res.Violations)
	}
}

// TestCheckFailsOnTimeToStopRegression: a p95 beyond budget fails with a clear
// violation.
func TestCheckFailsOnTimeToStopRegression(t *testing.T) {
	base := Baseline{Callers: 4, Seed: 7, Aggregate: sampleAggregate()}
	cur := sampleAggregate()
	cur.TimeToStop.P95 = 100 // +66% on baseline 60, budget 10%
	res := Check(base, cur, DefaultBudget)
	if res.OK {
		t.Fatal("expected failure on time-to-stop regression")
	}
	found := false
	for _, v := range res.Violations {
		if v.Metric == "time_to_stop_p95_ms" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing time_to_stop violation: %+v", res.Violations)
	}
}

// TestCheckFailsOnNewStallsAndDrops: absolute budgets of 0 fail on any new stall
// or drop.
func TestCheckFailsOnNewStallsAndDrops(t *testing.T) {
	base := Baseline{Callers: 4, Seed: 7, Aggregate: sampleAggregate()}
	cur := sampleAggregate()
	cur.StallMs = 200      // > baseline 120 + 0
	cur.DroppedFrames = 10 // > baseline 3 + 0
	res := Check(base, cur, DefaultBudget)
	if res.OK {
		t.Fatal("expected failure on new stalls/drops")
	}
	if len(res.Violations) < 2 {
		t.Fatalf("expected >=2 violations, got %+v", res.Violations)
	}
}

// TestCheckAllowsSlackBudget: a generous budget tolerates a regression.
func TestCheckAllowsSlackBudget(t *testing.T) {
	base := Baseline{Callers: 4, Seed: 7, Aggregate: sampleAggregate()}
	cur := sampleAggregate()
	cur.TimeToStop.P95 = 90
	cur.DoubleTalkMs.Sum = 150
	cur.StallMs = 500
	cur.DroppedFrames = 20
	budget := Budget{
		MaxTimeToStopRegressionPct: 100,
		MaxDoubleTalkRegressionPct: 100,
		MaxStallRegression:         1000,
		MaxDroppedRegression:       100,
	}
	if res := Check(base, cur, budget); !res.OK {
		t.Fatalf("expected pass under slack budget: %+v", res.Violations)
	}
}

// TestLoadMissingFileErrors: loading a missing baseline errors.
func TestLoadMissingFileErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error loading missing baseline")
	}
}
