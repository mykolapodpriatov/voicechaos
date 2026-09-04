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

# Run the same scenario against a real endpoint instead of the offline loopback
voicechaos run scenario.json --endpoint wss://api.example.com/realtime --codec openai-realtime --out report.json

# Authenticate the WebSocket upgrade (OpenAI Realtime). Prefer --header-env so
# the key is not copied into the shell history; put the full header value in
# the env var (including the "Bearer " prefix). Shell expansion works too.
export OPENAI_AUTH="Bearer $OPENAI_API_KEY"
voicechaos run scenario.json \
  --endpoint "wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview" \
  --codec openai-realtime \
  --header "OpenAI-Beta: realtime=v1" \
  --header-env "Authorization=OPENAI_AUTH"
# equivalent: --header "Authorization: Bearer $OPENAI_API_KEY"
```

### Live runs (`--endpoint`)

`--endpoint` (with a matching `--codec`, one of `openai-realtime` or `gemini-live`) drives the scenario's script against a real WebSocket endpoint through the stdlib `transport.WSTransport`, instead of the offline loopback + `FakeAgent`. Repeatable `--header "Name: value"` flags (curl `-H` form) and `--header-env NAME=ENVVAR` are written onto the opening handshake so APIs that require `Authorization` / `OpenAI-Beta` can actually accept the upgrade. `--header-env` reads the value from the environment at run time so the key does not have to appear on the command line; `--header "Authorization: Bearer $OPENAI_API_KEY"` also works via shell expansion.

This changes the run's semantics in two ways worth knowing:

- **No impairment queue.** `impair.Queue` exists to make the OFFLINE path's simulated chaos reproducible; a live run is already subject to whatever the real network and endpoint do, so nothing is layered on top. `dropped_frames` is therefore always `0` on a live report, and live metrics are not directly comparable to a baseline recorded from an offline `impair.Profile`.
- **No `FakeAgent`.** The endpoint drives its own turns; `TurnStart`/`TurnEnd` come from the codec decoding the endpoint's own events (e.g. OpenAI Realtime's `response.created`/`response.done`). A scripted barge-in still fires `into_ms` after the caller *observes* that real `TurnStart`, so the same `Script`/`BargeIn` semantics apply — there is just no synthetic turn-start to anchor on.
- **Timing is real, not virtual.** Script offsets (`at_ms`, `barge_in.into_ms`) are scheduled on elapsed wall-clock time (`clock.RealClock`) from when each session is primed, not on the offline path's virtual `ManualClock`. `max_duration_ms` bounds a live run's real duration; `0` means the run continues until its context is cancelled.

### Bounding a run with `--timeout`

`--timeout` takes a Go duration (`90s`, `5m`) and bounds the run's real duration:

```sh
voicechaos run scenario.json --endpoint wss://... --codec openai-realtime --timeout 90s --out report.json
```

How long a run may take is a property of where it runs, not of what it tests, so this belongs on the command line rather than in the scenario file. The same scenario is then usable from a laptop and from a CI job under different limits, and a scenario written for the offline path, where `max_duration_ms: 0` is harmless because virtual time ends when the script does, does not become an unkillable job the moment someone points it at `--endpoint`.

On a **live** run `--timeout` replaces `max_duration_ms`, which is the same kind of real-time bound, rather than the two being applied together. On the **offline** path `max_duration_ms` bounds *virtual* time and is a different thing entirely, so it stays and `--timeout` is a wall-clock safety net over it. With `--timeout` unset, nothing changes.

Hitting the bound is not a crash. The run stops, the report is still written, and it carries `"truncated": true` so nobody compares a partial run against a complete baseline. `report` prints the same warning.

Exit codes for `run`:

| Code | Meaning |
| --- | --- |
| 0 | Success. |
| 1 | The run failed. |
| 2 | Bad usage, including an unparseable or negative `--timeout`. |
| 3 | The run hit `--timeout`. The report was written and is marked truncated. |

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
  runner/                  bounded pool, cancel-before-wait, ownership-based leak counter; selects offline vs. live via Runner.Live
  engine/                  assembles + drives the deterministic offline pipeline (Run) and the real-endpoint live pipeline (RunLive)
```

## Determinism & race-safety

The whole default pipeline is deterministic and offline (injected clock + seeded RNG + loopback + `FakeAgent`), so a scenario replays to the identical event log and metrics. The suite runs under `go test -race`, with an **ownership-based** goroutine-leak assertion on the runner (an atomic lifecycle counter that returns to zero after `wg.Wait()`, not a brittle `NumGoroutine` check) and a byte-identical-replay test across concurrent runs.

## Roadmap

- **M1–M3 (done):** deterministic clock + impairment core, loopback + `FakeAgent`, scripted barge-ins, metrics, runner, scenario/baseline/CLI, the stdlib RFC6455 WebSocket transport with OpenAI-Realtime / Gemini-Live frame-mapping codecs, and the `--endpoint`/`--codec` CLI path that drives a scenario against a real endpoint with those codecs.
- **M4 (designed):** optional pion WebRTC adapter and real ElevenLabs/Deepgram TTS/STT adapters (build-tag seams already in place), a live dashboard, and a scenario recorder from a real call.

## License

[MIT](LICENSE) © 2026 Mykola Podpriatov
