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

// MetricCheck is one budget constraint after evaluation, whether or not it
// held. Every constraint produces one of these, so a passing run is as
// inspectable as a failing one: "nothing regressed" and "this metric moved 9%
// against a 10% budget" are very different things to be told.
type MetricCheck struct {
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	Limit    float64 `json:"limit"`
	// Delta is current minus baseline, so a negative value is an improvement.
	Delta float64 `json:"delta"`
	OK    bool    `json:"ok"`
	// Message is set only when the constraint failed; it is the same text
	// Violation carries.
	Message string `json:"message,omitempty"`
}

// CheckResult is the outcome of comparing a fresh aggregate to a baseline.
type CheckResult struct {
	OK bool `json:"ok"`
	// Checks holds every constraint, passing or not, in a fixed order. It has
	// no omitempty: a consumer indexing into it should never have to
	// special-case an absent key.
	Checks []MetricCheck `json:"checks"`
	// Violations is the failing subset, kept so existing consumers and the
	// terminal output do not have to filter Checks themselves.
	Violations []Violation `json:"violations,omitempty"`
}

// Check compares the current aggregate against the baseline under the budget. It
// returns OK=true with no violations when every metric is within budget.
func Check(base Baseline, current metrics.Aggregate, budget Budget) CheckResult {
	res := CheckResult{OK: true}

	// Constraint order is fixed, so two runs render the same rows in the same
	// places and a diff of two reports is readable.
	limitTTS := float64(base.Aggregate.TimeToStop.P95) * (1 + budget.MaxTimeToStopRegressionPct/100)
	res.add(MetricCheck{
		Metric:   "time_to_stop_p95_ms",
		Baseline: float64(base.Aggregate.TimeToStop.P95),
		Current:  float64(current.TimeToStop.P95),
		Limit:    limitTTS,
		Message:  fmt.Sprintf("p95 time-to-stop %dms exceeds budget %.1fms (baseline %dms +%.0f%%)", current.TimeToStop.P95, limitTTS, base.Aggregate.TimeToStop.P95, budget.MaxTimeToStopRegressionPct),
	})

	limitDT := float64(base.Aggregate.DoubleTalkMs.Sum) * (1 + budget.MaxDoubleTalkRegressionPct/100)
	res.add(MetricCheck{
		Metric:   "double_talk_total_ms",
		Baseline: float64(base.Aggregate.DoubleTalkMs.Sum),
		Current:  float64(current.DoubleTalkMs.Sum),
		Limit:    limitDT,
		Message:  fmt.Sprintf("total double-talk %dms exceeds budget %.1fms", current.DoubleTalkMs.Sum, limitDT),
	})

	limitStall := base.Aggregate.StallMs + budget.MaxStallRegression
	res.add(MetricCheck{
		Metric:   "stall_total_ms",
		Baseline: float64(base.Aggregate.StallMs),
		Current:  float64(current.StallMs),
		Limit:    float64(limitStall),
		Message:  fmt.Sprintf("total stall %dms exceeds budget %dms", current.StallMs, limitStall),
	})

	limitDrop := base.Aggregate.DroppedFrames + budget.MaxDroppedRegression
	res.add(MetricCheck{
		Metric:   "dropped_frames",
		Baseline: float64(base.Aggregate.DroppedFrames),
		Current:  float64(current.DroppedFrames),
		Limit:    float64(limitDrop),
		Message:  fmt.Sprintf("dropped frames %d exceeds budget %d", current.DroppedFrames, limitDrop),
	})

	return res
}

// add evaluates one constraint and records it, deriving OK and Delta and
// appending to Violations when it failed. Every constraint is "current must not
// exceed limit", so the comparison lives here rather than at four call sites
// that could drift apart.
func (r *CheckResult) add(c MetricCheck) {
	c.OK = c.Current <= c.Limit
	c.Delta = c.Current - c.Baseline
	if c.OK {
		// A message only ever describes a failure; carrying one on a passing
		// row would show up in the JSON and read as a problem.
		c.Message = ""
	}
	r.Checks = append(r.Checks, c)
	if !c.OK {
		r.OK = false
		r.Violations = append(r.Violations, Violation{
			Metric:   c.Metric,
			Baseline: c.Baseline,
			Current:  c.Current,
			Limit:    c.Limit,
			Message:  c.Message,
		})
	}
}
