// Package eventlog defines the byte-stable event-log types that sessions
// produce and the metrics layer consumes.
//
// The event log is the single source of truth for every metric. It carries
// frame send/receive events, explicit turn boundaries (TurnStart/TurnEnd),
// barge-in markers, and drop records — all timestamped on the injected clock so
// the log is byte-identical across replays of the same scenario+seed. It lives
// in its own package so metrics can be tested on hand-constructed logs without
// importing the session machinery (and to keep metrics free of any import
// cycle).
package eventlog

import (
	"sort"

	"voicechaos/internal/audio"
)

// EventType enumerates the kinds of entries in the log. The string forms are
// part of the stable log encoding.
type EventType uint8

const (
	// EventSend records a frame handed to the transport by the caller.
	EventSend EventType = iota
	// EventRecv records a frame the caller received from the transport. All
	// metrics are computed from receive-side timestamps.
	EventRecv
	// EventTurnStart marks the agent beginning a response turn (its first agent
	// frame, receive-side).
	EventTurnStart
	// EventTurnEnd marks the agent finishing a response turn (just after its
	// last agent frame, receive-side).
	EventTurnEnd
	// EventBargeIn marks the caller starting to speak over the agent (a
	// barge-in), at the send timestamp of the interrupting speech.
	EventBargeIn
	// EventDrop records a frame the impair layer dropped (sent but never
	// delivered).
	EventDrop
)

// String returns a stable lower-case name for the event type.
func (t EventType) String() string {
	switch t {
	case EventSend:
		return "send"
	case EventRecv:
		return "recv"
	case EventTurnStart:
		return "turn_start"
	case EventTurnEnd:
		return "turn_end"
	case EventBargeIn:
		return "barge_in"
	case EventDrop:
		return "drop"
	default:
		return "unknown"
	}
}

// Event is one entry in a session's event log. TS is the event time in integer
// milliseconds on the injected clock (receive-side time for EventRecv/turn
// markers, send-side time for EventSend/EventBargeIn). Frame carries the
// associated modeled frame (zero-valued for turn markers that have no frame).
// Turn is the 1-based index of the turn an event belongs to, used to attribute
// barge-ins and bound stall detection.
type Event struct {
	Type  EventType   `json:"type"`
	TS    int64       `json:"ts"`
	Turn  int         `json:"turn"`
	Frame audio.Frame `json:"frame"`
}

// Log is an ordered list of events for a single session, tagged with the
// session's index. Sessions append in occurrence order; Sort normalizes to the
// canonical total order before metrics/merge so the encoding is byte-stable.
type Log struct {
	SessionIndex int     `json:"session_index"`
	Events       []Event `json:"events"`
}

// Append adds an event to the log.
func (l *Log) Append(e Event) { l.Events = append(l.Events, e) }

// canonicalLess defines the total order used to make merged logs byte-stable
// regardless of goroutine scheduling: by timestamp, then session index, then
// event type, then frame sequence.
func canonicalLess(a, b Event, aSess, bSess int) bool {
	if a.TS != b.TS {
		return a.TS < b.TS
	}
	if aSess != bSess {
		return aSess < bSess
	}
	if a.Type != b.Type {
		return a.Type < b.Type
	}
	return a.Frame.Seq < b.Frame.Seq
}

// Sort orders the log's events into the canonical total order. The sort is
// stable so equal keys keep their append order.
func (l *Log) Sort() {
	sess := l.SessionIndex
	sort.SliceStable(l.Events, func(i, j int) bool {
		return canonicalLess(l.Events[i], l.Events[j], sess, sess)
	})
}

// MergedEvent pairs an Event with its originating session index, used when
// merging multiple session logs into one cross-session log.
type MergedEvent struct {
	SessionIndex int   `json:"session_index"`
	Event        Event `json:"event"`
}

// Merge combines per-session logs into a single cross-session log in the
// canonical total order (TS, sessionIndex, type, seq). Two runs of the same
// scenario+seed produce a byte-identical Merge, the determinism guarantee the
// runner asserts.
func Merge(logs []Log) []MergedEvent {
	var out []MergedEvent
	for _, l := range logs {
		for _, e := range l.Events {
			out = append(out, MergedEvent{SessionIndex: l.SessionIndex, Event: e})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return canonicalLess(out[i].Event, out[j].Event, out[i].SessionIndex, out[j].SessionIndex)
	})
	return out
}
