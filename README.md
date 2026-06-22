# voicechaos

> A load and chaos test harness for real-time voice agents — injects barge-ins, jitter, packet loss, and silence to find where they break.

![status](https://img.shields.io/badge/status-early%20development-orange) ![language](https://img.shields.io/badge/language-Go-blue) ![license](https://img.shields.io/badge/license-MIT-green)

Drives a fleet of N concurrent synthetic voice sessions against an OpenAI Realtime / Gemini Live / custom WebSocket endpoint over WebRTC/WS. It scripts millisecond-precise barge-in timing, deterministically degrades the media transport, and measures barge-in correctness.

## Why

Voice agents are demoed on a perfect connection and fall apart under real network conditions and interruptions. This finds the breaking point before your users do — and the concurrent-session/network-shaping core is ideal for Go.

## Features

- Spawns N concurrent synthetic voice sessions against Realtime/Live/custom WS
- Scriptable conversation flows with millisecond-precise timed barge-ins
- Deterministic transport-layer impairment: latency, jitter, reorder, loss, bandwidth caps
- Barge-in correctness metrics: time-to-stop, double-talk, stalls, dropped frames
- Replayable scenarios + regression baselines that fail CI; pluggable caller TTS/STT

## How it works

Describe a scenario (callers, script, timed barge-ins, network profile). voicechaos spins up concurrent sessions, shapes each transport deterministically, and reports interruption-latency and audio-resilience metrics against a saved baseline.

## Tech stack

- Go
- pion/webrtc
- gorilla/websocket
- ElevenLabs / Deepgram
- faster-whisper / local TTS

## Status & roadmap

🚧 **Early development.** This repository is being built in the open; the scaffold and design are in place and the implementation is landing incrementally.

- [ ] Concurrent synthetic-caller sessions over WS/WebRTC
- [ ] Scripted timed barge-ins + barge-in correctness metrics
- [ ] Deterministic transport impairment (latency/jitter/loss/reorder)
- [ ] Regression baselines + CI gate; Gemini Live adapter

## Installation

> Coming soon.

## License

[MIT](LICENSE) © 2026 Mykola Podpriatov
