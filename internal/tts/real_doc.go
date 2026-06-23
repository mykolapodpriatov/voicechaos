//go:build realtts

// This file documents the OPTIONAL real TTS/STT adapters (ElevenLabs synthesis,
// Deepgram transcription). They are designed-for, not implemented in this pass,
// and excluded from the default stdlib-only build by the "realtts" build tag —
// so the default binary and CI never require any speech-service SDK or network
// access.
//
// To work on them: `go build -tags realtts ./...`. Each adapter implements the
// interfaces already defined in this package:
//
//   - An ElevenLabs Synthesizer streams real TTS audio and maps it to modeled
//     audio.Frame timing (so the rest of the harness — impairment, metrics — is
//     unchanged); it must respect a context for cancellation and never block a
//     session past its lifetime.
//   - A Deepgram Transcriber performs real STT on received agent audio and
//     reports whether a transcript satisfies an expected marker, replacing the
//     default substring FakeTranscriber for scenarios that assert on real agent
//     speech content.
//
// Keeping these behind a build tag preserves the determinism and dependency-free
// guarantees of the default build while leaving a clear, typed seam for the
// real integrations.
package tts
