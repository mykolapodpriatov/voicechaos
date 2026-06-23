package agentproto

import (
	"context"
	"errors"
	"sync"

	"voicechaos/internal/audio"
	"voicechaos/internal/transport"
)

// FakeAgent is a deterministic, scriptable Agent used for all offline tests and
// loopback runs. It streams a fixed number of agent frames per response turn at
// the modeled cadence, reacts to a caller barge-in by stopping within
// StopLatencyMs, and can be scripted to ignore barge-ins (producing double-talk)
// or to stall mid-turn (producing a stall). Every emission is timed on the
// injected clock; nothing sleeps on real time.
type FakeAgent struct {
	cfg          FakeConfig
	sched        Scheduler
	tr           transport.Transport
	sessionIndex int

	mu       sync.Mutex
	seq      int64 // agent-side frame sequence
	schedSeq int64 // strictly-increasing scheduling counter (timer ordering only)
	turn     int   // current/last turn number (1-based); 0 = none yet
	talking  bool  // a turn is currently emitting
	// speechGen increments on each caller speech frame received while idle; the
	// endpointing callback uses it to fire only after the utterance has settled
	// (no newer speech), so the agent responds once per utterance rather than
	// once per frame.
	speechGen int64
	// stopAt is the virtual time after which queued emits for the current turn
	// are suppressed (set on barge-in to now+StopLatencyMs). Valid only while
	// interrupted is true.
	stopAt      int64
	interrupted bool
	// ended marks that the current turn's TurnEnd has already been sent, so a
	// redundant end (e.g. the full-length end after an early interrupted end) is
	// suppressed.
	ended bool
}

// FakeConfig parameterizes FakeAgent's scripted behavior.
type FakeConfig struct {
	// FramesPerTurn is how many agent frames a full, uninterrupted response
	// contains.
	FramesPerTurn int
	// FrameMs is the cadence between agent frames (and each frame's DurMs).
	// Defaults to audio.DefaultFrameMs when zero.
	FrameMs int
	// PayloadLen is the size proxy (bytes) for each agent frame, feeding the
	// bandwidth model.
	PayloadLen int
	// StopLatencyMs is how long after a barge-in the agent keeps emitting before
	// it stops. time-to-stop is measured against this.
	StopLatencyMs int
	// IgnoreBargeIn makes the agent keep talking through a barge-in (it never
	// sets a stop deadline), which the metrics observe as double-talk.
	IgnoreBargeIn bool
	// StallBeforeFrame, if > 0, inserts a StallMs gap before emitting the agent
	// frame at this 0-based index, modeling a mid-turn stall.
	StallBeforeFrame int
	// StallMs is the size of the inserted stall gap.
	StallMs int
	// EndpointMs is the silence gap after the caller's last speech frame at
	// which the agent considers the utterance finished and begins responding.
	// Defaults to one FrameMs when zero. Speech frames arriving before the agent
	// starts talking belong to the same utterance (not barge-ins).
	EndpointMs int
}

// NewFakeAgent builds a FakeAgent that drives the agent-side transport end tr,
// timing emissions on sched with the given sessionIndex (which orders its
// scheduled deliveries against other sessions).
func NewFakeAgent(cfg FakeConfig, sched Scheduler, tr transport.Transport, sessionIndex int) *FakeAgent {
	if cfg.FrameMs == 0 {
		cfg.FrameMs = audio.DefaultFrameMs
	}
	if cfg.FramesPerTurn == 0 {
		cfg.FramesPerTurn = 1
	}
	if cfg.EndpointMs == 0 {
		cfg.EndpointMs = cfg.FrameMs
	}
	return &FakeAgent{cfg: cfg, sched: sched, tr: tr, sessionIndex: sessionIndex}
}

// Run consumes caller frames and emits response turns until ctx is cancelled or
// the transport is closed (end of the caller's script). The first caller speech
// frame while idle starts a turn; a caller speech frame while talking is a
// barge-in.
func (a *FakeAgent) Run(ctx context.Context) error {
	for {
		f, err := a.tr.Recv(ctx)
		if err != nil {
			if errors.Is(err, transport.ErrClosed) {
				return nil
			}
			return err
		}
		if f.Kind != audio.KindSpeech {
			continue // silence frames don't trigger responses
		}
		a.onCallerSpeech(ctx, f)
	}
}

// onCallerSpeech reacts to a caller speech frame. While the agent is talking a
// speech frame is a barge-in; while idle it (re)arms endpointing so the agent
// responds once the caller's utterance settles (EndpointMs of no new speech).
func (a *FakeAgent) onCallerSpeech(ctx context.Context, _ audio.Frame) {
	a.mu.Lock()
	if a.talking {
		// Barge-in. Unless scripted to ignore it, schedule a stop StopLatencyMs
		// from now; queued emits past that time are suppressed, and the turn is
		// closed right after the stop so [TurnStart, TurnEnd) tightly bounds the
		// delivered audio.
		if !a.cfg.IgnoreBargeIn && !a.interrupted {
			a.interrupted = true
			a.stopAt = a.sched.NowMs() + int64(a.cfg.StopLatencyMs)
			turn := a.turn
			endAt := a.stopAt + int64(a.cfg.FrameMs)
			a.mu.Unlock()
			a.scheduleAt(endAt, func() { a.emitTurnEnd(ctx, turn, endAt) })
			return
		}
		a.mu.Unlock()
		return
	}
	// Idle: extend the current utterance and (re)arm the endpointing timer. Only
	// the latest-armed callback (matching speechGen) will actually start a turn.
	a.speechGen++
	gen := a.speechGen
	respondAt := a.sched.NowMs() + int64(a.cfg.EndpointMs)
	a.mu.Unlock()
	a.scheduleAt(respondAt, func() { a.maybeRespond(ctx, gen, respondAt) })
}

// maybeRespond starts a response turn if no newer caller speech arrived after
// the endpointing timer was armed (gen still current) and the agent is still
// idle.
func (a *FakeAgent) maybeRespond(ctx context.Context, gen, at int64) {
	a.mu.Lock()
	if a.talking || a.speechGen != gen {
		a.mu.Unlock()
		return
	}
	a.turn++
	a.talking = true
	a.interrupted = false
	a.ended = false
	turn := a.turn
	a.mu.Unlock()
	a.startTurn(ctx, turn, at)
}

// startTurn schedules the control + agent frames for one response turn on the
// virtual clock. The TurnStart marker goes out at t0; agent frame i goes out at
// t0 + i*FrameMs (plus any scripted stall); the TurnEnd marker follows the last
// emitted frame.
func (a *FakeAgent) startTurn(ctx context.Context, turn int, t0 int64) {
	cfg := a.cfg
	// TurnStart control frame at t0 (carrying timestamp t0).
	a.scheduleSend(ctx, t0, audio.KindTurnStart, t0, t0)

	lastFrameTS := t0
	for i := 0; i < cfg.FramesPerTurn; i++ {
		offset := int64(i * cfg.FrameMs)
		if cfg.StallBeforeFrame > 0 && i >= cfg.StallBeforeFrame {
			offset += int64(cfg.StallMs)
		}
		ts := t0 + offset
		lastFrameTS = ts
		idx := i
		a.scheduleAt(ts, func() {
			a.emitAgentFrame(ctx, turn, idx, ts, cfg.FrameMs, cfg.PayloadLen)
		})
	}

	// TurnEnd: after the last frame's interval. If the turn is interrupted, the
	// effective end is bounded by the stop deadline (handled at emit time), but
	// we always close the turn so the caller observes a TurnEnd.
	endTS := lastFrameTS + int64(cfg.FrameMs)
	a.scheduleAt(endTS, func() {
		a.emitTurnEnd(ctx, turn, endTS)
	})
}

// emitAgentFrame sends one agent frame unless the turn was interrupted and this
// frame falls after the stop deadline.
func (a *FakeAgent) emitAgentFrame(ctx context.Context, turn, _ int, ts int64, durMs, payload int) {
	a.mu.Lock()
	if a.turn != turn || !a.talking {
		a.mu.Unlock()
		return
	}
	if a.interrupted && ts > a.stopAt {
		a.mu.Unlock()
		return // suppressed: agent has stopped within StopLatencyMs
	}
	a.mu.Unlock()

	f := audio.Frame{Seq: a.nextSeq(), TS: ts, DurMs: durMs, Kind: audio.KindAgent, PayloadLen: payload}
	_ = a.tr.Send(ctx, f)
}

// emitTurnEnd closes the current turn and sends the TurnEnd control frame at the
// moment it fires (which is endTS on the clock). It is idempotent per turn: an
// interrupted turn closes early at stopAt+FrameMs via this callback, and the
// later full-length callback then no-ops, so [TurnStart, TurnEnd) tightly bounds
// the delivered audio.
func (a *FakeAgent) emitTurnEnd(ctx context.Context, turn int, endTS int64) {
	a.mu.Lock()
	if a.turn != turn || a.ended {
		a.mu.Unlock()
		return
	}
	a.ended = true
	a.talking = false
	seq := a.seq
	a.seq++
	a.mu.Unlock()
	_ = a.tr.Send(ctx, turnControl(audio.KindTurnEnd, seq, endTS))
}

// timerSeqBase places the agent's self-scheduled callbacks in a high sequence
// band so that, at an equal delivery timestamp, an inbound caller-frame delivery
// (which carries a small per-stream sequence) is always processed before an
// agent timer that fires at the same instant. This makes endpointing and
// barge-in suppression robust: a caller speech frame arriving at the same
// virtual time as the agent's respond/emit timer is seen first, so the agent
// never mistakes the tail of an utterance for the start of its own turn, and a
// barge-in frame is processed before a same-instant emit it should suppress.
// It keeps the (deliverAt, seq, sessionIndex) order fully deterministic.
const timerSeqBase = int64(1) << 40

// nextSchedSeq returns a strictly-increasing scheduling sequence in the timer
// band (timerSeqBase + counter). Every scheduled agent callback gets a UNIQUE,
// monotonically increasing value, so two callbacks that fire at the SAME
// deliverAt within a turn (e.g. a zero-jitter profile where consecutive frames
// share a timestamp) get a strict tie-break on the heap's
// (deliverAt, seq, sessionIndex) order — making their firing order a strict
// total order rather than heap-arbitrary, which is required for byte-identical
// replay. The counter stays far below the next band boundary for any realistic
// run, so the "caller delivery before agent timer" ordering is preserved.
func (a *FakeAgent) nextSchedSeq() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.schedSeq
	a.schedSeq++
	return timerSeqBase + s
}

// scheduleAt schedules fn to run at virtual time ts, ordered after any inbound
// deliveries at the same timestamp and after any earlier-scheduled agent timer
// at that timestamp (via the strictly-increasing scheduling seq).
func (a *FakeAgent) scheduleAt(ts int64, fn func()) {
	a.sched.Schedule(ts, a.nextSchedSeq(), a.sessionIndex, fn)
}

// scheduleSend schedules sending a control frame of the given kind at virtual
// time at, carrying timestamp ts.
func (a *FakeAgent) scheduleSend(ctx context.Context, at int64, kind audio.FrameKind, ts, _ int64) {
	seq := a.nextSeq()
	a.sched.Schedule(at, a.nextSchedSeq(), a.sessionIndex, func() {
		_ = a.tr.Send(ctx, turnControl(kind, seq, ts))
	})
}

// nextSeq returns the next agent-side sequence number.
func (a *FakeAgent) nextSeq() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.seq
	a.seq++
	return s
}

var _ Agent = (*FakeAgent)(nil)
