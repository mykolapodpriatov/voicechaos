package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"voicechaos/internal/clock"
	"voicechaos/internal/eventlog"
	"voicechaos/internal/script"
	"voicechaos/internal/session"
	"voicechaos/internal/transport"
)

// Dialer opens session i's connection to a real endpoint for a live run. ctx
// bounds the dial, including the WebSocket opening handshake.
type Dialer func(ctx context.Context, sessionIndex int) (*transport.WSConn, error)

// CodecFactory returns a fresh transport.FrameCodec for one session. It is
// called once per session because a codec may carry per-connection state
// (e.g. GeminiLiveCodec tracks whether the model is mid-turn), so instances
// must never be shared across sessions.
type CodecFactory func() transport.FrameCodec

// liveScheduler adapts a Session's virtual-time Scheduler interface to real
// wall-clock time for the live path: NowMs reads the real clock, and Schedule
// fires fn after a time.AfterFunc delay computed from deliverAt-NowMs
// (clamped to zero). seq and sessionIndex are accepted but unused: unlike the
// deterministic offline engine's single shared heap (which needs them for its
// total order), a live run has no cross-session delivery order to maintain —
// each session's timers are independent real-time callbacks.
type liveScheduler struct {
	clk clock.RealClock
}

func (s *liveScheduler) NowMs() int64 { return s.clk.NowMs() }

func (s *liveScheduler) Schedule(deliverAt, _ int64, _ int, fn func()) {
	delay := time.Duration(deliverAt-s.NowMs()) * time.Millisecond
	if delay < 0 {
		delay = 0
	}
	time.AfterFunc(delay, fn)
}

var _ session.Scheduler = (*liveScheduler)(nil)

// liveRig bundles one live session's wiring.
type liveRig struct {
	sess *session.Session
	tr   transport.Transport
}

// RunLive builds N = sc.Callers sessions against a REAL endpoint: each
// session dials its own transport.WSConn via dial, wraps it in a
// transport.WSTransport on clock.RealClock using a codec from newCodec, and
// runs a Session directly against it. There is no FakeAgent — a real endpoint
// drives its own turns — and no impair.Queue: the impairment model exists to
// make the OFFLINE path's chaos reproducible, but a live run is already
// subject to whatever the real network and endpoint do, so layering a
// SIMULATED impairment on top of it would just distort a measurement that is
// already authentic. Consequently DroppedFrames is always 0 for a live run
// and its metrics are not directly comparable to an offline baseline recorded
// under an impair.Profile.
//
// A session's speech (scripted prompts and reactive barge-ins) is timed on
// REAL elapsed time from when that session is primed, using the same
// Session/Script machinery as the offline path. In particular a barge-in
// still fires bi.IntoMs after the caller OBSERVES a TurnStart — Session's
// receive loop reacts identically whether that TurnStart came from the
// offline FakeAgent or was decoded from the live endpoint's own
// response-started event, so no separate barge-in logic is needed here.
//
// If sc.MaxDurationMs > 0 the run is bounded to that many real milliseconds
// past this call; reaching that bound is a normal, successful end of run (not
// reported as an error). If it is 0 the run is unbounded and continues until
// ctx is cancelled — callers driving an unbounded live scenario must supply a
// context that is itself bounded (a timeout, or Ctrl-C via
// signal.NotifyContext).
func RunLive(ctx context.Context, sc *script.Scenario, dial Dialer, newCodec CodecFactory, inst *Instrumentation) (Result, error) {
	runCtx := ctx
	if sc.MaxDurationMs > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(sc.MaxDurationMs)*time.Millisecond)
		defer cancel()
	}

	rigs := make([]liveRig, 0, sc.Callers)
	closeAll := func() {
		for _, r := range rigs {
			_ = r.tr.Close()
		}
	}

	sched := &liveScheduler{}
	for i := 0; i < sc.Callers; i++ {
		conn, err := dial(runCtx, i)
		if err != nil {
			closeAll()
			return Result{}, fmt.Errorf("engine: live dial session %d: %w", i, err)
		}
		tr := transport.NewWSTransport(conn, newCodec(), clock.RealClock{})
		se := session.New(i, sched, tr, sc)
		rigs = append(rigs, liveRig{sess: se, tr: tr})
	}

	// Prime every session (schedules its scripted prompts on real time) before
	// launching the read pumps, mirroring the offline path's prime-before-drive
	// ordering.
	for _, r := range rigs {
		r.sess.Prime(runCtx)
	}

	var wg sync.WaitGroup
	for i := range rigs {
		r := rigs[i]
		track(&wg, inst, func() { _ = r.sess.Serve(runCtx) })
	}

	// Wait for the run's bound: either the caller's ctx, or (when
	// sc.MaxDurationMs > 0) the deadline derived from it above.
	<-runCtx.Done()
	closeAll()
	wg.Wait()

	logs := make([]eventlog.Log, len(rigs))
	for i, r := range rigs {
		log := r.sess.Log()
		log.Sort()
		logs[i] = log
	}
	// Report the ORIGINAL ctx's error, not runCtx's: reaching our own
	// MaxDurationMs deadline is the run finishing normally, not a failure, so it
	// must never surface as context.DeadlineExceeded to the caller. Genuine
	// cancellation of the caller-supplied ctx still propagates.
	return Result{Logs: logs}, ctx.Err()
}
