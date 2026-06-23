package transport

import (
	"encoding/base64"
	"encoding/json"

	"voicechaos/internal/audio"
)

// padPayload returns base64 of payloadLen zero bytes, a deterministic stand-in
// for real PCM that preserves the size proxy used by the bandwidth model.
func padPayload(n int) string {
	if n <= 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(make([]byte, n))
}

// OpenAIRealtimeCodec maps modeled frames onto the OpenAI Realtime API's
// WebSocket event JSON. Caller speech becomes input_audio_buffer.append; on the
// receive side, response.created -> TurnStart, response.audio.delta ->
// KindAgent, response.done -> TurnEnd. It is a faithful-shape mapping helper for
// the M3 real-endpoint path (the audio bytes themselves are modeled).
type OpenAIRealtimeCodec struct{}

type oaiEvent struct {
	Type  string `json:"type"`
	Audio string `json:"audio,omitempty"`
	Delta string `json:"delta,omitempty"`
}

// EncodeSend encodes a caller speech frame as an input_audio_buffer.append
// event (text JSON).
func (OpenAIRealtimeCodec) EncodeSend(f audio.Frame) (bool, []byte, error) {
	ev := oaiEvent{Type: "input_audio_buffer.append", Audio: padPayload(f.PayloadLen)}
	data, err := json.Marshal(ev)
	return false, data, err
}

// DecodeRecv maps an OpenAI Realtime event to modeled frames.
func (OpenAIRealtimeCodec) DecodeRecv(msg Message, recvTSms int64) ([]audio.Frame, error) {
	var ev oaiEvent
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		return nil, err
	}
	switch ev.Type {
	case "response.created":
		return []audio.Frame{{TS: recvTSms, Kind: audio.KindTurnStart}}, nil
	case "response.audio.delta":
		n := len(decodeB64(ev.Delta))
		return []audio.Frame{{TS: recvTSms, DurMs: audio.DefaultFrameMs, Kind: audio.KindAgent, PayloadLen: n}}, nil
	case "response.done":
		return []audio.Frame{{TS: recvTSms, Kind: audio.KindTurnEnd}}, nil
	default:
		return nil, nil
	}
}

// GeminiLiveCodec maps modeled frames onto the Gemini Live API's bidiGenerate
// JSON: caller speech -> realtimeInput; serverContent with modelTurn ->
// KindAgent, turnComplete -> TurnEnd. A synthesized TurnStart is emitted on the
// first model audio of a turn.
type GeminiLiveCodec struct {
	talking bool
}

type geminiServerMsg struct {
	ServerContent *struct {
		ModelTurn *struct {
			Parts []struct {
				InlineData *struct {
					Data string `json:"data"`
				} `json:"inlineData,omitempty"`
			} `json:"parts"`
		} `json:"modelTurn,omitempty"`
		TurnComplete bool `json:"turnComplete,omitempty"`
	} `json:"serverContent,omitempty"`
}

// EncodeSend encodes a caller speech frame as a Gemini realtimeInput message.
func (GeminiLiveCodec) EncodeSend(f audio.Frame) (bool, []byte, error) {
	type inline struct {
		Data string `json:"data"`
	}
	msg := map[string]any{
		"realtimeInput": map[string]any{
			"mediaChunks": []inline{{Data: padPayload(f.PayloadLen)}},
		},
	}
	data, err := json.Marshal(msg)
	return false, data, err
}

// DecodeRecv maps a Gemini Live server message to modeled frames. It is a
// pointer receiver because it tracks turn state across messages.
func (c *GeminiLiveCodec) DecodeRecv(msg Message, recvTSms int64) ([]audio.Frame, error) {
	var m geminiServerMsg
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		return nil, err
	}
	if m.ServerContent == nil {
		return nil, nil
	}
	var out []audio.Frame
	if mt := m.ServerContent.ModelTurn; mt != nil {
		for _, p := range mt.Parts {
			if p.InlineData == nil {
				continue
			}
			if !c.talking {
				c.talking = true
				out = append(out, audio.Frame{TS: recvTSms, Kind: audio.KindTurnStart})
			}
			n := len(decodeB64(p.InlineData.Data))
			out = append(out, audio.Frame{TS: recvTSms, DurMs: audio.DefaultFrameMs, Kind: audio.KindAgent, PayloadLen: n})
		}
	}
	if m.ServerContent.TurnComplete {
		out = append(out, audio.Frame{TS: recvTSms, Kind: audio.KindTurnEnd})
		c.talking = false
	}
	return out, nil
}

func decodeB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

var (
	_ FrameCodec = OpenAIRealtimeCodec{}
	_ FrameCodec = (*GeminiLiveCodec)(nil)
)
