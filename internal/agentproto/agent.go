// Package agentproto models the behavior of the voice agent under test on the
// offline loopback path. It is the "other end" of a session: it consumes the
// caller's speech frames and streams back agent response frames, reacting to
// barge-ins by stopping within a configurable latency.
//
// Against a REAL endpoint there is no agentproto: the WS transport carries the
// endpoint's own frames and the same metrics are computed from observed
// timestamps. agentproto exists so the metric definitions are testable
// deterministically — its StopLatency, stall, and double-talk behavior are
// scriptable, so a known input yields a known time-to-stop / stall / overlap.
package agentproto

import (
	"context"

	"voicechaos/internal/audio"
)

// Agent is a participant that drives the agent-side end of a loopback
// transport. Run blocks until ctx is cancelled or the conversation ends,
// emitting the turn-control and agent frames that the caller-side session
// observes. Implementations must honor the injected clock (no real sleeps).
type Agent interface {
	// Run drives the agent until ctx is cancelled. It returns ctx.Err() on
	// cancellation, or nil on a clean end-of-conversation.
	Run(ctx context.Context) error
}

// Scheduler is the subset of clock.ManualClock an offline Agent needs to time
// its own frame emissions on virtual time.
type Scheduler interface {
	NowMs() int64
	Schedule(deliverAt, seq int64, sessionIndex int, fn func())
}

// turnControl builds the out-of-band control frame for a turn boundary.
func turnControl(kind audio.FrameKind, seq, ts int64) audio.Frame {
	return audio.Frame{Seq: seq, TS: ts, DurMs: 0, Kind: kind, PayloadLen: 0}
}
