package audio

import "testing"

// TestOverlapMs checks the half-open interval overlap math used by double-talk.
func TestOverlapMs(t *testing.T) {
	cases := []struct {
		name string
		a, b Frame
		want int64
	}{
		{"disjoint", Frame{TS: 0, DurMs: 20}, Frame{TS: 100, DurMs: 20}, 0},
		{"touching half-open", Frame{TS: 0, DurMs: 20}, Frame{TS: 20, DurMs: 20}, 0},
		{"partial", Frame{TS: 0, DurMs: 30}, Frame{TS: 20, DurMs: 30}, 10},
		{"contained", Frame{TS: 0, DurMs: 100}, Frame{TS: 40, DurMs: 20}, 20},
		{"identical", Frame{TS: 10, DurMs: 50}, Frame{TS: 10, DurMs: 50}, 50},
	}
	for _, c := range cases {
		if got := OverlapMs(c.a, c.b); got != c.want {
			t.Errorf("%s: OverlapMs=%d, want %d", c.name, got, c.want)
		}
		// Symmetric.
		if got := OverlapMs(c.b, c.a); got != c.want {
			t.Errorf("%s: OverlapMs (reversed)=%d, want %d", c.name, got, c.want)
		}
	}
}

// TestFrameEndAndControl checks End() and the control-kind helper.
func TestFrameEndAndControl(t *testing.T) {
	f := Frame{TS: 100, DurMs: 20}
	if f.End() != 120 {
		t.Errorf("End=%d, want 120", f.End())
	}
	if !KindTurnStart.IsControl() || !KindTurnEnd.IsControl() {
		t.Error("turn markers should be control frames")
	}
	if KindAgent.IsControl() || KindSpeech.IsControl() || KindSilence.IsControl() {
		t.Error("audio kinds should not be control frames")
	}
}

// TestKindStringStable pins the stable string forms (part of the byte-stable log).
func TestKindStringStable(t *testing.T) {
	want := map[FrameKind]string{
		KindSilence: "silence", KindSpeech: "speech", KindAgent: "agent",
		KindTurnStart: "turn_start", KindTurnEnd: "turn_end",
	}
	for k, s := range want {
		if k.String() != s {
			t.Errorf("Kind %d String=%q, want %q", k, k.String(), s)
		}
	}
}
