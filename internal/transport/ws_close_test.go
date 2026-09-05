package transport

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// deadlineIgnoringConn drops SetReadDeadline. It stands in for the real race
// this guards against: Read clears the connection's read deadline as it
// returns, so a deadline Close armed a moment earlier can be gone by the time
// Close reads. Under this conn, Close never has a deadline in force, which is
// exactly the state the hang happened in.
type deadlineIgnoringConn struct {
	net.Conn
}

func (deadlineIgnoringConn) SetReadDeadline(time.Time) error { return nil }

// silentPeer returns a WSConn whose peer reads everything and answers nothing,
// plus a cleanup. A real endpoint that never echoes Close behaves this way.
func silentPeer(t *testing.T, wrap func(net.Conn) net.Conn) *WSConn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, server)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	conn := wrap(client)
	return &WSConn{conn: conn, br: bufio.NewReader(conn), maxMsg: DefaultMaxMessageBytes}
}

// Close must return even with no read deadline in force. Before the drain was
// bounded by its own timer, this blocked forever.
func TestCloseReturnsWithoutAReadDeadline(t *testing.T) {
	c := silentPeer(t, func(conn net.Conn) net.Conn { return deadlineIgnoringConn{conn} })

	done := make(chan error, 1)
	go func() { done <- c.Close() }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked with no read deadline in force; the drain is unbounded again")
	}
}

// The bound is the drain timeout, not something an order of magnitude larger.
func TestCloseGivesUpOnTheDrainTimeout(t *testing.T) {
	c := silentPeer(t, func(conn net.Conn) net.Conn { return deadlineIgnoringConn{conn} })

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Close() }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked past the drain timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*closeDrainTimeout {
		t.Errorf("Close took %s, want roughly the %s drain timeout", elapsed, closeDrainTimeout)
	}
}

// A peer that does echo Close lets Close return without waiting out the timer.
func TestCloseReturnsPromptlyOnACloseEcho(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })

	echoed := make(chan struct{})
	go func() {
		defer close(echoed)
		// Read the client's masked Close frame (2-byte header, 4-byte mask,
		// 2-byte status payload), then answer with an unmasked server Close.
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(server, hdr); err != nil {
			return
		}
		_, _ = server.Write([]byte{0x88, 0x00})
	}()

	c := &WSConn{conn: client, br: bufio.NewReader(client), maxMsg: DefaultMaxMessageBytes}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Close() }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked despite a Close echo")
	}
	if elapsed := time.Since(start); elapsed >= closeDrainTimeout {
		t.Errorf("Close took %s; a Close echo should end the drain well before the %s timeout", elapsed, closeDrainTimeout)
	}
	<-echoed
}

// Close stays idempotent: the second call must not re-run the handshake.
func TestCloseIsIdempotentUnderTheBoundedDrain(t *testing.T) {
	c := silentPeer(t, func(conn net.Conn) net.Conn { return deadlineIgnoringConn{conn} })

	_ = c.Close()
	start := time.Now()
	_ = c.Close()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("the second Close took %s; it should be a no-op", elapsed)
	}
}

// Close must not read the connection while a Read owns it: both consume the
// same bufio.Reader. This drives the two concurrently and relies on -race,
// which is how the original report surfaced.
func TestCloseDoesNotReadWhileAReaderOwnsTheConnection(t *testing.T) {
	// The peer reads (so the Close frame write completes) and answers nothing,
	// so the reader stays parked inside its frame loop holding the reader.
	c := silentPeer(t, func(conn net.Conn) net.Conn { return conn })

	reading := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		close(reading)
		_, _ = c.Read(context.Background())
	}()
	<-reading
	// Give the reader a moment to actually enter the frame loop, so the two are
	// genuinely overlapping rather than merely started.
	time.Sleep(50 * time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked while a reader owned the connection")
	}

	select {
	case <-readDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the parked Read never returned after Close")
	}
}

// Closing while a reader is parked must be prompt: Close hands the drain over
// rather than waiting out its timer behind a lock it cannot get.
func TestCloseIsPromptWhileAReaderOwnsTheConnection(t *testing.T) {
	c := silentPeer(t, func(conn net.Conn) net.Conn { return conn })

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = c.Read(context.Background())
	}()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	_ = c.Close()
	if elapsed := time.Since(start); elapsed >= closeDrainTimeout {
		t.Errorf("Close took %s; with a reader parked it should skip the drain, not wait out the %s timer", elapsed, closeDrainTimeout)
	}
	<-readDone
}
