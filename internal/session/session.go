// Package session runs one synthetic caller: it plays a Script over an
// (impaired) Transport, drives millisecond-precise barge-ins, and records a
// byte-stable event log of everything sent and received. The recorded log is
// the sole input to the metrics layer.
package session

import (
	"context"
	"errors"
	"sync"

	"voicechaos/internal/audio"
	"voicechaos/internal/eventlog"
	"voicechaos/internal/script"
	"voicechaos/internal/transport"
)

// Scheduler is the subset of clock.ManualClock a session needs to time its
// caller speech on virtual time.
type Scheduler interface {
	NowMs() int64
	Schedule(deliverAt, seq int64, sessionIndex int, fn func())
}

// Session is one synthetic caller bound to a caller-side Transport, a shared
// clock, and a Script. It runs a receive loop that records events and schedules
// its caller speech (prompts and reactive barge-ins) on the clock.
type Session struct {
	index   int
	sched   Scheduler
	tr      transport.Transport
	sc      *script.Scenario
	frameMs int

	// mu guards log and obsTurn, which are touched both by the receive loop
	// (session goroutine) and by scheduled send callbacks (clock driver
	// goroutine).
	mu sync.Mutex
	// log is the recorded event log.
	log eventlog.Log
	// callerSeq is the caller-side frame sequence counter, advanced only while
	// scheduling sends on the session goroutine.
	callerSeq int64
	// obsTurn counts agent turns observed via TurnStart (1-based), used to map a
	// barge-in onto the caller turn that should react to it.
	obsTurn int
	// startMs is the virtual time captured at Prime; script offsets are relative
	// to it.
	startMs int64
}

// New builds a session with the given index over transport tr, timing caller
// speech on sched per scenario sc.
func New(index int, sched Scheduler, tr transport.Transport, sc *script.Scenario) *Session {
	fm := sc.Agent.FrameMs
	if fm == 0 {
		fm = audio.DefaultFrameMs
	}
	return &Session{
		index:   index,
		sched:   sched,
		tr:      tr,
		sc:      sc,
		frameMs: fm,
		log:     eventlog.Log{SessionIndex: index},
	}
}

// Log returns a snapshot of the recorded event log. Safe to call after Run
// returns.
func (s *Session) Log() eventlog.Log {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]eventlog.Event, len(s.log.Events))
	copy(events, s.log.Events)
	return eventlog.Log{SessionIndex: s.log.SessionIndex, Events: events}
}

// appendEvent records one event under the lock.
func (s *Session) appendEvent(e eventlog.Event) {
	s.mu.Lock()
	s.log.Append(e)
	s.mu.Unlock()
}

// currentTurn reads the observed-turn counter under the lock.
func (s *Session) currentTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.obsTurn
}

// Prime schedules the caller's prompt speech onto the clock. It must be called
// (on the driving goroutine, before the clock is advanced) so the scheduler is
// non-empty when the run begins. ctx scopes the eventual sends.
func (s *Session) Prime(ctx context.Context) {
	s.startMs = s.sched.NowMs()
	s.scheduleCallerPrompts(ctx)
}

// Serve consumes received frames until the transport closes or ctx is
// cancelled, recording events and scheduling reactive barge-ins. Call Prime
// first. Serve is the session's read pump and is intended to run as its own
// goroutine.
func (s *Session) Serve(ctx context.Context) error {
	for {
		f, err := s.tr.Recv(ctx)
		if err != nil {
			if errors.Is(err, transport.ErrClosed) {
				return nil
			}
			return err
		}
		s.onRecv(ctx, f)
	}
}

// Run primes and serves in one call (convenience for callers that schedule
// before launching the read pump on the same goroutine flow). Most callers use
// Prime + Serve so priming completes before the clock advances.
func (s *Session) Run(ctx context.Context) error {
	s.Prime(ctx)
	return s.Serve(ctx)
}

// scheduleCallerPrompts schedules the speech bursts for every caller turn's
// prompt at its absolute virtual offset.
func (s *Session) scheduleCallerPrompts(ctx context.Context) {
	for _, t := range s.sc.Script.Turns {
		turn := t
		s.scheduleSpeechBurst(ctx, s.startMs+int64(turn.AtMs), turn.DurMs, turn.PayloadLen, false)
	}
}

// onRecv records a received frame. Control frames become turn markers (with
// receive-side timestamps); agent audio frames are recorded as EventRecv and,
// on the first TurnStart of a turn, may trigger a scheduled barge-in.
func (s *Session) onRecv(ctx context.Context, f audio.Frame) {
	now := s.sched.NowMs()
	switch f.Kind {
	case audio.KindTurnStart:
		s.mu.Lock()
		s.obsTurn++
		turn := s.obsTurn
		s.log.Append(eventlog.Event{Type: eventlog.EventTurnStart, TS: now, Turn: turn})
		s.mu.Unlock()
		s.maybeScheduleBargeIn(ctx, turn, now)
	case audio.KindTurnEnd:
		s.appendEvent(eventlog.Event{Type: eventlog.EventTurnEnd, TS: now, Turn: s.currentTurn()})
	default:
		s.appendEvent(eventlog.Event{Type: eventlog.EventRecv, TS: now, Turn: s.currentTurn(), Frame: f})
	}
}

// maybeScheduleBargeIn schedules a barge-in speech burst for the caller turn
// matching the just-started agent turn, IntoMs after this TurnStart.
func (s *Session) maybeScheduleBargeIn(ctx context.Context, turn int, turnStartTS int64) {
	idx := turn - 1
	if idx < 0 || idx >= len(s.sc.Script.Turns) {
		return
	}
	bi := s.sc.Script.Turns[idx].BargeIn
	if bi == nil {
		return
	}
	at := turnStartTS + int64(bi.IntoMs)
	s.scheduleSpeechBurst(ctx, at, bi.DurMs, bi.PayloadLen, true)
}

// scheduleSpeechBurst schedules a run of caller speech frames covering
// [startMs, startMs+durMs) at the frame cadence. When isBargeIn is set, the
// first frame additionally records an EventBargeIn at its send timestamp (the
// barge_in_send_ts that time-to-stop is measured against). A zero/negative
// duration still emits one frame so a barge-in is always observable by the
// agent.
func (s *Session) scheduleSpeechBurst(ctx context.Context, startMs int64, durMs, payload int, isBargeIn bool) {
	n := durMs / s.frameMs
	if n <= 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		ts := startMs + int64(i*s.frameMs)
		seq := s.nextCallerSeq()
		first := i == 0
		s.sched.Schedule(ts, seq, s.index, func() {
			f := audio.Frame{Seq: seq, TS: ts, DurMs: s.frameMs, Kind: audio.KindSpeech, PayloadLen: payload}
			turn := s.currentTurn()
			if isBargeIn && first {
				s.appendEvent(eventlog.Event{Type: eventlog.EventBargeIn, TS: ts, Turn: turn, Frame: f})
			}
			s.appendEvent(eventlog.Event{Type: eventlog.EventSend, TS: ts, Turn: turn, Frame: f})
			_ = s.tr.Send(ctx, f)
		})
	}
}

// nextCallerSeq returns the next caller-side sequence number. It is called only
// while scheduling, which the clock serializes per session.
func (s *Session) nextCallerSeq() int64 {
	v := s.callerSeq
	s.callerSeq++
	return v
}
