// Package runner drives a Scenario to a Report: it runs the deterministic
// offline pipeline (the single shared-clock engine), computes per-session and
// aggregate metrics, and enforces the goroutine-ownership discipline that keeps
// an N-session load harness leak-free.
//
// Goroutine ownership (strict): each session owns exactly its read pumps, all
// derived from the run context. On cancel/timeout the transports' read deadline
// unblocks the pumps, the drivers return, and every wg entry fires. The leak
// check is OWNERSHIP-BASED, not NumGoroutine-based: an atomic lifecycle counter
// increments on goroutine start and decrements on exit, and Run asserts (via the
// returned Report) that it returns to zero after wg.Wait — a deterministic,
// environment-independent quiescence condition.
package runner

import (
	"context"
	"errors"
	"sync/atomic"

	"voicechaos/internal/engine"
	"voicechaos/internal/eventlog"
	"voicechaos/internal/metrics"
	"voicechaos/internal/script"
)

// Report is the result of a run.
type Report struct {
	// Scenario echoes the run inputs needed to interpret the metrics.
	Callers int   `json:"callers"`
	Seed    int64 `json:"seed"`
	// Sessions holds each session's metrics, in session-index order.
	Sessions []metrics.SessionMetrics `json:"sessions"`
	// Aggregate summarizes across sessions (the baseline target).
	Aggregate metrics.Aggregate `json:"aggregate"`
	// Logs holds each session's raw event log (in session-index order), so the
	// caller can assert byte-identical replay or inspect events.
	Logs []eventlog.Log `json:"-"`

	// PeakGoroutines is the peak number of concurrently live session-owned
	// goroutines observed (bounded by 2*Callers).
	PeakGoroutines int64 `json:"peak_goroutines"`
	// LeakedGoroutines is the lifecycle counter after wg.Wait; a correct run
	// leaves it at zero.
	LeakedGoroutines int64 `json:"leaked_goroutines"`
}

// Merged returns all sessions' events merged into one cross-session log in the
// canonical total order. Two runs of the same scenario+seed produce a
// byte-identical slice.
func (r Report) Merged() []eventlog.MergedEvent { return eventlog.Merge(r.Logs) }

// Runner runs scenarios. MaxConcurrency bounds the number of session-owned
// goroutines live at once; for the shared-clock offline engine the pumps must
// all be co-resident, so MaxConcurrency is clamped up to 2*Callers.
type Runner struct {
	// MaxConcurrency bounds concurrent session goroutines. Zero means unbounded
	// (sized to all sessions).
	MaxConcurrency int
}

// Run executes the scenario and returns its Report. It validates the scenario,
// drives the deterministic engine under an ownership leak counter, and computes
// metrics. ctx cancels the run (timeout/Ctrl-C); cancellation unblocks all read
// pumps and the runner still drains (cancel-before-wait) so no goroutine leaks.
func (rn *Runner) Run(ctx context.Context, sc *script.Scenario) (Report, error) {
	if err := sc.Validate(); err != nil {
		return Report{}, err
	}

	var live, peak int64
	inst := &engine.Instrumentation{Live: &live, Peak: &peak}
	// Each session owns two goroutines; the shared-clock engine needs them all
	// co-resident, so the semaphore must admit 2*Callers tokens.
	needed := 2 * sc.Callers
	bound := needed
	if rn.MaxConcurrency > 0 && rn.MaxConcurrency < bound {
		// Honor the requested bound only if it still admits all pumps; otherwise
		// the shared-clock driver would deadlock. Report the conflict clearly.
		return Report{}, errors.New("runner: MaxConcurrency too small for the shared-clock engine; needs >= 2*callers")
	}
	inst.Sem = make(chan struct{}, bound)

	res, runErr := engine.Run(ctx, sc, inst)

	leaked := atomic.LoadInt64(&live)

	rep := Report{
		Callers:          sc.Callers,
		Seed:             sc.Seed,
		Logs:             res.Logs,
		PeakGoroutines:   atomic.LoadInt64(&peak),
		LeakedGoroutines: leaked,
	}
	threshold := sc.StallThreshold()
	rep.Sessions = make([]metrics.SessionMetrics, len(res.Logs))
	for i, lg := range res.Logs {
		rep.Sessions[i] = metrics.ComputeSession(lg, threshold)
	}
	rep.Aggregate = metrics.ComputeAggregate(rep.Sessions)
	return rep, runErr
}
