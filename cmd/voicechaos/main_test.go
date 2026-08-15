package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voicechaos/internal/audio"
	"voicechaos/internal/eventlog"
	"voicechaos/internal/metrics"
	"voicechaos/internal/runner"
	"voicechaos/internal/script"
	"voicechaos/internal/transport"
)

const scenarioJSON = `{
  "callers": 3,
  "seed": 7,
  "stall_threshold_ms": 60,
  "profile": { "added_latency_ms": 30, "jitter_ms": 8, "loss_prob": 0.02, "bandwidth_bps": 64000 },
  "agent": { "frames_per_turn": 20, "frame_ms": 20, "payload_len": 160, "stop_latency_ms": 40, "endpoint_ms": 20 },
  "script": { "turns": [ { "at_ms": 0, "dur_ms": 60, "payload_len": 160, "barge_in": { "into_ms": 100, "dur_ms": 60, "payload_len": 160 } } ] }
}`

// devNull returns an *os.File for /dev/null and a cleanup; CLI output during
// tests is discarded.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func writeScenario(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scenario.json")
	if err := os.WriteFile(p, []byte(scenarioJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunReportHappyPath: `run --out` then `report` both succeed (exit 0).
func TestRunReportHappyPath(t *testing.T) {
	null := devNull(t)
	scenario := writeScenario(t)
	repPath := filepath.Join(t.TempDir(), "report.json")

	if code := run([]string{"run", scenario, "--out", repPath}, null, null); code != 0 {
		t.Fatalf("run exit %d, want 0", code)
	}
	if _, err := os.Stat(repPath); err != nil {
		t.Fatalf("report not written: %v", err)
	}
	if code := run([]string{"report", repPath}, null, null); code != 0 {
		t.Fatalf("report exit %d, want 0", code)
	}
}

// TestBaselineSaveThenCheckPasses: baseline save then check on the same scenario
// passes (exit 0) — determinism makes the gate stable.
func TestBaselineSaveThenCheckPasses(t *testing.T) {
	null := devNull(t)
	scenario := writeScenario(t)
	basePath := filepath.Join(t.TempDir(), "baseline.json")

	if code := run([]string{"baseline", "save", scenario, "--out", basePath}, null, null); code != 0 {
		t.Fatalf("baseline save exit %d, want 0", code)
	}
	if code := run([]string{"check", scenario, "--baseline", basePath}, null, null); code != 0 {
		t.Fatalf("check exit %d, want 0 (same scenario+seed)", code)
	}
}

// TestCheckFailsOnStrictBaseline: a check against an unrealistically strict
// baseline fails with exit 1.
func TestCheckFailsOnStrictBaseline(t *testing.T) {
	null := devNull(t)
	scenario := writeScenario(t)
	basePath := filepath.Join(t.TempDir(), "strict.json")
	strict := `{"callers":3,"seed":7,"aggregate":{"sessions":3,"time_to_stop_ms":{"count":3,"sum":3,"mean":1,"p50":1,"p95":1,"max":1},"double_talk_ms":{"count":3,"sum":3,"mean":1,"p50":1,"p95":1,"max":1},"stall_count":0,"stall_ms":0,"dropped_frames":0}}`
	if err := os.WriteFile(basePath, []byte(strict), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"check", scenario, "--baseline", basePath}, null, null); code != 1 {
		t.Fatalf("check exit %d, want 1 (regression)", code)
	}
}

// TestValidateValidScenario: `validate` on a well-formed scenario returns exit 0.
func TestValidateValidScenario(t *testing.T) {
	null := devNull(t)
	scenario := writeScenario(t)
	if code := run([]string{"validate", scenario}, null, null); code != 0 {
		t.Fatalf("validate valid exit %d, want 0", code)
	}
}

// TestValidateInvalidScenario: `validate` on an invalid scenario (zero callers)
// returns exit 1.
func TestValidateInvalidScenario(t *testing.T) {
	null := devNull(t)
	p := filepath.Join(t.TempDir(), "bad.json")
	bad := `{"callers":0,"seed":1,"profile":{},"agent":{"frames_per_turn":1},"script":{"turns":[{"at_ms":0}]}}`
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"validate", p}, null, null); code != 1 {
		t.Fatalf("validate invalid exit %d, want 1", code)
	}
}

// TestValidateMissingPath: `validate` with no scenario path returns exit 2.
func TestValidateMissingPath(t *testing.T) {
	null := devNull(t)
	if code := run([]string{"validate"}, null, null); code != 2 {
		t.Fatalf("validate missing-path exit %d, want 2", code)
	}
}

// writeReportJSON writes a minimal runner.Report for compare tests.
func writeReportJSON(t *testing.T, dir, name string, ttsP95 int64) string {
	t.Helper()
	rep := runner.Report{
		Callers: 1,
		Seed:    1,
		Aggregate: metrics.Aggregate{
			Sessions: 1,
			TimeToStop: metrics.Summary{
				Count: 1, Sum: ttsP95, Mean: ttsP95, P50: ttsP95, P95: ttsP95, Max: ttsP95,
			},
		},
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCompareIdenticalReportsPass: two copies of the same report exit 0.
func TestCompareIdenticalReportsPass(t *testing.T) {
	null := devNull(t)
	dir := t.TempDir()
	a := writeReportJSON(t, dir, "a.json", 100)
	b := writeReportJSON(t, dir, "b.json", 100)
	if code := run([]string{"compare", a, b}, null, null); code != 0 {
		t.Fatalf("compare identical exit %d, want 0", code)
	}
}

// TestCompareWorseTimeToStopFails: a candidate whose p95 time-to-stop is worse
// than the baseline report exits 1.
func TestCompareWorseTimeToStopFails(t *testing.T) {
	null := devNull(t)
	dir := t.TempDir()
	a := writeReportJSON(t, dir, "a.json", 100)
	b := writeReportJSON(t, dir, "b.json", 200)
	if code := run([]string{"compare", a, b}, null, null); code != 1 {
		t.Fatalf("compare worse-tts exit %d, want 1", code)
	}
}

// TestCompareTruncatedReportExits2: a truncated report.json is a usage/input
// error (exit 2), not a regression.
func TestCompareTruncatedReportExits2(t *testing.T) {
	null := devNull(t)
	dir := t.TempDir()
	a := writeReportJSON(t, dir, "a.json", 100)
	bad := filepath.Join(dir, "truncated.json")
	if err := os.WriteFile(bad, []byte(`{"callers":1,"seed":1`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"compare", a, bad}, null, null); code != 2 {
		t.Fatalf("compare truncated exit %d, want 2", code)
	}
}

// TestUnknownSubcommand returns exit 2.
func TestUnknownSubcommand(t *testing.T) {
	null := devNull(t)
	if code := run([]string{"frobnicate"}, null, null); code != 2 {
		t.Fatalf("unknown subcommand exit %d, want 2", code)
	}
}

// TestNoArgsUsage returns exit 2.
func TestNoArgsUsage(t *testing.T) {
	null := devNull(t)
	if code := run(nil, null, null); code != 2 {
		t.Fatalf("no-args exit %d, want 2", code)
	}
}

// TestRunMissingScenario returns a non-zero exit.
func TestRunMissingScenario(t *testing.T) {
	null := devNull(t)
	if code := run([]string{"run", filepath.Join(t.TempDir(), "absent.json")}, null, null); code == 0 {
		t.Fatal("expected non-zero exit for missing scenario")
	}
}

// --- live path: a local, in-process WS server stub (no real network) -------

// writeServerTextFrame writes an unmasked, single-frame text WebSocket message
// (servers must not mask), the minimum needed to push scripted
// OpenAI-Realtime-shaped events at a dialed client in tests.
func writeServerTextFrame(conn net.Conn, s string) error {
	data := []byte(s)
	var header []byte
	switch {
	case len(data) <= 125:
		header = []byte{0x81, byte(len(data))}
	case len(data) <= 0xFFFF:
		header = make([]byte, 4)
		header[0] = 0x81
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:], uint16(len(data)))
	default:
		header = make([]byte, 10)
		header[0] = 0x81
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:], uint64(len(data)))
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(data)
	return err
}

// liveWSServer starts an httptest.Server that performs the RFC6455 server
// handshake (via transport.AcceptKey, the same accept-key derivation the real
// client validates) and, on each connection, writes messages in order with a
// small pacing delay, then drains and discards whatever the client sends
// until it disconnects. It stands in for a real voice endpoint so the live
// path can be exercised in-process, with no real network.
func liveWSServer(t *testing.T, messages ...string) *httptest.Server {
	return liveWSServerOnRequest(t, nil, messages...)
}

// liveWSServerOnRequest is liveWSServer with an optional hook that sees the
// upgrade request (used to assert --header / --header-env reach the handshake).
func liveWSServerOnRequest(t *testing.T, onReq func(*http.Request), messages ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onReq != nil {
			onReq(r)
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + transport.AcceptKey(key) + "\r\n\r\n"
		if _, err := conn.Write([]byte(resp)); err != nil {
			_ = conn.Close()
			return
		}

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			_, _ = io.Copy(io.Discard, conn) // client frames are masked; the test only cares what the server pushes
		}()
		for _, msg := range messages {
			if err := writeServerTextFrame(conn, msg); err != nil {
				_ = conn.Close()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		<-drained // wait for the client to close before tearing down
		_ = conn.Close()
	}))
	t.Cleanup(ts.Close)
	return ts
}

// wsURLOf converts an httptest.Server's http:// URL to its ws:// equivalent.
func wsURLOf(ts *httptest.Server) string {
	return "ws://" + strings.TrimPrefix(ts.URL, "http://")
}

// liveScenario is a minimal, valid scenario for the live path: one caller, a
// single short prompt turn, and no impairment profile (irrelevant — the live
// path never applies impair.Queue).
func liveScenario(maxDurationMs int) *script.Scenario {
	return &script.Scenario{
		Callers:       1,
		Seed:          1,
		MaxDurationMs: maxDurationMs,
		Agent:         script.AgentBehavior{FramesPerTurn: 1},
		Script:        script.Script{Turns: []script.CallerTurn{{AtMs: 0, DurMs: 20, PayloadLen: 160}}},
	}
}

// TestLiveEndpointDecodesRealFrames drives Runner's live path (the same
// wiring cmdRun assembles from --endpoint/--codec) against the in-process WS
// stub and asserts the decoded event log matches what the stub scripted: one
// TurnStart, two received agent frames, one TurnEnd, and — since the live
// path never layers impair.Queue on a real connection — zero dropped frames.
func TestLiveEndpointDecodesRealFrames(t *testing.T) {
	ts := liveWSServer(t,
		`{"type":"response.created"}`,
		`{"type":"response.audio.delta","delta":"AAAA"}`,
		`{"type":"response.audio.delta","delta":"AAAA"}`,
		`{"type":"response.done"}`,
	)
	url := wsURLOf(ts)

	rn := &runner.Runner{Live: &runner.LiveConfig{
		Dial: func(ctx context.Context, _ int) (*transport.WSConn, error) {
			return transport.DialWS(ctx, url, 0, nil)
		},
		NewCodec: func() transport.FrameCodec { return transport.OpenAIRealtimeCodec{} },
	}}

	rep, err := rn.Run(context.Background(), liveScenario(150))
	if err != nil {
		t.Fatalf("live run: %v", err)
	}
	if len(rep.Logs) != 1 {
		t.Fatalf("want 1 session log, got %d", len(rep.Logs))
	}

	var turnStarts, turnEnds, agentFrames int
	for _, e := range rep.Logs[0].Events {
		switch {
		case e.Type == eventlog.EventTurnStart:
			turnStarts++
		case e.Type == eventlog.EventTurnEnd:
			turnEnds++
		case e.Type == eventlog.EventRecv && e.Frame.Kind == audio.KindAgent:
			agentFrames++
		case e.Type == eventlog.EventDrop:
			t.Fatalf("live path reported a drop event %+v; the live path must never apply impair.Queue", e)
		}
	}
	if turnStarts != 1 {
		t.Errorf("turn starts = %d, want 1", turnStarts)
	}
	if turnEnds != 1 {
		t.Errorf("turn ends = %d, want 1", turnEnds)
	}
	if agentFrames != 2 {
		t.Errorf("agent frames received = %d, want 2", agentFrames)
	}
	if rep.Aggregate.DroppedFrames != 0 {
		t.Errorf("aggregate dropped frames = %d, want 0", rep.Aggregate.DroppedFrames)
	}
}

// TestRunEndpointFlagWiring exercises `voicechaos run --endpoint --codec`
// through the CLI dispatcher: a valid pair drives a full live run to a
// written report, and each invalid combination is rejected with a clear
// error before any dial is attempted (so an unreachable endpoint URL is safe
// to use in the flag-validation cases).
func TestRunEndpointFlagWiring(t *testing.T) {
	null := devNull(t)
	scenario := liveScenario(120)
	data, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	scPath := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(scPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("valid endpoint and codec runs live and writes a report", func(t *testing.T) {
		ts := liveWSServer(t, `{"type":"response.created"}`, `{"type":"response.done"}`)
		repPath := filepath.Join(t.TempDir(), "report.json")
		code := run([]string{"run", scPath, "--endpoint", wsURLOf(ts), "--codec", "openai-realtime", "--out", repPath}, null, null)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		raw, err := os.ReadFile(repPath)
		if err != nil {
			t.Fatalf("report not written: %v", err)
		}
		var rep runner.Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("parse report: %v", err)
		}
		if rep.Callers != 1 || len(rep.Sessions) != 1 {
			t.Fatalf("callers=%d sessions=%d, want 1/1", rep.Callers, len(rep.Sessions))
		}
	})

	t.Run("endpoint without codec is rejected", func(t *testing.T) {
		code := run([]string{"run", scPath, "--endpoint", "ws://127.0.0.1:1"}, null, null)
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})

	t.Run("codec without endpoint is rejected", func(t *testing.T) {
		code := run([]string{"run", scPath, "--codec", "openai-realtime"}, null, null)
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})

	t.Run("unknown codec is rejected", func(t *testing.T) {
		code := run([]string{"run", scPath, "--endpoint", "ws://127.0.0.1:1", "--codec", "not-a-codec"}, null, null)
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})
}

// TestRunHeaderFlags: --header / --header-env land on the WebSocket upgrade
// request; malformed specs, an unset env var, and using either flag without
// --endpoint are rejected with exit 2.
func TestRunHeaderFlags(t *testing.T) {
	null := devNull(t)
	scenario := liveScenario(120)
	data, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	scPath := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(scPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("header is sent on the handshake", func(t *testing.T) {
		saw := make(chan http.Header, 1)
		ts := liveWSServerOnRequest(t, func(r *http.Request) { saw <- r.Header.Clone() },
			`{"type":"response.created"}`, `{"type":"response.done"}`)
		code := run([]string{
			"run", scPath,
			"--endpoint", wsURLOf(ts),
			"--codec", "openai-realtime",
			"--header", "Authorization: Bearer test-key",
			"--header", "OpenAI-Beta: realtime=v1",
		}, null, null)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		select {
		case got := <-saw:
			if got.Get("Authorization") != "Bearer test-key" {
				t.Fatalf("Authorization = %q, want Bearer test-key", got.Get("Authorization"))
			}
			if got.Get("OpenAI-Beta") != "realtime=v1" {
				t.Fatalf("OpenAI-Beta = %q, want realtime=v1", got.Get("OpenAI-Beta"))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("server never observed the handshake")
		}
	})

	t.Run("header-env reads the value from the environment", func(t *testing.T) {
		t.Setenv("VOICECHAOS_TEST_KEY", "Bearer from-env")
		saw := make(chan http.Header, 1)
		ts := liveWSServerOnRequest(t, func(r *http.Request) { saw <- r.Header.Clone() },
			`{"type":"response.created"}`, `{"type":"response.done"}`)
		code := run([]string{
			"run", scPath,
			"--endpoint", wsURLOf(ts),
			"--codec", "openai-realtime",
			"--header-env", "Authorization=VOICECHAOS_TEST_KEY",
		}, null, null)
		if code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		select {
		case got := <-saw:
			if got.Get("Authorization") != "Bearer from-env" {
				t.Fatalf("Authorization = %q, want Bearer from-env", got.Get("Authorization"))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("server never observed the handshake")
		}
	})

	t.Run("unset header-env is rejected", func(t *testing.T) {
		code := run([]string{
			"run", scPath,
			"--endpoint", "ws://127.0.0.1:1",
			"--codec", "openai-realtime",
			"--header-env", "Authorization=VOICECHAOS_TEST_UNSET",
		}, null, null)
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})

	t.Run("malformed header is rejected", func(t *testing.T) {
		code := run([]string{
			"run", scPath,
			"--endpoint", "ws://127.0.0.1:1",
			"--codec", "openai-realtime",
			"--header", "NotAHeader",
		}, null, null)
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})

	t.Run("header without endpoint is rejected", func(t *testing.T) {
		code := run([]string{"run", scPath, "--header", "Authorization: Bearer x"}, null, null)
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})
}
