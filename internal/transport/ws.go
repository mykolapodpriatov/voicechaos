package transport

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wsGUID is the RFC6455 GUID concatenated with the client key to derive the
// expected Sec-WebSocket-Accept.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// DefaultMaxMessageBytes bounds a reassembled message; larger messages are
// rejected with an error so a malicious/buggy server cannot exhaust memory.
const DefaultMaxMessageBytes = 1 << 20 // 1 MiB

// WS frame opcodes (RFC6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// Message is one reassembled WebSocket data message returned by WSConn.Read.
type Message struct {
	// Binary is true for a binary message, false for a text message.
	Binary bool
	// Data is the full (reassembled) payload.
	Data []byte
}

// WSConn is a minimal, hand-rolled RFC6455 WebSocket CLIENT over a single TCP
// (or TLS) connection. It is intentionally small but specified to a trustworthy
// level: client frames are masked with a fresh crypto/rand key per frame, Read
// reassembles continuation frames and transparently answers pings with pongs,
// oversized messages are rejected, and the close handshake and context-driven
// cancellation are handled so no goroutine or connection leaks.
//
// WSConn is safe for one concurrent reader and one concurrent writer (the usual
// WebSocket usage); it is not safe for multiple concurrent readers or writers.
type WSConn struct {
	conn   net.Conn
	br     *bufio.Reader
	maxMsg int

	wmu sync.Mutex // serializes writes (data frames + control frames)

	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
}

// DialWS dials rawURL (ws:// or wss://) and performs the RFC6455 opening
// handshake, returning a ready WSConn. ctx bounds the dial + handshake.
// maxMsgBytes <= 0 selects DefaultMaxMessageBytes.
func DialWS(ctx context.Context, rawURL string, maxMsgBytes int) (*WSConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ws: parse url: %w", err)
	}
	var useTLS bool
	switch u.Scheme {
	case "ws":
		useTLS = false
	case "wss":
		useTLS = true
	default:
		return nil, fmt.Errorf("ws: unsupported scheme %q", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		if useTLS {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	d := &net.Dialer{}
	rawConn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("ws: dial: %w", err)
	}
	conn := rawConn
	if useTLS {
		tconn := tls.Client(rawConn, &tls.Config{ServerName: u.Hostname()})
		if dl, ok := ctx.Deadline(); ok {
			_ = tconn.SetDeadline(dl)
		}
		if err := tconn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("ws: tls handshake: %w", err)
		}
		_ = tconn.SetDeadline(time.Time{})
		conn = tconn
	}

	if maxMsgBytes <= 0 {
		maxMsgBytes = DefaultMaxMessageBytes
	}
	c := &WSConn{conn: conn, br: bufio.NewReader(conn), maxMsg: maxMsgBytes}
	if err := c.handshake(ctx, u); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// handshake performs the client opening handshake and validates the server's
// Sec-WebSocket-Accept.
func (c *WSConn) handshake(ctx context.Context, u *url.URL) error {
	if dl, ok := ctx.Deadline(); ok {
		// Setting the deadline is what bounds the handshake. If it fails, the
		// blocking write+read below would run with NO timeout and could hang
		// forever, so fail fast rather than swallow it.
		if err := c.conn.SetDeadline(dl); err != nil {
			return fmt.Errorf("ws: set handshake deadline: %w", err)
		}
		// Clearing it can only fail on an already-broken conn, and every later
		// read/write would then fail with that same error anyway. Nothing useful
		// to report, and nothing to return it through from a defer.
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}

	keyBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, keyBytes); err != nil {
		return fmt.Errorf("ws: gen key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	reqPath := u.RequestURI()
	if reqPath == "" {
		reqPath = "/"
	}
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", reqPath)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	req.WriteString("\r\n")
	if _, err := c.conn.Write([]byte(req.String())); err != nil {
		return fmt.Errorf("ws: write handshake: %w", err)
	}

	resp, err := http.ReadResponse(c.br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return fmt.Errorf("ws: read handshake response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("ws: handshake failed: status %d", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return errors.New("ws: handshake missing Upgrade: websocket")
	}
	if !strings.EqualFold(resp.Header.Get("Connection"), "upgrade") {
		return errors.New("ws: handshake missing Connection: upgrade")
	}
	want := acceptKey(key)
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		return fmt.Errorf("ws: bad Sec-WebSocket-Accept: got %q want %q", got, want)
	}
	return nil
}

// AcceptKey computes the RFC6455 Sec-WebSocket-Accept value for a client key:
// base64(sha1(key + GUID)). Exported so a test/echo server can produce the same
// value.
func AcceptKey(clientKey string) string { return acceptKey(clientKey) }

func acceptKey(clientKey string) string {
	h := sha1.New()
	// hash.Hash's Write is documented never to return an error, so there is no
	// failure to handle here and no error path to plumb through acceptKey.
	_, _ = io.WriteString(h, clientKey+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Write sends data as a single final (FIN=1) masked frame. binary selects the
// binary opcode; otherwise a text frame is sent. ctx bounds the write via a
// write deadline.
func (c *WSConn) Write(ctx context.Context, binary bool, data []byte) error {
	op := byte(opText)
	if binary {
		op = opBinary
	}
	return c.writeFrame(ctx, op, data)
}

// writeFrame writes one complete masked client frame with FIN set.
func (c *WSConn) writeFrame(ctx context.Context, opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.isClosed() {
		return ErrClosed
	}
	if dl, ok := ctx.Deadline(); ok {
		// The deadline is the only thing bounding the two conn.Write calls below;
		// without it a stalled peer blocks this write (and the wmu it holds)
		// indefinitely. A failure to arm it must therefore surface, not vanish.
		if err := c.conn.SetWriteDeadline(dl); err != nil {
			return fmt.Errorf("ws: set write deadline: %w", err)
		}
		// Clearing only fails on an already-broken conn; see handshake.
		defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	}

	var header [14]byte
	header[0] = 0x80 | opcode // FIN + opcode
	n := 2
	pl := len(payload)
	switch {
	case pl <= 125:
		header[1] = 0x80 | byte(pl) // MASK bit + length
	case pl <= 0xFFFF:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(pl))
		n = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(pl))
		n = 10
	}

	// Fresh masking key per frame (crypto/rand) — a protocol requirement for
	// client frames.
	var mask [4]byte
	if _, err := io.ReadFull(rand.Reader, mask[:]); err != nil {
		return fmt.Errorf("ws: gen mask: %w", err)
	}
	copy(header[n:n+4], mask[:])
	n += 4

	if _, err := c.conn.Write(header[:n]); err != nil {
		return fmt.Errorf("ws: write frame header: %w", err)
	}
	if pl > 0 {
		masked := make([]byte, pl)
		for i := 0; i < pl; i++ {
			masked[i] = payload[i] ^ mask[i&3]
		}
		if _, err := c.conn.Write(masked); err != nil {
			return fmt.Errorf("ws: write frame payload: %w", err)
		}
	}
	return nil
}

// Read returns the next reassembled data message. It transparently reassembles
// continuation frames, answers pings with pongs, and returns a typed error on a
// server-initiated close (errServerClose). ctx cancellation unblocks a blocked
// read by setting a past read deadline so the read goroutine never leaks.
func (c *WSConn) Read(ctx context.Context) (Message, error) {
	// Wire ctx cancellation to a past read deadline so a blocked Read returns.
	// This runs on ctx's goroutine with no error path to return through; if it
	// fails the conn is already broken and the blocked read is failing anyway.
	stop := context.AfterFunc(ctx, func() {
		_ = c.conn.SetReadDeadline(time.Unix(0, 0))
	})
	defer stop()
	// Apply any ctx deadline up front, too. This is what bounds a read against a
	// silent peer, so a failure to arm it can leave Read blocked past the
	// caller's deadline — report it instead of dropping it.
	if dl, ok := ctx.Deadline(); ok {
		if err := c.conn.SetReadDeadline(dl); err != nil {
			return Message{}, fmt.Errorf("ws: set read deadline: %w", err)
		}
	}
	// Clearing only fails on an already-broken conn; see handshake.
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	var (
		buf      []byte
		dataOp   byte
		haveData bool
	)
	for {
		fr, err := c.readFrame()
		if err != nil {
			if ctx.Err() != nil {
				return Message{}, ctx.Err()
			}
			return Message{}, err
		}
		switch fr.opcode {
		case opPing:
			if err := c.writeFrame(ctx, opPong, fr.payload); err != nil {
				return Message{}, err
			}
		case opPong:
			// Ignore unsolicited pongs.
		case opClose:
			// Echo the close and surface a typed error.
			_ = c.sendCloseFrame(ctx, fr.payload)
			c.markClosed()
			return Message{}, errServerClose
		case opText, opBinary:
			if haveData {
				return Message{}, errors.New("ws: new data frame before previous message finished")
			}
			dataOp = fr.opcode
			haveData = true
			buf = append(buf, fr.payload...)
			if len(buf) > c.maxMsg {
				return Message{}, errMessageTooLarge
			}
			if fr.fin {
				return Message{Binary: dataOp == opBinary, Data: buf}, nil
			}
		case opContinuation:
			if !haveData {
				return Message{}, errors.New("ws: continuation frame without start")
			}
			buf = append(buf, fr.payload...)
			if len(buf) > c.maxMsg {
				return Message{}, errMessageTooLarge
			}
			if fr.fin {
				return Message{Binary: dataOp == opBinary, Data: buf}, nil
			}
		default:
			return Message{}, fmt.Errorf("ws: unknown opcode 0x%X", fr.opcode)
		}
	}
}

// errServerClose is returned by Read when the server initiates the close
// handshake.
var errServerClose = errors.New("ws: server closed connection")

// errMessageTooLarge is returned when a (reassembled) message exceeds the limit.
var errMessageTooLarge = errors.New("ws: message exceeds size limit")

// ErrServerClose reports whether err signals a server-initiated WebSocket close.
func ErrServerClose(err error) bool { return errors.Is(err, errServerClose) }

// ErrMessageTooLarge reports whether err signals an oversized message.
func ErrMessageTooLarge(err error) bool { return errors.Is(err, errMessageTooLarge) }

// frame is a single decoded WebSocket frame.
type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// readFrame reads and validates one frame. It rejects masked server frames
// (servers must not mask) and enforces the per-frame size limit.
func (c *WSConn) readFrame() (frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return frame{}, err
	}
	fin := h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return frame{}, errors.New("ws: reserved bits set")
	}
	opcode := h[0] & 0x0F
	masked := h[1]&0x80 != 0
	if masked {
		return frame{}, errors.New("ws: server frame must not be masked")
	}
	plen := int(h[1] & 0x7F)
	switch plen {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return frame{}, err
		}
		plen = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return frame{}, err
		}
		v := binary.BigEndian.Uint64(ext[:])
		if v > uint64(c.maxMsg) {
			return frame{}, errMessageTooLarge
		}
		plen = int(v)
	}
	if plen > c.maxMsg {
		return frame{}, errMessageTooLarge
	}
	// Control frames must be <=125 bytes and not fragmented (RFC6455 §5.5).
	if opcode >= 0x8 {
		if plen > 125 {
			return frame{}, errors.New("ws: control frame too large")
		}
		if !fin {
			return frame{}, errors.New("ws: fragmented control frame")
		}
	}
	payload := make([]byte, plen)
	if plen > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return frame{}, err
		}
	}
	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

// Close performs the client-initiated close handshake: it sends a Close frame,
// reads (with a short deadline) until the server's Close echo or the deadline,
// then closes the underlying connection. It is idempotent.
func (c *WSConn) Close() error {
	var retErr error
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// Send Close with a normal-closure status code (1000).
		_ = c.sendCloseFrame(ctx, closePayload(1000))
		c.markClosed()
		// Drain until the server's Close echo or the deadline, so the peer sees a
		// clean handshake; ignore errors (the conn is going away regardless).
		_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
		for {
			fr, err := c.readFrame()
			if err != nil {
				break
			}
			if fr.opcode == opClose {
				break
			}
		}
		retErr = c.conn.Close()
	})
	return retErr
}

// sendCloseFrame writes a Close control frame with the given payload.
func (c *WSConn) sendCloseFrame(ctx context.Context, payload []byte) error {
	return c.writeFrameAllowClosed(ctx, opClose, payload)
}

// writeFrameAllowClosed is writeFrame without the closed guard, used during the
// close handshake itself. It only ever carries control frames (Close/Pong),
// whose payload RFC6455 §5.5 caps at 125 bytes; it encodes the length in a
// single 7-bit field, so it rejects an over-cap payload rather than emit a
// malformed frame.
func (c *WSConn) writeFrameAllowClosed(ctx context.Context, opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	pl := len(payload)
	if pl > 125 {
		return fmt.Errorf("ws: control frame payload %d bytes exceeds RFC6455 limit of 125", pl)
	}
	if dl, ok := ctx.Deadline(); ok {
		// Same reasoning as writeFrame: the deadline is all that bounds the
		// writes below. Callers here (the close handshake) already choose to
		// ignore the returned error, but that is their decision to make — this
		// function should still report the failure rather than hide it.
		if err := c.conn.SetWriteDeadline(dl); err != nil {
			return fmt.Errorf("ws: set write deadline: %w", err)
		}
		// Clearing only fails on an already-broken conn; see handshake.
		defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	}
	var header [14]byte
	header[0] = 0x80 | opcode
	n := 2
	header[1] = 0x80 | byte(pl) // control frame payloads are always <=125
	var mask [4]byte
	if _, err := io.ReadFull(rand.Reader, mask[:]); err != nil {
		return err
	}
	copy(header[n:n+4], mask[:])
	n += 4
	if _, err := c.conn.Write(header[:n]); err != nil {
		return err
	}
	if pl > 0 {
		masked := make([]byte, pl)
		for i := 0; i < pl; i++ {
			masked[i] = payload[i] ^ mask[i&3]
		}
		if _, err := c.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

// closePayload builds a Close frame body carrying a 2-byte status code.
func closePayload(code uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, code)
	return b
}

func (c *WSConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *WSConn) markClosed() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}
