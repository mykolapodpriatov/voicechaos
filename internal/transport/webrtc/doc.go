//go:build webrtc

// Package webrtc is the OPTIONAL pion-based WebRTC transport adapter. It is
// designed-for, not implemented in this pass, and is excluded from the default
// stdlib-only build by the "webrtc" build tag — so `go build ./...` and CI never
// require pion (github.com/pion/webrtc) or any external module.
//
// To work on it: `go build -tags webrtc ./...` after adding pion to go.mod. The
// adapter must satisfy transport.Transport (Send/Recv/Close of modeled
// audio.Frame) by carrying frames over an RTCDataChannel (or an audio track with
// a modeled-payload bridge), driving the connection from a session context so a
// timeout/cancel tears down peer connections without leaking goroutines —
// mirroring the discipline of the stdlib loopback and WebSocket transports.
//
// Design sketch:
//
//   - NewWebRTCTransport(ctx, signalingURL) negotiates an offer/answer via a
//     pluggable signaler, opens a reliable, ordered DataChannel, and returns a
//     transport.Transport.
//   - Send marshals an audio.Frame to the channel; Recv reassembles and stamps a
//     receive timestamp from the injected clock (RealClock for live runs).
//   - The same impair model can be layered by shaping Send via a DelayFunc, but
//     real WebRTC already adds network behavior, so impairment is typically
//     applied at the OS/network layer for this adapter.
//
// Until implemented, this file exists only to document the seam and to keep the
// package path stable.
package webrtc
