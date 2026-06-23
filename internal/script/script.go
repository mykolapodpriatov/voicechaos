// Package script defines the JSON-serializable Scenario and Script that
// describe a load+chaos run: how many synthetic callers, what each caller says
// and when it barges in, the network impairment profile, the seed, and limits.
//
// A Scenario is fully replayable: the same Scenario + seed produces the same
// event log and metrics on the offline loopback path. The script is expressed
// in virtual milliseconds so it is independent of wall-clock timing.
package script

import (
	"errors"
	"fmt"

	"voicechaos/internal/agentproto"
	"voicechaos/internal/audio"
	"voicechaos/internal/impair"
)

// BargeIn describes the caller interrupting the agent's reply. IntoMs is the
// number of milliseconds after the caller observes the agent's TurnStart at
// which the caller starts speaking over the agent. DurMs/PayloadLen size the
// interrupting speech burst.
type BargeIn struct {
	IntoMs     int `json:"into_ms"`
	DurMs      int `json:"dur_ms"`
	PayloadLen int `json:"payload_len"`
}

// CallerTurn is one prompt the caller speaks plus an optional barge-in into the
// agent's response to that prompt. AtMs is the virtual offset from session
// start at which the caller begins the prompt. DurMs/PayloadLen size the prompt
// speech.
type CallerTurn struct {
	AtMs       int      `json:"at_ms"`
	DurMs      int      `json:"dur_ms"`
	PayloadLen int      `json:"payload_len"`
	BargeIn    *BargeIn `json:"barge_in,omitempty"`
}

// AgentBehavior scripts the modeled agent's response shape so metric values are
// deterministic. It maps onto agentproto.FakeConfig.
type AgentBehavior struct {
	FramesPerTurn    int  `json:"frames_per_turn"`
	FrameMs          int  `json:"frame_ms"`
	PayloadLen       int  `json:"payload_len"`
	StopLatencyMs    int  `json:"stop_latency_ms"`
	IgnoreBargeIn    bool `json:"ignore_barge_in"`
	StallBeforeFrame int  `json:"stall_before_frame,omitempty"`
	StallMs          int  `json:"stall_ms,omitempty"`
	// EndpointMs is the silence gap after the caller's prompt at which the agent
	// begins responding. Defaults to FrameMs when zero.
	EndpointMs int `json:"endpoint_ms,omitempty"`
}

// FakeConfig converts the scenario's AgentBehavior into the agentproto config.
func (b AgentBehavior) FakeConfig() agentproto.FakeConfig {
	return agentproto.FakeConfig{
		FramesPerTurn:    b.FramesPerTurn,
		FrameMs:          b.FrameMs,
		PayloadLen:       b.PayloadLen,
		StopLatencyMs:    b.StopLatencyMs,
		IgnoreBargeIn:    b.IgnoreBargeIn,
		StallBeforeFrame: b.StallBeforeFrame,
		StallMs:          b.StallMs,
		EndpointMs:       b.EndpointMs,
	}
}

// Script is the per-caller conversation: an ordered list of caller turns.
type Script struct {
	Turns []CallerTurn `json:"turns"`
}

// Scenario is the complete, replayable run definition.
type Scenario struct {
	// Callers is the number of concurrent synthetic sessions N.
	Callers int `json:"callers"`
	// Seed seeds the per-session RNG (session k uses seed+k).
	Seed int64 `json:"seed"`
	// MaxDurationMs bounds a session's virtual duration; 0 means unbounded
	// (drained until no work remains).
	MaxDurationMs int `json:"max_duration_ms"`
	// StallThresholdMs is the gap above which a within-turn silence counts as a
	// stall. Defaults to DefaultStallThresholdMs when zero.
	StallThresholdMs int `json:"stall_threshold_ms"`
	// Profile is the transport impairment applied to every session (each gets
	// its own seeded queue).
	Profile impair.Profile `json:"profile"`
	// Agent scripts the modeled agent's behavior.
	Agent AgentBehavior `json:"agent"`
	// Script is the conversation every caller runs.
	Script Script `json:"script"`
}

// DefaultStallThresholdMs is the default stall threshold: three frame periods
// at the default cadence.
const DefaultStallThresholdMs = 3 * audio.DefaultFrameMs

// Validate checks the scenario for self-consistency, returning a descriptive
// error on the first problem found.
func (s *Scenario) Validate() error {
	if s.Callers <= 0 {
		return errors.New("scenario: callers must be > 0")
	}
	if len(s.Script.Turns) == 0 {
		return errors.New("scenario: script must have at least one turn")
	}
	if s.Agent.FramesPerTurn <= 0 {
		return errors.New("scenario: agent.frames_per_turn must be > 0")
	}
	if err := validateProfile(s.Profile); err != nil {
		return err
	}
	for i, t := range s.Script.Turns {
		if t.AtMs < 0 || t.DurMs < 0 {
			return fmt.Errorf("scenario: turn %d has negative timing", i)
		}
		if t.BargeIn != nil && t.BargeIn.IntoMs < 0 {
			return fmt.Errorf("scenario: turn %d barge_in.into_ms must be >= 0", i)
		}
	}
	return nil
}

func validateProfile(p impair.Profile) error {
	if p.LossProb < 0 || p.LossProb > 1 {
		return errors.New("scenario: profile.loss_prob must be in [0,1]")
	}
	if p.ReorderProb < 0 || p.ReorderProb > 1 {
		return errors.New("scenario: profile.reorder_prob must be in [0,1]")
	}
	if p.AddedLatencyMs < 0 || p.JitterMs < 0 || p.BandwidthBps < 0 {
		return errors.New("scenario: profile latency/jitter/bandwidth must be >= 0")
	}
	return nil
}

// StallThreshold returns the effective stall threshold, applying the default.
func (s *Scenario) StallThreshold() int {
	if s.StallThresholdMs > 0 {
		return s.StallThresholdMs
	}
	return DefaultStallThresholdMs
}
