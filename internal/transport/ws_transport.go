package transport

import (
	"context"
	"sync"

	"voicechaos/internal/audio"
	"voicechaos/internal/clock"
)

// FrameCodec maps between modeled audio frames and an endpoint's wire protocol.
// EncodeSend turns a caller frame into one outbound WebSocket message (binary or
// text). DecodeRecv turns one inbound message into zero or more modeled frames
// (an audio delta becomes a KindAgent frame; a response-start/-done control
// message becomes a KindTurnStart/KindTurnEnd frame). Returning no frames is
// valid (the message is irrelevant to metrics).
type FrameCodec interface {
	EncodeSend(f audio.Frame) (binary bool, data []byte, err error)
	DecodeRecv(msg Message, recvTSms int64) ([]audio.Frame, error)
}

// WSTransport adapts a WSConn to the Transport interface using a FrameCodec, so
// a Session can run against a real endpoint exactly as it runs against the
// loopback. Receive timestamps come from the injected clock (the real clock for
// live runs). Recv reassembles control frames (TurnStart/TurnEnd) so the same
// metrics are computed from observed timestamps.
type WSTransport struct {
	conn  *WSConn
	codec FrameCodec
	clk   clock.Clock

	mu      sync.Mutex
	pending []audio.Frame // decoded frames not yet returned by Recv
}

// NewWSTransport wraps conn with codec, stamping receive times from clk (use a
// RealClock for live runs).
func NewWSTransport(conn *WSConn, codec FrameCodec, clk clock.Clock) *WSTransport {
	return &WSTransport{conn: conn, codec: codec, clk: clk}
}

// Send encodes f and writes it as a single WebSocket message.
func (t *WSTransport) Send(ctx context.Context, f audio.Frame) error {
	binary, data, err := t.codec.EncodeSend(f)
	if err != nil {
		return err
	}
	return t.conn.Write(ctx, binary, data)
}

// Recv returns the next decoded frame, reading and decoding further WebSocket
// messages as needed. A server-initiated close surfaces as ErrClosed.
func (t *WSTransport) Recv(ctx context.Context) (audio.Frame, error) {
	t.mu.Lock()
	if len(t.pending) > 0 {
		f := t.pending[0]
		t.pending = t.pending[1:]
		t.mu.Unlock()
		return f, nil
	}
	t.mu.Unlock()

	for {
		msg, err := t.conn.Read(ctx)
		if err != nil {
			if ErrServerClose(err) {
				return audio.Frame{}, ErrClosed
			}
			return audio.Frame{}, err
		}
		frames, derr := t.codec.DecodeRecv(msg, t.clk.NowMs())
		if derr != nil {
			return audio.Frame{}, derr
		}
		if len(frames) == 0 {
			continue
		}
		t.mu.Lock()
		t.pending = append(t.pending, frames...)
		f := t.pending[0]
		t.pending = t.pending[1:]
		t.mu.Unlock()
		return f, nil
	}
}

// Close runs the WebSocket close handshake and tears down the connection.
func (t *WSTransport) Close() error { return t.conn.Close() }

var _ Transport = (*WSTransport)(nil)
