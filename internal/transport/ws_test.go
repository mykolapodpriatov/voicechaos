package transport

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"voicechaos/internal/audio"
	"voicechaos/internal/clock"
)

// --- hand-rolled stdlib WebSocket server (http.Hijacker, NOT gorilla) --------

// wsServerConn is a minimal server-side WS connection for tests: it completes
// the handshake on hijack and reads/writes RFC6455 frames directly.
type wsServerConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// upgrade performs the server side of the opening handshake over a hijacked
// conn. It validates the Upgrade headers and replies with the computed
// Sec-WebSocket-Accept.
func upgrade(w http.ResponseWriter, r *http.Request) (*wsServerConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return nil, errors.New("no upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return nil, errors.New("no key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("no hijacker")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + AcceptKey(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &wsServerConn{conn: conn, br: brw.Reader}, nil
}

// readFrame reads one client frame, unmasking the payload (client frames must be
// masked).
func (s *wsServerConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(s.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	opcode = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	plen := int(h[1] & 0x7F)
	switch plen {
	case 126:
		var e [2]byte
		if _, err = io.ReadFull(s.br, e[:]); err != nil {
			return
		}
		plen = int(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err = io.ReadFull(s.br, e[:]); err != nil {
			return
		}
		plen = int(binary.BigEndian.Uint64(e[:]))
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(s.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, plen)
	if _, err = io.ReadFull(s.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return
}

// writeFrame writes one UNMASKED server frame (servers must not mask).
func (s *wsServerConn) writeFrame(fin bool, opcode byte, payload []byte) error {
	var header [10]byte
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	header[0] = b0
	n := 2
	pl := len(payload)
	switch {
	case pl <= 125:
		header[1] = byte(pl)
	case pl <= 0xFFFF:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:4], uint16(pl))
		n = 4
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:10], uint64(pl))
		n = 10
	}
	if _, err := s.conn.Write(header[:n]); err != nil {
		return err
	}
	if pl > 0 {
		if _, err := s.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// echoServer returns an httptest.Server whose handler runs fn with the upgraded
// server conn. fn owns the conn lifecycle.
func wsTestServer(fn func(s *wsServerConn)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := upgrade(w, r)
		if err != nil {
			return
		}
		fn(s)
	}))
}

func wsURL(ts *httptest.Server) string {
	return "ws://" + strings.TrimPrefix(ts.URL, "http://")
}

// --- tests -------------------------------------------------------------------

// TestWSHandshakeAndEcho: a successful handshake (validated Accept) and a masked
// client frame echoed back.
func TestWSHandshakeAndEcho(t *testing.T) {
	ts := wsTestServer(func(s *wsServerConn) {
		for {
			fin, op, pl, err := s.readFrame()
			if err != nil {
				return
			}
			if op == opClose {
				_ = s.writeFrame(true, opClose, pl)
				return
			}
			_ = s.writeFrame(fin, op, pl) // echo
		}
	})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL(ts), 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.Write(ctx, true, []byte("hello world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	msg, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg.Data) != "hello world" || !msg.Binary {
		t.Fatalf("echo mismatch: %q binary=%v", msg.Data, msg.Binary)
	}
}

// TestWSBadAcceptRejected: a server returning a wrong Sec-WebSocket-Accept is
// rejected by the client.
func TestWSBadAcceptRejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: WRONG\r\n\r\n"))
		_ = conn.Close()
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := DialWS(ctx, wsURL(ts), 0); err == nil {
		t.Fatal("expected handshake rejection on bad Accept")
	}
}

// TestWSFragmentReassembly: the server sends a fragmented message (FIN=0 text +
// FIN=0 continuation + FIN=1 continuation); the client returns ONE Frame.
func TestWSFragmentReassembly(t *testing.T) {
	ts := wsTestServer(func(s *wsServerConn) {
		// Wait for the client's first frame (a trigger), then send fragments.
		if _, _, _, err := s.readFrame(); err != nil {
			return
		}
		_ = s.writeFrame(false, opText, []byte("Hel"))
		_ = s.writeFrame(false, opContinuation, []byte("lo, "))
		_ = s.writeFrame(true, opContinuation, []byte("world"))
		// Read the client's close and echo it.
		for {
			_, op, pl, err := s.readFrame()
			if err != nil {
				return
			}
			if op == opClose {
				_ = s.writeFrame(true, opClose, pl)
				return
			}
		}
	})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL(ts), 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.Write(ctx, false, []byte("go")) // trigger
	msg, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg.Data) != "Hello, world" {
		t.Fatalf("reassembled %q, want %q", msg.Data, "Hello, world")
	}
	if msg.Binary {
		t.Fatal("reassembled message should be text")
	}
}

// TestWSAutoPong: the server sends a ping; the client auto-replies with a pong
// carrying the same payload, then the server's subsequent echo is delivered.
func TestWSAutoPong(t *testing.T) {
	gotPong := make(chan []byte, 1)
	ts := wsTestServer(func(s *wsServerConn) {
		// Trigger frame from client first.
		if _, _, _, err := s.readFrame(); err != nil {
			return
		}
		_ = s.writeFrame(true, opPing, []byte("ping-1"))
		// Expect the client's pong.
		for {
			_, op, pl, err := s.readFrame()
			if err != nil {
				return
			}
			if op == opPong {
				gotPong <- pl
				_ = s.writeFrame(true, opText, []byte("after-pong"))
				continue
			}
			if op == opClose {
				_ = s.writeFrame(true, opClose, pl)
				return
			}
		}
	})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL(ts), 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.Write(ctx, false, []byte("go"))
	// Read should auto-handle the ping and return the "after-pong" data message.
	msg, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg.Data) != "after-pong" {
		t.Fatalf("got %q, want after-pong", msg.Data)
	}
	select {
	case pl := <-gotPong:
		if string(pl) != "ping-1" {
			t.Fatalf("pong payload %q, want ping-1", pl)
		}
	case <-time.After(time.Second):
		t.Fatal("server never received the auto-pong")
	}
}

// TestWSCloseHandshake: client Close -> server Close echo -> conn closed.
func TestWSCloseHandshake(t *testing.T) {
	sawClose := make(chan struct{}, 1)
	ts := wsTestServer(func(s *wsServerConn) {
		for {
			_, op, pl, err := s.readFrame()
			if err != nil {
				return
			}
			if op == opClose {
				sawClose <- struct{}{}
				_ = s.writeFrame(true, opClose, pl) // echo
				return
			}
		}
	})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL(ts), 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-sawClose:
	case <-time.After(time.Second):
		t.Fatal("server never received client Close frame")
	}
	// A second Close is idempotent.
	if err := conn.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestWSServerInitiatedClose: a server-sent Close surfaces as ErrServerClose.
func TestWSServerInitiatedClose(t *testing.T) {
	ts := wsTestServer(func(s *wsServerConn) {
		if _, _, _, err := s.readFrame(); err != nil {
			return
		}
		_ = s.writeFrame(true, opClose, closePayload(1000))
		// Read the client's echoed Close, then exit.
		_, _, _, _ = s.readFrame()
	})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL(ts), 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.Write(ctx, false, []byte("go"))
	if _, err := conn.Read(ctx); !ErrServerClose(err) {
		t.Fatalf("read returned %v, want ErrServerClose", err)
	}
}

// TestWSOversizedMessageRejected: a message exceeding the client's max is
// rejected with an error.
func TestWSOversizedMessageRejected(t *testing.T) {
	ts := wsTestServer(func(s *wsServerConn) {
		if _, _, _, err := s.readFrame(); err != nil {
			return
		}
		_ = s.writeFrame(true, opBinary, make([]byte, 4096))
		_, _, _, _ = s.readFrame()
	})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL(ts), 1024) // 1 KiB limit
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.Write(ctx, false, []byte("go"))
	if _, err := conn.Read(ctx); !ErrMessageTooLarge(err) {
		t.Fatalf("read returned %v, want ErrMessageTooLarge", err)
	}
}

// TestWSClientFramesAreMasked: the server asserts that every client frame it
// reads carries the MASK bit set (a protocol requirement).
func TestWSClientFramesAreMasked(t *testing.T) {
	masked := make(chan bool, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := upgrade(w, r)
		if err != nil {
			return
		}
		// Read raw header bytes to inspect the MASK bit directly.
		for {
			var h [2]byte
			if _, err := io.ReadFull(s.br, h[:]); err != nil {
				return
			}
			isMasked := h[1]&0x80 != 0
			plen := int(h[1] & 0x7F)
			masked <- isMasked
			switch plen {
			case 126:
				var e [2]byte
				_, _ = io.ReadFull(s.br, e[:])
				plen = int(binary.BigEndian.Uint16(e[:]))
			case 127:
				var e [8]byte
				_, _ = io.ReadFull(s.br, e[:])
				plen = int(binary.BigEndian.Uint64(e[:]))
			}
			if isMasked {
				var mask [4]byte
				_, _ = io.ReadFull(s.br, mask[:])
			}
			buf := make([]byte, plen)
			_, _ = io.ReadFull(s.br, buf)
			if h[0]&0x0F == opClose {
				return
			}
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL(ts), 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Write(ctx, true, []byte("a"))
	_ = conn.Write(ctx, true, []byte("bb"))
	_ = conn.Close()
	for i := 0; i < 2; i++ {
		select {
		case m := <-masked:
			if !m {
				t.Fatalf("client frame %d was not masked", i)
			}
		case <-time.After(time.Second):
			t.Fatal("did not observe client frame")
		}
	}
}

// TestWSCtxCancelUnblocksReadNoLeak: a blocked Read returns promptly when ctx is
// cancelled (via SetReadDeadline(past)), and the read goroutine exits — no leak.
func TestWSCtxCancelUnblocksReadNoLeak(t *testing.T) {
	ts := wsTestServer(func(s *wsServerConn) {
		// Read the trigger, then go silent (never send), holding the conn open.
		if _, _, _, err := s.readFrame(); err != nil {
			return
		}
		// Block until the client closes.
		for {
			_, op, _, err := s.readFrame()
			if err != nil {
				return
			}
			if op == opClose {
				return
			}
		}
	})
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, err := DialWS(dialCtx, wsURL(ts), 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.Write(context.Background(), false, []byte("go"))

	var wg sync.WaitGroup
	wg.Add(1)
	errc := make(chan error, 1)
	go func() {
		defer wg.Done()
		_, err := conn.Read(ctx) // will block (server is silent)
		errc <- err
	}()

	time.Sleep(50 * time.Millisecond) // ensure Read is blocked
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock on ctx cancel (goroutine leak)")
	}
	wg.Wait() // the read goroutine exited -> no leak
}

// TestWSControlFrameOversizedPayloadRejected: a control frame whose payload
// exceeds RFC6455 §5.5's 125-byte cap must be rejected with a clear error rather
// than silently mis-encoded (the single 7-bit length field would otherwise wrap
// and produce a malformed frame). 126 bytes is the smallest violating size.
func TestWSControlFrameOversizedPayloadRejected(t *testing.T) {
	// Drain the server side so that, even if the guard were missing and the
	// malformed frame got written, the write completes and the assertion fires
	// immediately (rather than blocking on an unread pipe).
	newConn := func(_ *testing.T) (*WSConn, func()) {
		srv, cli := net.Pipe()
		drained := make(chan struct{})
		go func() { defer close(drained); _, _ = io.Copy(io.Discard, srv) }()
		c := &WSConn{conn: cli, br: bufio.NewReader(cli), maxMsg: DefaultMaxMessageBytes}
		return c, func() { _ = cli.Close(); _ = srv.Close(); <-drained }
	}

	// 126 bytes is the smallest payload violating RFC6455 §5.5's 125-byte cap.
	c, cleanup := newConn(t)
	defer cleanup()
	err := c.writeFrameAllowClosed(context.Background(), opClose, make([]byte, 126))
	if err == nil {
		t.Fatal("oversized (126-byte) control-frame payload was accepted; want an error, not a malformed frame")
	}
	if !strings.Contains(err.Error(), "control frame payload") {
		t.Fatalf("unexpected error %q, want a control-frame size error", err)
	}

	// A 125-byte payload is exactly at the limit and must still encode cleanly.
	c2, cleanup2 := newConn(t)
	defer cleanup2()
	if err := c2.writeFrameAllowClosed(context.Background(), opClose, make([]byte, 125)); err != nil {
		t.Fatalf("125-byte control frame rejected, want accepted: %v", err)
	}
}

// TestWSTransportRoundTrip exercises the WSTransport + codec layer over the echo
// server: a caller frame is encoded, echoed, and decoded back into a frame.
func TestWSTransportRoundTrip(t *testing.T) {
	ts := wsTestServer(func(s *wsServerConn) {
		for {
			fin, op, pl, err := s.readFrame()
			if err != nil {
				return
			}
			if op == opClose {
				_ = s.writeFrame(true, opClose, pl)
				return
			}
			// Reply with an OpenAI-style response.audio.delta so the codec yields
			// a KindAgent frame.
			_ = fin
			_ = s.writeFrame(true, opText, []byte(`{"type":"response.audio.delta","delta":""}`))
		}
	})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL(ts), 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tr := NewWSTransport(conn, OpenAIRealtimeCodec{}, clock.RealClock{})
	defer tr.Close()
	if err := tr.Send(ctx, audio.Frame{Kind: audio.KindSpeech, PayloadLen: 16}); err != nil {
		t.Fatalf("send: %v", err)
	}
	f, err := tr.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Kind != audio.KindAgent {
		t.Fatalf("decoded kind %v, want agent", f.Kind)
	}
}
