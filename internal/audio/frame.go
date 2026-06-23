// Package audio models the audio stream as a sequence of timed frames rather
// than real PCM samples. The harness measures turn-taking timing, not
// perceptual audio quality, so a frame only needs to describe its position on
// a timeline, how long it lasts, and a size proxy used by the bandwidth model.
package audio

// FrameKind classifies a frame by who produced it and whether it carries
// speech. The kinds are used by the metrics layer to compute double-talk
// (caller speech overlapping agent speech) and by sessions to detect a
// barge-in (caller speech arriving while the agent is talking).
type FrameKind uint8

const (
	// KindSilence is a caller-side frame carrying no speech (the caller is
	// quiet). It never triggers a barge-in.
	KindSilence FrameKind = iota
	// KindSpeech is a caller-side frame carrying speech. A KindSpeech frame
	// arriving while the agent is talking is a barge-in.
	KindSpeech
	// KindAgent is an agent-side frame (the agent's spoken response).
	KindAgent
	// KindTurnStart is an out-of-band control frame marking the start of an
	// agent response turn. It carries no audio (PayloadLen 0, DurMs 0); the
	// session converts it to an EventTurnStart at its receive timestamp. It is
	// the modeled analogue of a real endpoint's response-created signal.
	KindTurnStart
	// KindTurnEnd is an out-of-band control frame marking the end of an agent
	// response turn (the analogue of response.done). The session converts it to
	// an EventTurnEnd at its receive timestamp.
	KindTurnEnd
)

// String returns a stable, lower-case name for the kind. The names are part of
// the byte-stable event log, so they must not change casually.
func (k FrameKind) String() string {
	switch k {
	case KindSilence:
		return "silence"
	case KindSpeech:
		return "speech"
	case KindAgent:
		return "agent"
	case KindTurnStart:
		return "turn_start"
	case KindTurnEnd:
		return "turn_end"
	default:
		return "unknown"
	}
}

// IsControl reports whether the kind is an out-of-band control frame
// (turn-start or turn-end) carrying no audio.
func (k FrameKind) IsControl() bool {
	return k == KindTurnStart || k == KindTurnEnd
}

// DefaultFrameMs is the modeled audio cadence in milliseconds. Each frame
// nominally covers DefaultFrameMs of audio. 20ms matches the common Opus frame
// size used by real-time voice transports.
const DefaultFrameMs = 20

// Frame is one modeled unit of audio on the injected clock's timeline.
//
// A frame covers the half-open interval [TS, TS+DurMs): TS is the inclusive
// start in integer milliseconds on the clock base (no wall-clock), and the
// frame ends just before TS+DurMs. Using integer milliseconds keeps event logs
// byte-stable across runs. PayloadLen is the size in bytes used purely as the
// input to the bandwidth serialization model in package impair; it is not real
// audio data.
type Frame struct {
	// Seq is a per-stream monotonically increasing sequence number. It is the
	// primary tie-breaker for delivery ordering and reassembly.
	Seq int64 `json:"seq"`
	// TS is the inclusive start of the frame in integer milliseconds on the
	// injected clock base.
	TS int64 `json:"ts"`
	// DurMs is the frame duration in milliseconds; the frame occupies
	// [TS, TS+DurMs).
	DurMs int `json:"dur_ms"`
	// Kind classifies the frame (silence, speech, or agent).
	Kind FrameKind `json:"kind"`
	// PayloadLen is the size proxy in bytes for the bandwidth model.
	PayloadLen int `json:"payload_len"`
}

// End returns the exclusive end of the frame's interval, TS+DurMs.
func (f Frame) End() int64 {
	return f.TS + int64(f.DurMs)
}

// OverlapMs returns the number of milliseconds the half-open interval of f
// overlaps the half-open interval of g. It is symmetric and never negative.
// This is the building block for the double-talk metric.
func OverlapMs(f, g Frame) int64 {
	lo := f.TS
	if g.TS > lo {
		lo = g.TS
	}
	hi := f.End()
	if ghi := g.End(); ghi < hi {
		hi = ghi
	}
	if hi <= lo {
		return 0
	}
	return hi - lo
}
