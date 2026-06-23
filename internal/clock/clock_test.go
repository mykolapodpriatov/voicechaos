package clock

import (
	"sync"
	"testing"
)

// TestManualClockOrdersByDeliverAtSeqSession verifies the global event loop pops
// events in the (deliverAt, seq, sessionIndex) total order.
func TestManualClockOrdersByDeliverAtSeqSession(t *testing.T) {
	mc := NewManualClock(0)
	var fired []string
	add := func(at, seq int64, sess int, tag string) {
		mc.Schedule(at, seq, sess, func() { fired = append(fired, tag) })
	}
	// Intentionally schedule out of order.
	add(30, 0, 0, "c")
	add(10, 5, 0, "a2")
	add(10, 5, 1, "a3") // same (deliverAt,seq), higher session -> after a2
	add(10, 1, 0, "a1") // same deliverAt, lower seq -> first
	add(20, 0, 0, "b")

	mc.AdvanceUntilEmpty()
	want := []string{"a1", "a2", "a3", "b", "c"}
	if len(fired) != len(want) {
		t.Fatalf("fired %v, want %v", fired, want)
	}
	for i := range want {
		if fired[i] != want[i] {
			t.Fatalf("order[%d]=%s, want %s (full %v)", i, fired[i], want[i], fired)
		}
	}
}

// TestManualClockAdvancesTime verifies Now tracks the fired event's time and
// never goes backward.
func TestManualClockAdvancesTime(t *testing.T) {
	mc := NewManualClock(1000)
	if mc.NowMs() != 1000 {
		t.Fatalf("base now=%d", mc.NowMs())
	}
	var times []int64
	mc.Schedule(1500, 0, 0, func() { times = append(times, mc.NowMs()) })
	mc.Schedule(1200, 0, 0, func() { times = append(times, mc.NowMs()) })
	mc.Schedule(900, 0, 0, func() { times = append(times, mc.NowMs()) }) // past -> clamped to 1000
	mc.AdvanceUntilEmpty()
	want := []int64{1000, 1200, 1500}
	for i := range want {
		if times[i] != want[i] {
			t.Fatalf("times[%d]=%d, want %d (%v)", i, times[i], want[i], times)
		}
	}
}

// TestAdvanceReturnsFalseWhenEmpty checks the empty-heap signal.
func TestAdvanceReturnsFalseWhenEmpty(t *testing.T) {
	mc := NewManualClock(0)
	if mc.Advance() {
		t.Fatal("Advance on empty heap returned true")
	}
	mc.Schedule(5, 0, 0, func() {})
	if !mc.Advance() {
		t.Fatal("Advance with one event returned false")
	}
	if mc.Advance() {
		t.Fatal("Advance after draining returned true")
	}
}

// TestReentrantScheduling verifies a callback may schedule more work, which the
// loop drains.
func TestReentrantScheduling(t *testing.T) {
	mc := NewManualClock(0)
	count := 0
	var schedule func(at int64)
	schedule = func(at int64) {
		mc.Schedule(at, 0, 0, func() {
			count++
			if count < 5 {
				schedule(at + 10)
			}
		})
	}
	schedule(10)
	n := mc.AdvanceUntilEmpty()
	if count != 5 || n != 5 {
		t.Fatalf("count=%d n=%d, want 5/5", count, n)
	}
}

// TestConcurrentScheduleSafe runs Schedule from many goroutines and asserts all
// events fire (race detector validates safety).
func TestConcurrentScheduleSafe(t *testing.T) {
	mc := NewManualClock(0)
	var got int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	const goroutines, each = 16, 50
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				mc.Schedule(int64(i), int64(g), g, func() {
					mu.Lock()
					got++
					mu.Unlock()
				})
			}
		}(g)
	}
	wg.Wait()
	mc.AdvanceUntilEmpty()
	if got != goroutines*each {
		t.Fatalf("fired %d events, want %d", got, goroutines*each)
	}
}
