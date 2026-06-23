package tts

import (
	"testing"

	"voicechaos/internal/audio"
)

// TestFakeSynthesizerDeterministic: identical text yields identical frames.
func TestFakeSynthesizerDeterministic(t *testing.T) {
	s := FakeSynthesizer{CharsPerFrame: 4}
	a := s.Synthesize("hello world", 1000, 20, 160)
	b := s.Synthesize("hello world", 1000, 20, 160)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("frame %d differs", i)
		}
	}
}

// TestFakeSynthesizerFrameCount: ceil(len/CharsPerFrame) frames, at the cadence.
func TestFakeSynthesizerFrameCount(t *testing.T) {
	s := FakeSynthesizer{CharsPerFrame: 4}
	// 11 chars / 4 = ceil 3 frames.
	frames := s.Synthesize("hello world", 500, 20, 160)
	if len(frames) != 3 {
		t.Fatalf("frame count %d, want 3", len(frames))
	}
	for i, f := range frames {
		if f.Kind != audio.KindSpeech {
			t.Fatalf("frame %d kind %v, want speech", i, f.Kind)
		}
		if f.TS != int64(500+i*20) {
			t.Fatalf("frame %d ts %d, want %d", i, f.TS, 500+i*20)
		}
		if f.DurMs != 20 || f.PayloadLen != 160 {
			t.Fatalf("frame %d dims wrong: %+v", i, f)
		}
	}
}

// TestFakeSynthesizerEmptyTextOneFrame: empty text still yields one frame.
func TestFakeSynthesizerEmptyTextOneFrame(t *testing.T) {
	got := (FakeSynthesizer{}).Synthesize("", 0, 20, 100)
	if len(got) != 1 {
		t.Fatalf("empty text frames %d, want 1", len(got))
	}
}

// TestFakeTranscriberMatch: substring matching semantics.
func TestFakeTranscriberMatch(t *testing.T) {
	tr := FakeTranscriber{}
	if !tr.Matches("the quick brown fox", "quick") {
		t.Error("expected match for substring")
	}
	if tr.Matches("hello", "world") {
		t.Error("unexpected match")
	}
	if !tr.Matches("anything", "") {
		t.Error("empty marker should always match")
	}
}
