package transport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"voicechaos/internal/audio"
	"voicechaos/internal/clock"
)

func sframe(seq, ts int64) audio.Frame {
	return audio.Frame{Seq: seq, TS: ts, DurMs: 20, Kind: audio.KindAgent, PayloadLen: 100}
}

// driveTo runs the clock loop in a goroutine until ctx is cancelled, so blocked
// Recvs are served by scheduled deliveries.
func driveTo(ctx context.Context, mc *clock.ManualClock) (stop func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if !mc.Advance() {
				// Nothing scheduled right now; yield and re-check.
				time.Sleep(time.Millisecond)
			}
		}
	}()
	return func() { <-done }
}

// collectUntilClosed reads frames from tr into a slice until ErrClosed/cancel,
// re-parking on each Recv so the delivery rendezvous always completes.
func collectUntilClosed(ctx context.Context, tr Transport) (*[]int64, *sync.Mutex, *sync.WaitGroup) {
	got := &[]int64{}
	mu := &sync.Mutex{}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			f, err := tr.Recv(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			*got = append(*got, f.Seq)
			mu.Unlock()
		}
	}()
	return got, mu, wg
}

func waitFor(t *testing.T, mu *sync.Mutex, got *[]int64, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		c := len(*got)
		mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d frames", n)
}

// TestLoopbackDuplexDelivery: frames sent on each end arrive on the other end in
// order.
func TestLoopbackDuplexDelivery(t *testing.T) {
	mc := clock.NewManualClock(0)
	caller, agent := Loopback(mc, 0, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	wait := driveTo(ctx, mc)

	got, mu, wg := collectUntilClosed(ctx, agent)

	for i := 0; i < 3; i++ {
		if err := caller.Send(ctx, sframe(int64(i), int64(10+i*10))); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	waitFor(t, mu, got, 3)

	_ = caller.Close()
	_ = agent.Close()
	cancel()
	wait()
	wg.Wait()

	if len(*got) != 3 || (*got)[0] != 0 || (*got)[1] != 1 || (*got)[2] != 2 {
		t.Fatalf("received seqs %v, want [0 1 2]", *got)
	}
}

// TestLoopbackOrderingByDeliverAt: with a DelayFunc that reverses delivery times,
// frames arrive in deliverAt order, not send order.
func TestLoopbackOrderingByDeliverAt(t *testing.T) {
	mc := clock.NewManualClock(0)
	// Delay later-sent frames less so they overtake: deliverAt = 1000 - seq*100.
	delay := func(f audio.Frame, _ int64) (int64, bool) {
		return 1000 - f.Seq*100, false
	}
	caller, agent := Loopback(mc, 0, delay, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got, mu, wg := collectUntilClosed(ctx, agent)

	// Schedule all four deliveries BEFORE driving, so the heap holds all of them
	// and the driver pops them in deliverAt order (not send order).
	for i := 0; i < 4; i++ {
		_ = caller.Send(ctx, sframe(int64(i), 0))
	}
	// Drive synchronously from this goroutine until the consumer has all four.
	for {
		mu.Lock()
		c := len(*got)
		mu.Unlock()
		if c >= 4 {
			break
		}
		if !mc.Advance() {
			time.Sleep(time.Millisecond)
		}
	}

	_ = caller.Close()
	_ = agent.Close()
	wg.Wait()

	// Reversed: seq 3 (deliverAt 700) first ... seq 0 (deliverAt 1000) last.
	want := []int64{3, 2, 1, 0}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Fatalf("order %v, want %v", *got, want)
		}
	}
}

// TestLoopbackCloseUnblocksRecv: closing an end returns ErrClosed from a blocked
// Recv.
func TestLoopbackCloseUnblocksRecv(t *testing.T) {
	mc := clock.NewManualClock(0)
	caller, _ := Loopback(mc, 0, nil, nil)

	errc := make(chan error, 1)
	go func() {
		_, err := caller.Recv(context.Background())
		errc <- err
	}()
	time.Sleep(10 * time.Millisecond)
	_ = caller.Close()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Recv after close returned %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Recv did not unblock on Close")
	}
}

// TestLoopbackCtxCancelUnblocksRecv: cancelling ctx returns ctx.Err() from a
// blocked Recv.
func TestLoopbackCtxCancelUnblocksRecv(t *testing.T) {
	mc := clock.NewManualClock(0)
	caller, _ := Loopback(mc, 0, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := caller.Recv(ctx)
		errc <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Recv returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Recv did not unblock on ctx cancel")
	}
	_ = caller.Close()
}

// TestLoopbackSendAfterCloseErrors checks Send on a closed end errors.
func TestLoopbackSendAfterCloseErrors(t *testing.T) {
	mc := clock.NewManualClock(0)
	caller, _ := Loopback(mc, 0, nil, nil)
	_ = caller.Close()
	if err := caller.Send(context.Background(), sframe(0, 0)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after close returned %v, want ErrClosed", err)
	}
}

// TestLoopbackDeliverUnblocksOnCancelMidRendezvous reproduces the rendezvous
// deadlock: a deliver() that has handed a frame and is blocked waiting for the
// worker to consume-and-re-park must NOT hang forever when the worker's Recv
// instead returns ctx.Err() (cancellation) without re-parking. The worker
// consumes exactly one frame and then attempts a second Recv under a cancelled
// context; deliver must observe the cancellation and return. A hard timeout
// turns a regression (the old permanent hang) into a failure instead of a hung
// suite. Runs an end's deliver from a goroutine, mirroring the clock driver.
func TestLoopbackDeliverUnblocksOnCancelMidRendezvous(t *testing.T) {
	mc := clock.NewManualClock(0)
	// caller sends toward agentEnd; agentEnd is the receiving end whose Recv we
	// cancel mid-rendezvous.
	_, agentEnd := Loopback(mc, 0, nil, nil)
	ae := agentEnd.(*loopEnd)

	ctx, cancel := context.WithCancel(context.Background())

	consumed := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		// First Recv parks, then returns the handed frame.
		if _, err := agentEnd.Recv(ctx); err != nil {
			return
		}
		close(consumed) // frame consumed; do NOT re-park yet
		// Cancel before the second Recv so it returns ctx.Err() without parking,
		// exactly the situation that used to strand deliver's second wait.
		cancel()
		_, _ = agentEnd.Recv(ctx) // returns ctx.Err(), no re-park
	}()

	deliverDone := make(chan struct{})
	go func() {
		defer close(deliverDone)
		ae.deliver(sframe(1, 10))
	}()

	select {
	case <-consumed:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("worker never consumed the delivered frame")
	}

	select {
	case <-deliverDone:
		// deliver returned despite the cancelled, never-re-parking Recv: fixed.
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("deliver() did not unblock after Recv was cancelled mid-rendezvous (deadlock)")
	}
	<-workerDone

	// The end must still close cleanly with no goroutine left blocked.
	_ = agentEnd.Close()
}
