// Package metrics computes barge-in correctness metrics from session event
// logs using precise, documented definitions. A load tool whose metrics are
// ambiguous is useless, so each definition is pinned and tested on constructed
// logs:
//
//   - time-to-stop: recv_ts(last agent frame before the interrupted turn's
//     TurnEnd) - barge_in_send_ts, per interrupted turn. Zero agent frames
//     after the barge-in in that turn => 0 (the agent stopped immediately).
//   - double-talk: total ms of overlap between caller speech intervals
//     [send_ts, send_ts+DurMs) and agent intervals anchored at their RECEIVE
//     time [recv_ts, recv_ts+DurMs).
//   - stall: a gap > threshold between consecutive received agent frames,
//     bounded within a single [TurnStart, TurnEnd) interval, so natural
//     inter-turn silence is never counted.
//   - dropped frames: count of EventDrop entries (frames the impair layer
//     dropped).
//
// All values come from receive-side timestamps in the byte-stable log, so two
// runs of the same scenario+seed yield identical metrics.
package metrics

import (
	"sort"

	"voicechaos/internal/audio"
	"voicechaos/internal/eventlog"
)

// SessionMetrics holds one session's computed metrics.
type SessionMetrics struct {
	SessionIndex int `json:"session_index"`
	// TimeToStopMs is one entry per interrupted turn, in turn order.
	TimeToStopMs []int64 `json:"time_to_stop_ms"`
	// DoubleTalkMs is the total caller/agent overlap across the session.
	DoubleTalkMs int64 `json:"double_talk_ms"`
	// StallCount and StallMs summarize within-turn agent stalls.
	StallCount int   `json:"stall_count"`
	StallMs    int64 `json:"stall_ms"`
	// DroppedFrames is the number of frames the impair layer dropped.
	DroppedFrames int `json:"dropped_frames"`
	// ReorderedFrames is the number of received agent frames that arrived out of
	// order: a frame whose Seq is below the running maximum Seq already received.
	ReorderedFrames int `json:"reordered_frames"`
}

// turnSpan records the receive-side [start,end) bounds of a turn and whether an
// end was observed.
type turnSpan struct {
	start    int64
	end      int64
	hasStart bool
	hasEnd   bool
}

// ComputeSession computes the metrics for a single session log. stallThresholdMs
// is the gap above which within-turn agent silence counts as a stall. The input
// is sorted into canonical order first so results are independent of append
// order.
func ComputeSession(log eventlog.Log, stallThresholdMs int) SessionMetrics {
	log.Sort()
	m := SessionMetrics{SessionIndex: log.SessionIndex}

	spans := turnSpans(log)

	m.TimeToStopMs = timeToStop(log, spans)
	m.DoubleTalkMs = doubleTalk(log)
	m.StallCount, m.StallMs = stalls(log, spans, int64(stallThresholdMs))
	m.DroppedFrames = droppedFrames(log)
	m.ReorderedFrames = reorderedFrames(log)
	return m
}

// turnSpans extracts the receive-side [TurnStart, TurnEnd) span of each turn.
func turnSpans(log eventlog.Log) map[int]*turnSpan {
	spans := map[int]*turnSpan{}
	for _, e := range log.Events {
		switch e.Type {
		case eventlog.EventTurnStart:
			s := spans[e.Turn]
			if s == nil {
				s = &turnSpan{}
				spans[e.Turn] = s
			}
			s.start = e.TS
			s.hasStart = true
		case eventlog.EventTurnEnd:
			s := spans[e.Turn]
			if s == nil {
				s = &turnSpan{}
				spans[e.Turn] = s
			}
			s.end = e.TS
			s.hasEnd = true
		}
	}
	return spans
}

// timeToStop returns one time-to-stop value per interrupted turn (a turn that
// has a barge-in), in ascending turn order.
func timeToStop(log eventlog.Log, spans map[int]*turnSpan) []int64 {
	// Earliest barge-in send timestamp per turn.
	bargeAt := map[int]int64{}
	for _, e := range log.Events {
		if e.Type == eventlog.EventBargeIn {
			if _, ok := bargeAt[e.Turn]; !ok {
				bargeAt[e.Turn] = e.TS
			}
		}
	}
	// Last received agent-frame timestamp per turn after that turn's barge-in.
	lastAgentAfter := map[int]int64{}
	hasAgentAfter := map[int]bool{}
	for _, e := range log.Events {
		if e.Type != eventlog.EventRecv || e.Frame.Kind != audio.KindAgent {
			continue
		}
		bts, ok := bargeAt[e.Turn]
		if !ok {
			continue
		}
		// Only frames at/after the barge-in within the turn's span count toward
		// "still talking after the barge-in".
		if e.TS < bts {
			continue
		}
		if span := spans[e.Turn]; span != nil && span.hasEnd && e.TS >= span.end {
			continue
		}
		if !hasAgentAfter[e.Turn] || e.TS > lastAgentAfter[e.Turn] {
			lastAgentAfter[e.Turn] = e.TS
			hasAgentAfter[e.Turn] = true
		}
	}

	turns := make([]int, 0, len(bargeAt))
	for t := range bargeAt {
		turns = append(turns, t)
	}
	sort.Ints(turns)

	out := make([]int64, 0, len(turns))
	for _, t := range turns {
		if hasAgentAfter[t] {
			out = append(out, lastAgentAfter[t]-bargeAt[t])
		} else {
			out = append(out, 0) // agent stopped immediately
		}
	}
	return out
}

// doubleTalk sums overlap between caller speech (send-side intervals) and agent
// audio (receive-side intervals).
func doubleTalk(log eventlog.Log) int64 {
	var caller, agent []audio.Frame
	for _, e := range log.Events {
		switch {
		case e.Type == eventlog.EventSend && e.Frame.Kind == audio.KindSpeech:
			caller = append(caller, audio.Frame{TS: e.TS, DurMs: e.Frame.DurMs})
		case e.Type == eventlog.EventRecv && e.Frame.Kind == audio.KindAgent:
			// Anchor the agent interval at its receive time.
			agent = append(agent, audio.Frame{TS: e.TS, DurMs: e.Frame.DurMs})
		}
	}
	var total int64
	for _, c := range caller {
		for _, a := range agent {
			total += audio.OverlapMs(c, a)
		}
	}
	return total
}

// stalls counts gaps > threshold between consecutive received agent frames
// within a single turn span and sums those gap durations.
func stalls(log eventlog.Log, spans map[int]*turnSpan, threshold int64) (count int, totalMs int64) {
	// Collect received agent-frame receive timestamps per turn, in order.
	perTurn := map[int][]int64{}
	for _, e := range log.Events {
		if e.Type == eventlog.EventRecv && e.Frame.Kind == audio.KindAgent {
			perTurn[e.Turn] = append(perTurn[e.Turn], e.TS)
		}
	}
	// Iterate turns in sorted order (not map-range order) so the computation is
	// deterministic by construction, matching timeToStop. Totals are scalar sums
	// today, but a fixed iteration order keeps results stable if that ever changes.
	turns := make([]int, 0, len(perTurn))
	for turn := range perTurn {
		turns = append(turns, turn)
	}
	sort.Ints(turns)
	for _, turn := range turns {
		ts := perTurn[turn]
		span := spans[turn]
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
		for i := 1; i < len(ts); i++ {
			gap := ts[i] - ts[i-1]
			if gap <= threshold {
				continue
			}
			// Both endpoints must lie within [TurnStart, TurnEnd).
			if span != nil {
				if span.hasStart && ts[i-1] < span.start {
					continue
				}
				if span.hasEnd && ts[i] >= span.end {
					continue
				}
			}
			count++
			totalMs += gap
		}
	}
	return count, totalMs
}

// droppedFrames counts EventDrop entries.
func droppedFrames(log eventlog.Log) int {
	n := 0
	for _, e := range log.Events {
		if e.Type == eventlog.EventDrop {
			n++
		}
	}
	return n
}

// reorderedFrames counts received agent frames that arrive out of order: a frame
// whose Seq is below the running maximum Seq of agent frames already received.
// The log is in canonical receive order (ComputeSession sorts it first), so a
// lower Seq at a later position is precisely a frame the caller observed
// reordered — a deterministic, receive-side complement to dropped frames.
func reorderedFrames(log eventlog.Log) int {
	var runningMax int64
	seen := false
	count := 0
	for _, e := range log.Events {
		if e.Type != eventlog.EventRecv || e.Frame.Kind != audio.KindAgent {
			continue
		}
		if seen && e.Frame.Seq < runningMax {
			count++
			continue
		}
		runningMax = e.Frame.Seq
		seen = true
	}
	return count
}
