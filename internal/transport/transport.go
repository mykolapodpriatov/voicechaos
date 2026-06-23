// Package transport defines the Transport interface that carries modeled audio
// frames between a synthetic caller and a voice agent, plus a deterministic
// in-memory loopback implementation. The stdlib RFC6455 WebSocket client lives
// in this package too (ws.go) for the real-endpoint path.
package transport

import (
	"context"
	"errors"

	"voicechaos/internal/audio"
)

// ErrClosed is returned by Send/Recv once the transport has been closed.
var ErrClosed = errors.New("transport: closed")

// Transport is a bidirectional, frame-oriented channel. Implementations carry
// audio.Frame values in order (subject to any impairment layered on top) and
// honor context cancellation on both Send and Recv.
type Transport interface {
	// Send transmits one frame. It returns ctx.Err() if ctx is cancelled
	// before the frame is accepted, or ErrClosed if the transport is closed.
	Send(ctx context.Context, f audio.Frame) error
	// Recv blocks until a frame is available, returning it. It returns
	// ctx.Err() on cancellation or ErrClosed once the transport is closed and
	// drained.
	Recv(ctx context.Context) (audio.Frame, error)
	// Close shuts down the transport. It is idempotent and unblocks any
	// in-flight Send/Recv with ErrClosed.
	Close() error
}
