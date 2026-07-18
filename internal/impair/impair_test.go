package impair

import (
	"math/rand"
	"testing"
	"time"

	"voicechaos/internal/audio"
)

func frame(seq, ts int64, payload int) audio.Frame {
	return audio.Frame{Seq: seq, TS: ts, DurMs: 20, Kind: audio.KindAgent, PayloadLen: payload}
}

// runQueue applies the queue to n frames sent one per `step` ms starting at
// base, returning the deliverAt (or -1 for dropped) per frame.
func runQueue(q *Queue, n int, base, step int64, payload int) []int64 {
	out := make([]int64, n)
	for i := 0; i < n; i++ {
		now := base + int64(i)*step
		da, drop := q.Delay(frame(int64(i), now, payload), now)
		if drop {
			out[i] = -1
		} else {
			out[i] = da
		}
	}
	return out
}

// TestZeroProfileIsIdentity: a zero profile delivers every frame immediately and
// never drops.
func TestZeroProfileIsIdentity(t *testing.T) {
	q := NewQueue(Profile{}, 1, 0)
	for i := 0; i < 10; i++ {
		now := int64(1000 + i*20)
		da, drop := q.Delay(frame(int64(i), now, 160), now)
		if drop {
			t.Fatalf("frame %d dropped under zero profile", i)
		}
		if da != now {
			t.Fatalf("frame %d deliverAt=%d, want %d", i, da, now)
		}
	}
}

// TestSameSeedSameTrace: identical (seed, profile, inputs) yields an identical
// deliverAt/drop sequence.
func TestSameSeedSameTrace(t *testing.T) {
	p := Profile{AddedLatencyMs: 30, JitterMs: 10, ReorderProb: 0.2, LossProb: 0.1, BandwidthBps: 64000}
	a := runQueue(NewQueue(p, 99, 3), 200, 1000, 20, 160)
	b := runQueue(NewQueue(p, 99, 3), 200, 1000, 20, 160)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("frame %d: trace diverged a=%d b=%d", i, a[i], b[i])
		}
	}
}

// TestPerSessionRNGIsolation: re-running session k alone reproduces its exact
// trace, and a different session index produces a different (valid) trace.
func TestPerSessionRNGIsolation(t *testing.T) {
	p := Profile{AddedLatencyMs: 20, JitterMs: 10, ReorderProb: 0.3, LossProb: 0.2}
	const seed = 12345

	// Session 2 run standalone twice -> identical.
	s2a := runQueue(NewQueue(p, seed, 2), 300, 5000, 20, 100)
	s2b := runQueue(NewQueue(p, seed, 2), 300, 5000, 20, 100)
	for i := range s2a {
		if s2a[i] != s2b[i] {
			t.Fatalf("session 2 not reproducible at %d", i)
		}
	}
	// Different session index -> different trace (overwhelmingly likely).
	s3 := runQueue(NewQueue(p, seed, 3), 300, 5000, 20, 100)
	same := true
	for i := range s2a {
		if s2a[i] != s3[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("session 2 and 3 produced identical traces; per-session RNG not isolated")
	}
}

// TestDifferentSeedDifferentSequence: a different seed yields a different valid
// sequence.
func TestDifferentSeedDifferentSequence(t *testing.T) {
	p := Profile{JitterMs: 15, LossProb: 0.15}
	a := runQueue(NewQueue(p, 1, 0), 200, 0, 20, 100)
	b := runQueue(NewQueue(p, 2, 0), 200, 0, 20, 100)
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seeds produced identical traces")
	}
}

// TestLossRateApproximatesProb: empirical loss rate is close to LossProb.
func TestLossRateApproximatesProb(t *testing.T) {
	p := Profile{LossProb: 0.25}
	q := NewQueue(p, 42, 0)
	const n = 20000
	tr := runQueue(q, n, 0, 1, 100)
	dropped := 0
	for _, v := range tr {
		if v < 0 {
			dropped++
		}
	}
	rate := float64(dropped) / float64(n)
	if rate < 0.23 || rate > 0.27 {
		t.Fatalf("loss rate %.3f not near 0.25", rate)
	}
	if q.DroppedCount() != dropped {
		t.Fatalf("DroppedCount=%d, counted=%d", q.DroppedCount(), dropped)
	}
}

// TestLatencyJitterBounds: deliverAt is within [now+latency, now+latency+2*jitter]
// and never before now.
func TestLatencyJitterBounds(t *testing.T) {
	p := Profile{AddedLatencyMs: 50, JitterMs: 12}
	q := NewQueue(p, 7, 0)
	for i := 0; i < 5000; i++ {
		now := int64(1000 + i)
		da, drop := q.Delay(frame(int64(i), now, 0), now) // payload 0 => no bandwidth term
		if drop {
			t.Fatal("unexpected drop with LossProb=0")
		}
		lo := now + 50
		hi := now + 50 + 2*12
		if da < lo || da > hi {
			t.Fatalf("deliverAt %d out of [%d,%d]", da, lo, hi)
		}
		if da < now {
			t.Fatalf("deliverAt %d before now %d", da, now)
		}
	}
}

// TestJitterNonNegativeRange: jitter spans [0, 2*Jitter] (both extremes reachable
// over many draws) and is never negative.
func TestJitterNonNegativeRange(t *testing.T) {
	p := Profile{AddedLatencyMs: 0, JitterMs: 5}
	q := NewQueue(p, 3, 0)
	minOff, maxOff := int64(1<<62), int64(-1)
	for i := 0; i < 10000; i++ {
		now := int64(i)
		da, _ := q.Delay(frame(int64(i), now, 0), now)
		off := da - now
		if off < 0 {
			t.Fatalf("negative jitter offset %d", off)
		}
		if off < minOff {
			minOff = off
		}
		if off > maxOff {
			maxOff = off
		}
	}
	if minOff != 0 {
		t.Errorf("min jitter offset %d, want 0", minOff)
	}
	if maxOff != 10 {
		t.Errorf("max jitter offset %d, want 10 (2*Jitter)", maxOff)
	}
}

// TestBandwidthBacklogThrottles: under saturation, the backlog accumulates so the
// effective rate matches BandwidthBps (delay grows beyond the inter-arrival gap).
func TestBandwidthBacklogThrottles(t *testing.T) {
	// 8000 bps = 1000 bytes/sec. A 100-byte frame takes 100*8*1000/8000 = 100ms.
	p := Profile{BandwidthBps: 8000}
	q := NewQueue(p, 1, 0)
	// Send 10 frames 10ms apart (faster than the 100ms serialization) -> backlog.
	const n = 10
	out := runQueue(q, n, 0, 10, 100)
	// Each frame's serialization is 100ms; with a saturated link the k-th frame
	// (0-based) finishes at ~ (k+1)*100.
	for i := 0; i < n; i++ {
		want := int64((i + 1) * 100)
		if out[i] != want {
			t.Fatalf("frame %d deliverAt %d, want %d (backlog)", i, out[i], want)
		}
	}
	// Deliveries are monotonically increasing (queue serializes).
	for i := 1; i < n; i++ {
		if out[i] <= out[i-1] {
			t.Fatalf("backlog not monotonic at %d: %d <= %d", i, out[i], out[i-1])
		}
	}
}

// TestCompositionOrderLossBeforeBandwidth: a dropped frame must NOT advance the
// bandwidth backlog (loss is applied first, and a dropped frame never
// serializes). We verify by comparing the backlog with and without a forced
// early drop.
func TestCompositionOrderLossBeforeBandwidth(t *testing.T) {
	// With loss enabled, the RNG stream is: for each frame, one Float64 for loss,
	// then (if delivered) jitter draws etc. We assert that dropped frames are
	// recorded and that lastDelivery only advances for delivered frames by
	// checking the delivered frames' deliverAt equal the no-loss backlog of only
	// the delivered subset.
	p := Profile{LossProb: 0.5, BandwidthBps: 8000}
	q := NewQueue(p, 2024, 0)
	const n = 40
	out := runQueue(q, n, 0, 5, 100) // 100 bytes => 100ms serialization each

	// Reconstruct expected backlog over delivered frames only.
	var lastDelivery int64
	have := false
	for i := 0; i < n; i++ {
		if out[i] < 0 {
			continue // dropped: must not have advanced backlog
		}
		start := int64(0)
		if have && lastDelivery > start {
			start = lastDelivery
		}
		// The delivered frame's deliverAt must equal start+100 (no latency/jitter
		// in this profile), proving the backlog only counted delivered frames.
		want := start + 100
		if out[i] != want {
			t.Fatalf("delivered frame %d deliverAt %d, want %d (loss must precede bandwidth)", i, out[i], want)
		}
		lastDelivery = want
		have = true
	}
	if q.DroppedCount() == 0 {
		t.Fatal("expected some drops with LossProb=0.5")
	}
}

// TestBurstLossDisabledConsumesRNGLikeIID: with both burst transition probs zero
// the loss step must draw exactly one Float64 per frame — the plain i.i.d. model,
// unchanged. Reconstructing the drop pattern with an independent RNG seeded
// identically pins the byte-stable RNG consumption so committed baselines hold.
func TestBurstLossDisabledConsumesRNGLikeIID(t *testing.T) {
	const seed, sessionIndex = 999, 0
	p := Profile{LossProb: 0.3} // no burst knobs => disabled
	q := NewQueue(p, seed, sessionIndex)
	ref := rand.New(rand.NewSource(seed + int64(sessionIndex)))
	for i := 0; i < 5000; i++ {
		now := int64(i)
		_, drop := q.Delay(frame(int64(i), now, 0), now) // payload 0 => loss is the only draw
		if want := ref.Float64() < p.LossProb; drop != want {
			t.Fatalf("frame %d drop=%v, want %v (RNG consumption diverged from i.i.d.)", i, drop, want)
		}
	}
}

// TestBurstLossSameSeedSameTrace: the Gilbert-Elliott model replays byte-stably —
// identical (seed, profile, inputs) yields an identical drop/deliver trace — and
// actually drops frames.
func TestBurstLossSameSeedSameTrace(t *testing.T) {
	p := Profile{AddedLatencyMs: 20, JitterMs: 5, LossProb: 0.05, BurstLossToBadProb: 0.1, BurstLossToGoodProb: 0.4, BandwidthBps: 64000}
	a := runQueue(NewQueue(p, 77, 2), 500, 1000, 20, 160)
	b := runQueue(NewQueue(p, 77, 2), 500, 1000, 20, 160)
	drops := 0
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("burst-loss trace diverged at %d: a=%d b=%d", i, a[i], b[i])
		}
		if a[i] < 0 {
			drops++
		}
	}
	if drops == 0 {
		t.Fatal("expected some burst-loss drops with the correlated model")
	}
}

// TestBurstLossProducesBurst (burst shape): entering the bad state and never
// recovering yields a maximal run of consecutive drops that i.i.d. loss at
// LossProb=0 could never produce.
func TestBurstLossProducesBurst(t *testing.T) {
	p := Profile{LossProb: 0, BurstLossToBadProb: 1.0, BurstLossToGoodProb: 0.0}
	q := NewQueue(p, 1, 0)
	const n = 50
	tr := runQueue(q, n, 0, 20, 0)
	// Frame 0 is decided in the good state with LossProb 0 => delivered.
	if tr[0] < 0 {
		t.Fatal("frame 0 dropped; good-state LossProb=0 should deliver it")
	}
	// The transition after frame 0 forces the bad state; frames 1.. are a burst.
	for i := 1; i < n; i++ {
		if tr[i] != -1 {
			t.Fatalf("frame %d delivered; expected a loss burst (bad state)", i)
		}
	}
	if q.DroppedCount() != n-1 {
		t.Fatalf("dropped %d, want %d (burst)", q.DroppedCount(), n-1)
	}
}

// TestBurstLossRecoversToGood (burst shape): toBad=1 and toGood=1 make the chain
// alternate good<->bad every frame, so drops land on a strict every-other-frame
// pattern — proving the bad->good recovery transition fires and losses correlate
// with state rather than being independent.
func TestBurstLossRecoversToGood(t *testing.T) {
	p := Profile{LossProb: 0, BurstLossToBadProb: 1.0, BurstLossToGoodProb: 1.0}
	q := NewQueue(p, 1, 0)
	const n = 20
	tr := runQueue(q, n, 0, 20, 0)
	for i := 0; i < n; i++ {
		bad := i%2 == 1 // frame 0 good, then alternate
		if dropped := tr[i] == -1; dropped != bad {
			t.Fatalf("frame %d dropped=%v, want %v (alternating good/bad)", i, tr[i] == -1, bad)
		}
	}
}

// TestZeroWallClock: the entire impair test path uses no real time. This sentinel
// asserts a large simulated run completes effectively instantly (the formulas are
// pure arithmetic; there is no time.Sleep).
func TestZeroWallClock(t *testing.T) {
	p := Profile{AddedLatencyMs: 100, JitterMs: 50, ReorderProb: 0.5, LossProb: 0.1, BandwidthBps: 16000}
	q := NewQueue(p, 5, 0)
	start := time.Now()
	_ = runQueue(q, 100000, 0, 1, 200)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("impair simulation took %v of wall time; expected ~instant", elapsed)
	}
}

// TestDroppedRecordsFrames: dropped frames are recorded in drop order and match
// the count.
func TestDroppedRecordsFrames(t *testing.T) {
	p := Profile{LossProb: 1.0} // drop everything
	q := NewQueue(p, 1, 0)
	for i := 0; i < 5; i++ {
		_, drop := q.Delay(frame(int64(i), int64(i*20), 100), int64(i*20))
		if !drop {
			t.Fatalf("frame %d not dropped under LossProb=1", i)
		}
	}
	dropped := q.Dropped()
	if len(dropped) != 5 || q.DroppedCount() != 5 {
		t.Fatalf("dropped len=%d count=%d, want 5", len(dropped), q.DroppedCount())
	}
	for i, f := range dropped {
		if f.Seq != int64(i) {
			t.Fatalf("dropped[%d].Seq=%d, want %d (drop order)", i, f.Seq, i)
		}
	}
	// Returned slice is a copy: mutating it must not affect the queue.
	dropped[0].Seq = 999
	if q.Dropped()[0].Seq == 999 {
		t.Fatal("Dropped() did not return a defensive copy")
	}
}

// TestReorderAddsDelay: with ReorderProb=1 every frame gets the extra reorder
// delay, so deliverAt = now + latency + jitter + reorderDelay.
func TestReorderAddsDelay(t *testing.T) {
	p := Profile{AddedLatencyMs: 10, ReorderProb: 1.0, ReorderDelayMs: 40}
	q := NewQueue(p, 1, 0)
	for i := 0; i < 100; i++ {
		now := int64(i * 20)
		da, _ := q.Delay(frame(int64(i), now, 0), now)
		if da != now+10+40 {
			t.Fatalf("frame %d deliverAt %d, want %d", i, da, now+50)
		}
	}
}

// TestNegativeReorderDelayFlooredToSendNow: a Profile constructed directly in Go
// (bypassing scenario validation) with a negative ReorderDelayMs must never
// produce a deliverAt behind sendNow — the reorder step re-asserts the
// no-past-delivery floor so the scheduler's monotonic-clock invariant holds.
func TestNegativeReorderDelayFlooredToSendNow(t *testing.T) {
	p := Profile{ReorderProb: 1.0, ReorderDelayMs: -100}
	q := NewQueue(p, 1, 0)
	for i := 0; i < 100; i++ {
		now := int64(i * 20)
		da, drop := q.Delay(frame(int64(i), now, 0), now)
		if drop {
			t.Fatalf("frame %d unexpectedly dropped", i)
		}
		if da < now {
			t.Fatalf("frame %d deliverAt %d rewound behind sendNow %d", i, da, now)
		}
		// With no latency/jitter the floored value is exactly sendNow.
		if da != now {
			t.Fatalf("frame %d deliverAt %d, want %d (floored)", i, da, now)
		}
	}
}
