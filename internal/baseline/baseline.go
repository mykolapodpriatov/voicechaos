// Package baseline saves a metrics aggregate to JSON and checks a fresh run
// against it under a budget, returning a clear pass/fail diff. Because the
// offline run is deterministic given the seed, a baseline is stable and a CI
// gate built on it is not flaky.
package baseline

import (
	"encoding/json"
	"fmt"
	"os"

	"voicechaos/internal/metrics"
)

// Baseline is a saved aggregate plus the scenario identity it was produced from.
type Baseline struct {
	Callers   int               `json:"callers"`
	Seed      int64             `json:"seed"`
	Aggregate metrics.Aggregate `json:"aggregate"`
}

// Save writes the baseline to path as indented JSON.
func Save(path string, b Baseline) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// Load reads a baseline from path.
func Load(path string) (Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Baseline{}, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return Baseline{}, fmt.Errorf("baseline: parse %s: %w", path, err)
	}
	return b, nil
}

// Budget bounds how far a fresh run may regress from the baseline before Check
// fails. Percentage budgets are applied to the corresponding baseline value;
// absolute budgets are added to it.
type Budget struct {
	// MaxTimeToStopRegressionPct caps growth of the p95 time-to-stop, as a
	// percentage of the baseline p95 (e.g. 10 = allow up to +10%).
	MaxTimeToStopRegressionPct float64 `json:"max_time_to_stop_regression_pct"`
	// MaxDoubleTalkRegressionPct caps growth of total double-talk (sum), as a
	// percentage of the baseline.
	MaxDoubleTalkRegressionPct float64 `json:"max_double_talk_regression_pct"`
	// MaxStallRegression caps growth of total stalled milliseconds (absolute).
	MaxStallRegression int64 `json:"max_stall_regression"`
	// MaxDroppedRegression caps growth of dropped frames (absolute).
	MaxDroppedRegression int `json:"max_dropped_regression"`
}

// DefaultBudget is a reasonable starting budget: 10% on latency/double-talk and
// no new stalls or drops.
var DefaultBudget = Budget{
	MaxTimeToStopRegressionPct: 10,
	MaxDoubleTalkRegressionPct: 10,
	MaxStallRegression:         0,
	MaxDroppedRegression:       0,
}

// Violation describes one failed budget constraint.
type Violation struct {
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	Limit    float64 `json:"limit"`
	Message  string  `json:"message"`
}

// CheckResult is the outcome of comparing a fresh aggregate to a baseline.
type CheckResult struct {
	OK         bool        `json:"ok"`
	Violations []Violation `json:"violations,omitempty"`
}

// Check compares the current aggregate against the baseline under the budget. It
// returns OK=true with no violations when every metric is within budget.
func Check(base Baseline, current metrics.Aggregate, budget Budget) CheckResult {
	res := CheckResult{OK: true}

	// time-to-stop p95 (percentage budget).
	limitTTS := float64(base.Aggregate.TimeToStop.P95) * (1 + budget.MaxTimeToStopRegressionPct/100)
	if float64(current.TimeToStop.P95) > limitTTS {
		res.OK = false
		res.Violations = append(res.Violations, Violation{
			Metric:   "time_to_stop_p95_ms",
			Baseline: float64(base.Aggregate.TimeToStop.P95),
			Current:  float64(current.TimeToStop.P95),
			Limit:    limitTTS,
			Message:  fmt.Sprintf("p95 time-to-stop %dms exceeds budget %.1fms (baseline %dms +%.0f%%)", current.TimeToStop.P95, limitTTS, base.Aggregate.TimeToStop.P95, budget.MaxTimeToStopRegressionPct),
		})
	}

	// double-talk total (percentage budget).
	limitDT := float64(base.Aggregate.DoubleTalkMs.Sum) * (1 + budget.MaxDoubleTalkRegressionPct/100)
	if float64(current.DoubleTalkMs.Sum) > limitDT {
		res.OK = false
		res.Violations = append(res.Violations, Violation{
			Metric:   "double_talk_total_ms",
			Baseline: float64(base.Aggregate.DoubleTalkMs.Sum),
			Current:  float64(current.DoubleTalkMs.Sum),
			Limit:    limitDT,
			Message:  fmt.Sprintf("total double-talk %dms exceeds budget %.1fms", current.DoubleTalkMs.Sum, limitDT),
		})
	}

	// stalls (absolute budget).
	limitStall := base.Aggregate.StallMs + budget.MaxStallRegression
	if current.StallMs > limitStall {
		res.OK = false
		res.Violations = append(res.Violations, Violation{
			Metric:   "stall_total_ms",
			Baseline: float64(base.Aggregate.StallMs),
			Current:  float64(current.StallMs),
			Limit:    float64(limitStall),
			Message:  fmt.Sprintf("total stall %dms exceeds budget %dms", current.StallMs, limitStall),
		})
	}

	// dropped frames (absolute budget).
	limitDrop := base.Aggregate.DroppedFrames + budget.MaxDroppedRegression
	if current.DroppedFrames > limitDrop {
		res.OK = false
		res.Violations = append(res.Violations, Violation{
			Metric:   "dropped_frames",
			Baseline: float64(base.Aggregate.DroppedFrames),
			Current:  float64(current.DroppedFrames),
			Limit:    float64(limitDrop),
			Message:  fmt.Sprintf("dropped frames %d exceeds budget %d", current.DroppedFrames, limitDrop),
		})
	}

	return res
}
