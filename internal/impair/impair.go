// Package impair implements the deterministic transport impairment model: a
// constrained delivery QUEUE (not independent per-frame transforms) that drops,
// delays, jitters, reorders, and bandwidth-throttles frames reproducibly from a
// (seed, profile) pair.
//
// The model is the core value of voicechaos: given the same seed and profile,
// two implementations must produce the same delivery timestamps and the same
// drop sequence, so a CI baseline is stable rather than flaky. Two properties
// make that hold:
//
//   - Per-session RNG. Each Queue owns its own rand.New(rand.NewSource(seed +
//     sessionIndex)); there is never a shared *rand.Rand. Re-running session k
//     alone reproduces session k's exact loss/jitter/reorder trace, and two
//     sessions never perturb each other.
//
//   - Pinned composition order. For each sent frame the queue applies, strictly
//     in this order: (1) loss, (2) latency+jitter, (3) reorder, (4) bandwidth
//     backlog. The order is fixed so the event log is determined by inputs, not
//     by code structure.
//
// All delay is realized as a deliverAt timestamp computed at enqueue time from
// the injected clock; nothing sleeps on real time.
package impair

import (
	"math/rand"
	"sync"

	"voicechaos/internal/audio"
	"voicechaos/internal/transport"
)

// Profile parameterizes the impairment. Zero values mean "no impairment of that
// kind": a zero Profile delivers every frame immediately and unchanged.
type Profile struct {
	// AddedLatencyMs is a fixed one-way delay added to every delivered frame.
	AddedLatencyMs int `json:"added_latency_ms"`
	// JitterMs sets the jitter spread. The added jitter is drawn uniformly from
	// the NON-NEGATIVE range [0, 2*JitterMs] (same variance as ±JitterMs but
	// never a past timestamp), so deliverAt is never moved before now.
	JitterMs int `json:"jitter_ms"`
	// ReorderProb is the probability in [0,1] that a frame is given an extra
	// reorder delay so a later frame can overtake it.
	ReorderProb float64 `json:"reorder_prob"`
	// LossProb is the probability in [0,1] that a frame is dropped (never
	// delivered, recorded for the dropped-frames metric).
	LossProb float64 `json:"loss_prob"`
	// BandwidthBps is the link capacity in BITS per second used by the backlog
	// model. Zero means unlimited (no serialization delay / no backlog).
	BandwidthBps int `json:"bandwidth_bps"`
	// ReorderDelayMs is the extra delay applied when a frame is selected for
	// reordering. Default DefaultReorderDelayMs when zero.
	ReorderDelayMs int `json:"reorder_delay_ms"`
}

// DefaultReorderDelayMs is the extra delay added to a frame chosen for
// reordering when Profile.ReorderDelayMs is zero. It is large enough that the
// next frame at the default cadence can overtake the reordered one.
const DefaultReorderDelayMs = 40

// Queue is a stateful, per-session constrained delivery queue. It is NOT
// goroutine-safe on its own; the loopback transport calls Delay under the
// sending end's lock so each session's queue is accessed single-threaded. The
// dropped-frame count is published through an atomic-free counter guarded by
// the same discipline plus an internal mutex for out-of-band reads.
type Queue struct {
	profile Profile
	rng     *rand.Rand

	mu           sync.Mutex
	lastDelivery int64 // virtual ms of the last serialized delivery (bandwidth backlog)
	dropped      []audio.Frame
	hasLast      bool
}

// NewQueue builds a per-session impairment queue. The RNG is seeded with
// seed+sessionIndex so each session has an independent, reproducible trace.
func NewQueue(profile Profile, seed int64, sessionIndex int) *Queue {
	return &Queue{
		profile: profile,
		rng:     rand.New(rand.NewSource(seed + int64(sessionIndex))),
	}
}

// Delay implements transport.DelayFunc. It computes the virtual delivery
// timestamp for f sent at sendNow, applying the pinned composition order, and
// reports drop=true when the frame is lost. Loss/jitter/reorder draws advance
// the per-session RNG in a fixed sequence so the trace is reproducible.
func (q *Queue) Delay(f audio.Frame, sendNow int64) (deliverAt int64, drop bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	p := q.profile

	// (1) Loss: drop with LossProb. The RNG is consulted whenever loss is
	// enabled so the draw sequence is stable regardless of the outcome.
	if p.LossProb > 0 && q.rng.Float64() < p.LossProb {
		q.dropped = append(q.dropped, f)
		return 0, true
	}

	// (2) Latency + jitter: jitter is uniform on [0, 2*JitterMs].
	deliverAt = sendNow + int64(p.AddedLatencyMs)
	if p.JitterMs > 0 {
		deliverAt += int64(q.rng.Intn(2*p.JitterMs + 1))
	}
	// Floor: never deliver in the past.
	if deliverAt < sendNow {
		deliverAt = sendNow
	}

	// (3) Reorder: with ReorderProb add an extra bounded delay so a later frame
	// can overtake this one. Realized purely via deliverAt.
	if p.ReorderProb > 0 && q.rng.Float64() < p.ReorderProb {
		extra := p.ReorderDelayMs
		if extra == 0 {
			extra = DefaultReorderDelayMs
		}
		deliverAt += int64(extra)
	}
	// Re-assert the no-past-delivery floor AFTER reorder. A negative
	// ReorderDelayMs (rejected by validateProfile, but the Queue is constructible
	// directly in Go) would otherwise push deliverAt behind sendNow and feed the
	// shared min-heap scheduler a timestamp behind virtual time, breaking the
	// monotonic-clock invariant. For non-negative reorder delays this is a no-op,
	// so the byte-stable event log is unchanged.
	if deliverAt < sendNow {
		deliverAt = sendNow
	}

	// (4) Bandwidth backlog: serialization delay accumulates under saturation.
	// delay_ms = PayloadLen*8*1000 / BandwidthBps; the frame can only start
	// serializing once the previous one finished, so backlog (congestion)
	// builds rather than being an independent per-frame transform.
	if p.BandwidthBps > 0 && f.PayloadLen > 0 {
		delayMs := int64(f.PayloadLen) * 8 * 1000 / int64(p.BandwidthBps)
		start := deliverAt
		if q.hasLast && q.lastDelivery > start {
			start = q.lastDelivery
		}
		deliverAt = start + delayMs
		q.lastDelivery = deliverAt
		q.hasLast = true
	}

	return deliverAt, false
}

// Dropped returns a copy of the frames this queue has dropped so far, in drop
// order. It is safe to call concurrently with Delay.
func (q *Queue) Dropped() []audio.Frame {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]audio.Frame, len(q.dropped))
	copy(out, q.dropped)
	return out
}

// DroppedCount returns the number of frames dropped so far.
func (q *Queue) DroppedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.dropped)
}

// compile-time assertion that Delay satisfies the transport hook.
var _ transport.DelayFunc = (*Queue)(nil).Delay
