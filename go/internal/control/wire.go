package control

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes bounds one control frame in both directions, matching the
// mod's default budget.
const MaxFrameBytes = 16 * 1024 * 1024

// Frame types.
const (
	FrameHello   = "hello"
	FrameWelcome = "welcome"
	FrameRequest = "request"
	FrameReply   = "reply"
	FrameEvent   = "event"
	FrameError   = "error"
	FramePing    = "ping"
)

// Hello is the first client frame after the TLS handshake.
type Hello struct {
	Type     string     `json:"type"`
	Protocol int        `json:"protocol"`
	Token    string     `json:"token"`
	LastSeq  int64      `json:"lastSeq"`
	Client   ClientInfo `json:"client"`
}

// ClientInfo identifies the daemon to the server (never used for auth).
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Replay describes what a reconnecting client missed.
type Replay struct {
	RequestedSince      int64 `json:"requestedSince"`
	From                int64 `json:"from"`
	To                  int64 `json:"to"`
	Lost                bool  `json:"lost"`
	BufferedEvents      int   `json:"bufferedEvents"`
	PersistedAcrossRuns bool  `json:"persistedAcrossRuns"`
}

// Welcome is the server's answer to hello.
type Welcome struct {
	Type              string          `json:"type"`
	Protocol          int             `json:"protocol"`
	Mod               string          `json:"mod"`
	ModVersion        string          `json:"modVersion"`
	Minecraft         string          `json:"minecraft"`
	InstanceID        string          `json:"instanceId"`
	RunID             string          `json:"runId"`
	RunStartedAtMilli int64           `json:"runStartedAtMillis"`
	SessionID         string          `json:"sessionId"`
	Transport         string          `json:"transport"`
	ServerTimeMillis  int64           `json:"serverTimeMillis"`
	Permissions       []string        `json:"permissions"`
	Capabilities      []string        `json:"capabilities"`
	Replay            Replay          `json:"replay"`
	Limits            json.RawMessage `json:"limits"`
	Raw               json.RawMessage `json:"-"`
}

// Event is one sequenced server push.
type Event struct {
	Seq      int64
	RunID    string
	StreamID string
	Replay   bool
	Payload  json.RawMessage
}

// Map returns the inner event object decoded for convenience.
func (e Event) Map() map[string]any {
	var value map[string]any
	if len(e.Payload) == 0 {
		return map[string]any{}
	}
	if err := json.Unmarshal(e.Payload, &value); err != nil {
		return map[string]any{"_raw": string(e.Payload)}
	}
	return value
}

// ErrorEnvelope is the structured error inside a failed reply or a fatal frame.
type ErrorEnvelope struct {
	Code          string          `json:"code"`
	Message       string          `json:"message"`
	Retryable     bool            `json:"retryable"`
	ResultUnknown bool            `json:"resultUnknown"`
	Details       json.RawMessage `json:"details,omitempty"`
}

// Reply is one server answer to a request.
type Reply struct {
	Type             string          `json:"type"`
	OK               bool            `json:"ok"`
	ID               string          `json:"id"`
	Result           json.RawMessage `json:"result"`
	Error            *ErrorEnvelope  `json:"error"`
	ServerTimeMillis int64           `json:"serverTimeMillis"`
}

// Request is one client request frame.
type Request struct {
	Type          string         `json:"type"`
	ID            string         `json:"id"`
	Op            string         `json:"op"`
	Params        map[string]any `json:"params"`
	TimeoutMillis int64          `json:"timeoutMillis,omitempty"`
}

// EventEnvelope is the wire shape of a pushed event.
type EventEnvelope struct {
	Type      string          `json:"type"`
	Seq       int64           `json:"seq"`
	RunID     string          `json:"runId"`
	StreamID  string          `json:"streamId"`
	Replay    bool            `json:"replay,omitempty"`
	Event     json.RawMessage `json:"event"`
	Timestamp int64           `json:"serverTimeMillis"`
}

// WriteFrame writes one length-prefixed JSON frame.
func WriteFrame(writer *bufio.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) > MaxFrameBytes {
		return fmt.Errorf("outbound frame of %d bytes exceeds the %d byte limit", len(payload), MaxFrameBytes)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one length-prefixed JSON frame.
func ReadFrame(reader *bufio.Reader) (json.RawMessage, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > MaxFrameBytes {
		return nil, fmt.Errorf("frame length %d is outside 1..%d", length, MaxFrameBytes)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

var errClosed = errors.New("control connection is closed")
