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

	// Truncated marks a report whose run was cut short by a wall-clock bound
	// rather than by the script finishing. The metrics in it are real but
	// cover less of the scenario than the scenario describes, so they must not
	// be compared against a baseline recorded from a complete run. A report
	// that is silently short is worse than no report, which is why this is on
	// the report itself and not only on the CLI's exit code.
	Truncated bool `json:"truncated,omitempty"`
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
	// Live, when non-nil, selects the live path (engine.RunLive) against a real
	// endpoint instead of the default deterministic offline loopback+FakeAgent
	// path (engine.Run). Nil means offline.
	Live *LiveConfig
}

// LiveConfig configures Runner.Run's live path: how to dial each session's
// connection to the real endpoint and which FrameCodec to speak on it.
type LiveConfig struct {
	// Dial opens session i's transport.WSConn to the real endpoint.
	Dial engine.Dialer
	// NewCodec returns a fresh transport.FrameCodec for one session; it is
	// called once per session (see engine.CodecFactory).
	NewCodec engine.CodecFactory
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

	var res engine.Result
	var runErr error
	if rn.Live != nil {
		// Each live session owns exactly one goroutine (its Serve read pump; there
		// is no FakeAgent driver on the live path), so the semaphore only needs to
		// admit Callers tokens.
		bound := sc.Callers
		if rn.MaxConcurrency > 0 && rn.MaxConcurrency < bound {
			return Report{}, errors.New("runner: MaxConcurrency too small for the live engine; needs >= callers")
		}
		inst.Sem = make(chan struct{}, bound)
		res, runErr = engine.RunLive(ctx, sc, rn.Live.Dial, rn.Live.NewCodec, inst)
	} else {
		// Each session owns two goroutines; the shared-clock engine needs them all
		// co-resident, so the semaphore must admit 2*Callers tokens.
		bound := 2 * sc.Callers
		if rn.MaxConcurrency > 0 && rn.MaxConcurrency < bound {
			// Honor the requested bound only if it still admits all pumps; otherwise
			// the shared-clock driver would deadlock. Report the conflict clearly.
			return Report{}, errors.New("runner: MaxConcurrency too small for the shared-clock engine; needs >= 2*callers")
		}
		inst.Sem = make(chan struct{}, bound)
		res, runErr = engine.Run(ctx, sc, inst)
	}

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
