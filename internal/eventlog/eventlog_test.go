package eventlog

import (
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
func TestMergeDeterministic(t *testing.T) {
	logs := []Log{
		{SessionIndex: 0, Events: []Event{{Type: EventRecv, TS: 5, Frame: audio.Frame{Seq: 1}}}},
		{SessionIndex: 1, Events: []Event{{Type: EventRecv, TS: 5, Frame: audio.Frame{Seq: 1}}}},
	}
	if len(Merge(logs)) != len(Merge(logs)) {
		t.Fatal("merge length not stable")
	}
}
