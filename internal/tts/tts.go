// Package tts defines the TTS/STT interfaces the synthetic caller uses and
// ships deterministic, marker-based fakes. The default build never calls a real
// speech service: the caller's "speech" is modeled timed frames and its "STT"
// is a marker matcher. Real ElevenLabs/Deepgram adapters are optional build-tag
// adapters (see the package docs); they implement these same interfaces.
package tts

import "voicechaos/internal/audio"

// Synthesizer turns a piece of caller text into a sequence of modeled speech
// frames covering a duration. It is the seam where a real TTS engine would
// produce audio; the default FakeSynthesizer produces deterministic timed
// frames so runs stay reproducible.
type Synthesizer interface {
	// Synthesize returns the speech frames for text starting at startMs on the
	// clock base, at the given frame cadence. The frames carry KindSpeech and a
	// PayloadLen size proxy.
	Synthesize(text string, startMs int64, frameMs, payloadLen int) []audio.Frame
}

// Transcriber is the caller's STT seam: it decides whether a received agent
// payload matches an expected marker. The default fake does a substring match,
// avoiding any real transcription while still letting scenarios assert "the
// agent said X".
type Transcriber interface {
	// Matches reports whether transcript text from the agent satisfies the
	// expected marker.
	Matches(transcript, marker string) bool
}

// FakeSynthesizer is a deterministic Synthesizer: it emits ceil(len(text)/
// CharsPerFrame) frames (at least one) at the frame cadence, so identical text
// always yields identical frames.
type FakeSynthesizer struct {
	// CharsPerFrame controls how many characters map to one frame. Defaults to
	// DefaultCharsPerFrame when zero.
	CharsPerFrame int
}

// DefaultCharsPerFrame is the default text-to-frame ratio for FakeSynthesizer.
const DefaultCharsPerFrame = 4

// Synthesize implements Synthesizer deterministically.
func (s FakeSynthesizer) Synthesize(text string, startMs int64, frameMs, payloadLen int) []audio.Frame {
	cpf := s.CharsPerFrame
	if cpf <= 0 {
		cpf = DefaultCharsPerFrame
	}
	if frameMs <= 0 {
		frameMs = audio.DefaultFrameMs
	}
	n := (len(text) + cpf - 1) / cpf
	if n <= 0 {
		n = 1
	}
	frames := make([]audio.Frame, n)
	for i := 0; i < n; i++ {
		frames[i] = audio.Frame{
			Seq:        int64(i),
			TS:         startMs + int64(i*frameMs),
			DurMs:      frameMs,
			Kind:       audio.KindSpeech,
			PayloadLen: payloadLen,
		}
	}
	return frames
}

// FakeTranscriber is a deterministic Transcriber doing a substring match.
type FakeTranscriber struct{}

// Matches reports whether marker is empty or a substring of transcript.
func (FakeTranscriber) Matches(transcript, marker string) bool {
	if marker == "" {
		return true
	}
	return contains(transcript, marker)
}

// contains is a tiny substring check avoiding a strings import for this single
// use (keeps the dependency surface obvious).
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var (
	_ Synthesizer = FakeSynthesizer{}
	_ Transcriber = FakeTranscriber{}
)
