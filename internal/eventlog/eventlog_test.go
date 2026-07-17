package eventlog

import (
	"reflect"
	"testing"

	"voicechaos/internal/audio"
)

// TestSortCanonicalOrder: events sort by (TS, type, seq) within a session.
func TestSortCanonicalOrder(t *testing.T) {
	lg := Log{SessionIndex: 0}
	lg.Append(Event{Type: EventRecv, TS: 100, Frame: audio.Frame{Seq: 2}})
	lg.Append(Event{Type: EventSend, TS: 100, Frame: audio.Frame{Seq: 1}})
	lg.Append(Event{Type: EventTurnStart, TS: 50})
	lg.Sort()
	if lg.Events[0].Type != EventTurnStart || lg.Events[0].TS != 50 {
		t.Fatalf("first event %v@%d, want turn_start@50", lg.Events[0].Type, lg.Events[0].TS)
	}
	// At TS=100, EventSend (lower type) precedes EventRecv.
	if lg.Events[1].Type != EventSend || lg.Events[2].Type != EventRecv {
		t.Fatalf("tie order wrong: %v then %v", lg.Events[1].Type, lg.Events[2].Type)
	}
}

// TestMergeOrdersBySessionThenTime: merging two sessions interleaves by TS, then
// session index.
func TestMergeOrdersBySessionThenTime(t *testing.T) {
	a := Log{SessionIndex: 0, Events: []Event{
		{Type: EventSend, TS: 10}, {Type: EventSend, TS: 30},
	}}
	b := Log{SessionIndex: 1, Events: []Event{
		{Type: EventSend, TS: 10}, {Type: EventSend, TS: 20},
	}}
	merged := Merge([]Log{a, b})
	// Expected order: (0,10),(1,10),(1,20),(0,30).
	want := []struct {
		sess int
		ts   int64
	}{{0, 10}, {1, 10}, {1, 20}, {0, 30}}
	if len(merged) != len(want) {
		t.Fatalf("merged len %d, want %d", len(merged), len(want))
	}
	for i, w := range want {
		if merged[i].SessionIndex != w.sess || merged[i].Event.TS != w.ts {
			t.Fatalf("merged[%d]=(s%d,%d), want (s%d,%d)", i, merged[i].SessionIndex, merged[i].Event.TS, w.sess, w.ts)
		}
	}
}

// TestMergeDeterministic: merging the same logs twice yields identical output.
//
// The two events are deliberately identical in every canonical sort key EXCEPT
// SessionIndex (same TS, same type, same Seq), so they form the one tie that
// only the session-index tie-break can resolve. That makes this the input most
// able to expose a non-total ordering, so the assertions compare the merged
// events in full — comparing only lengths would pass for any ordering.
func TestMergeDeterministic(t *testing.T) {
	logs := []Log{
		{SessionIndex: 0, Events: []Event{{Type: EventRecv, TS: 5, Frame: audio.Frame{Seq: 1}}}},
		{SessionIndex: 1, Events: []Event{{Type: EventRecv, TS: 5, Frame: audio.Frame{Seq: 1}}}},
	}
	first, second := Merge(logs), Merge(logs)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("merge not stable across calls:\nfirst  = %+v\nsecond = %+v", first, second)
	}
	// Merge must also leave its input untouched, or a second merge of the same
	// logs would be merging different data.
	if !reflect.DeepEqual(logs[0].Events[0], logs[1].Events[0]) {
		t.Fatalf("Merge mutated its input logs: %+v", logs)
	}
	// The tie resolves by session index, ascending — the ordering the equal
	// TS/type/Seq input above exists to pin down.
	want := []MergedEvent{
		{SessionIndex: 0, Event: Event{Type: EventRecv, TS: 5, Frame: audio.Frame{Seq: 1}}},
		{SessionIndex: 1, Event: Event{Type: EventRecv, TS: 5, Frame: audio.Frame{Seq: 1}}},
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("tie not broken by session index:\ngot  = %+v\nwant = %+v", first, want)
	}
}
