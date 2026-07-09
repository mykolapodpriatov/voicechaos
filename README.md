# voicechaos

> A load and chaos test harness for real-time voice agents — injects barge-ins, jitter, packet loss, and bandwidth caps to find where they break, with **deterministic, replayable** runs and CI regression baselines.

![status](https://img.shields.io/badge/status-M1--M3%20complete-brightgreen) ![language](https://img.shields.io/badge/language-Go-blue) ![deps](https://img.shields.io/badge/deps-stdlib--only-success) ![license](https://img.shields.io/badge/license-MIT-green)

`voicechaos` drives N concurrent synthetic voice sessions against a real-time endpoint, scripts **millisecond-precise barge-ins**, **deterministically degrades the transport** (added latency, jitter, reorder, packet loss, bandwidth caps), and measures **barge-in correctness** (time-to-stop, double-talk, response stalls, dropped frames). Scenarios are replayable from a seed and metrics can be committed as a baseline so CI fails when interruption latency or resilience regresses.

## Why

Voice agents are demoed on a perfect connection and fall apart under real networks and interruptions. This finds the breaking point before your users do — and, crucially, it does so **deterministically**: the same scenario and seed replay to a **byte-identical event log and identical metrics**, so a CI baseline means something instead of flaking.

## The deterministic-chaos core (what makes it different)

Reproducible "chaos" is the whole point. The offline pipeline has no wall-clock and no shared RNG:

- **One shared `ManualClock` with an event-loop scheduler.** A single min-heap holds the next delivery across *all* sessions; a global `Advance()` pops the next `deliverAt` and jumps virtual time to it. There is no `time.Sleep` in any pure logic — the clock advances to the next scheduled delivery. Delivery order is the single total order `(deliverAt, seq, sessionIndex)`, so replay is independent of goroutine scheduling.
- **Per-session RNG.** Each session gets its own `rand.New(rand.NewSource(seed + sessionIndex))` — never a shared `*rand.Rand`. Re-running session *k* alone reproduces its exact loss/jitter/reorder trace, and sessions never perturb each other.
- **`impair` is a constrained delivery *queue*** with a pinned composition order — **loss → latency+jitter → reorder → bandwidth** — so two implementations produce the same event log from `(seed, profile)`. Jitter is the non-negative range `[0, 2×Jitter]` with a `deliverAt = max(now, deliverAt)` floor; bandwidth uses the backlog formula `delay_ms = PayloadLen·8·1000 / BandwidthBps` with `deliverAt = max(deliverAt, lastDelivery) + delay_ms`, so congestion accumulates under saturation.
- **Loopback transport** returns two `Transport` ends (caller and agent); a scriptable `FakeAgent` drives the agent end, so the metric definitions are testable against known inputs.

The result: `voicechaos run scenario.json` twice → the same numbers; a baseline committed today still passes tomorrow.

## Honest scope

- **Audio is modeled as timed frames, not real PCM.** A `Frame` is `{Seq, TS(ms), Kind, DurMs, PayloadLen}` on the injected clock; the harness measures **turn-taking timing**, not perceptual audio quality (it is not a MOS analyzer). `PayloadLen` is the size proxy for the bandwidth model. This is what keeps runs deterministic and byte-stable.
- **Turn boundaries are explicit events.** The event log carries `TurnStart`/`TurnEnd` markers (the `FakeAgent` emits them; the WebSocket adapter maps the endpoint's response-start / `response.done`). Metrics are computed relative to these markers, never inferred from silence — so two implementations produce identical numbers.
- **The default build is stdlib-only.** No external Go modules; `go.mod` has no `require` block. The real-endpoint transport is a **hand-rolled RFC6455 WebSocket client** (handshake, per-frame `crypto/rand` masking, continuation-frame reassembly, auto-pong, close handshake, oversized-message rejection, leak-free context cancellation).
- **WebRTC and real TTS/STT are optional, behind build tags, and *not* in the default build or CI.** `internal/transport/webrtc` (pion) and the ElevenLabs/Deepgram adapters are documented seams behind `//go:build webrtc` / `//go:build realtts`. The default ships a deterministic loopback transport, the stdlib WS transport, and marker-based fake TTS/STT.

## Metrics (precise, receive-side definitions)

All metrics come from the receive-side event log (what the caller observes — impairment delay is part of the measured experience):

- **time-to-stop** — `recv_ts(last agent frame before the interrupted turn's TurnEnd) − barge_in_send_ts`; `0` if no agent frame arrives after the barge-in (the agent stopped immediately).
- **double-talk** — total ms of overlap between caller speech intervals `[send_ts, +DurMs)` and agent intervals anchored at their receive time `[recv_ts, +DurMs)`.
- **stall** — a gap `> stall_threshold_ms` between consecutive received agent frames, bounded within a `[TurnStart, TurnEnd)` interval (natural inter-turn silence never counts).
- **dropped frames** — frames the impair layer dropped (recorded at drop time).
- Aggregated across sessions with **p50/p95 via the nearest-rank method** (integer-stable).

## Install & build

```sh
go build ./...                 # default, stdlib-only
go install ./cmd/voicechaos    # installs the `voicechaos` binary
```

Requires Go 1.23+. There is nothing to `go get`.

## Usage

```sh
# Run a scenario on the deterministic loopback path and write a report
voicechaos run scenario.json --out report.json
voicechaos report report.json

# Save a metrics baseline, then fail CI on regression beyond a budget
voicechaos baseline save scenario.json --out baseline.json
voicechaos check scenario.json --baseline baseline.json   # exit 1 on regression
```

### Scenario (JSON)

```json
{
  "callers": 4,
  "seed": 7,
  "stall_threshold_ms": 60,
  "profile": { "added_latency_ms": 30, "jitter_ms": 8, "reorder_prob": 0.05, "loss_prob": 0.02, "bandwidth_bps": 64000 },
  "agent": { "frames_per_turn": 25, "frame_ms": 20, "payload_len": 160, "stop_latency_ms": 40, "endpoint_ms": 20 },
  "script": { "turns": [
    { "at_ms": 0, "dur_ms": 60, "payload_len": 160,
      "barge_in": { "into_ms": 120, "dur_ms": 80, "payload_len": 160 } }
  ] }
}
```

A barge-in fires `into_ms` after the caller observes the agent's `TurnStart` for that turn — i.e. "at T ms into the agent's reply, the caller starts speaking." The downlink (agent → caller) carries the impairment the caller's metrics measure; the uplink is clean so a barge-in always reaches the agent.

## Architecture

```
cmd/voicechaos/            run | baseline | check | report (graceful shutdown)
internal/
  audio/                   Frame: timed-frame audio model (no PCM)
  clock/                   Clock iface; ManualClock + single-heap event-loop scheduler
  transport/               Transport iface; deterministic loopback; stdlib RFC6455 WS client + codecs
    webrtc/                optional pion adapter behind //go:build webrtc (documented, not default)
  impair/                  constrained delivery queue: loss/latency+jitter/reorder/bandwidth (seeded)
  agentproto/              modeled agent + scriptable FakeAgent (barge-in stop, stall, double-talk)
  session/                 one synthetic caller: runs a Script, drives barge-ins, records events
  eventlog/                byte-stable event log + canonical merge
  metrics/                 time-to-stop, double-talk, stall, dropped frames; nearest-rank percentiles
  script/                  Scenario + Script (JSON) + validation
  config/                  loads + validates Scenario JSON (DisallowUnknownFields); CLI↔runner boundary
  baseline/                save/load + budgeted pass/fail
  tts/                     TTS/STT interfaces + deterministic fakes (real adapters behind //go:build realtts)
  runner/                  bounded pool, cancel-before-wait, ownership-based leak counter
  engine/                  assembles + drives the deterministic offline pipeline
```

## Determinism & race-safety

The whole default pipeline is deterministic and offline (injected clock + seeded RNG + loopback + `FakeAgent`), so a scenario replays to the identical event log and metrics. The suite runs under `go test -race`, with an **ownership-based** goroutine-leak assertion on the runner (an atomic lifecycle counter that returns to zero after `wg.Wait()`, not a brittle `NumGoroutine` check) and a byte-identical-replay test across concurrent runs.

## Roadmap

- **M1–M3 (done):** deterministic clock + impairment core, loopback + `FakeAgent`, scripted barge-ins, metrics, runner, scenario/baseline/CLI, and the stdlib RFC6455 WebSocket transport with OpenAI-Realtime / Gemini-Live frame-mapping codecs.
- **M4 (designed):** optional pion WebRTC adapter and real ElevenLabs/Deepgram TTS/STT adapters (build-tag seams already in place), a live dashboard, and a scenario recorder from a real call.

## License

[MIT](LICENSE) © 2026 Mykola Podpriatov
