// Package session adapts the two transports to one operation surface: the
// authenticated remote control protocol and the explicit legacy loopback
// adapter. The daemon only ever sees this interface, so transport and tool
// logic stay separated.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

// State is a point-in-time snapshot of one target session.
type State struct {
	Target        string   `json:"target"`
	Transport     string   `json:"transport"`
	Vantage       string   `json:"vantage"`
	Connected     bool     `json:"connected"`
	ModVersion    string   `json:"modVersion,omitempty"`
	Minecraft     string   `json:"minecraft,omitempty"`
	Instance      string   `json:"instance,omitempty"`
	InstanceID    string   `json:"instanceId,omitempty"`
	RunID         string   `json:"runId,omitempty"`
	SessionID     string   `json:"sessionId,omitempty"`
	StreamID      string   `json:"streamId,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Permissions   []string `json:"permissions,omitempty"`
	LastError     string   `json:"lastError,omitempty"`
	LastErrorCode string   `json:"lastErrorCode,omitempty"`
	Endpoint      string   `json:"endpoint,omitempty"`
	PortSource    string   `json:"portSource,omitempty"`
	LastSeq       int64    `json:"lastSeq"`
	ReplayLost    bool     `json:"replayLost,omitempty"`
	ConnectedAt   string   `json:"connectedAt,omitempty"`
}

// RawEvent is a transport-neutral pushed event. Remote events carry the
// server's sequence numbers; the daemon assigns its own stream on top of them.
type RawEvent struct {
	Seq     int64
	RunID   string
	Replay  bool
	Payload map[string]any
}

// Adapter is one live connection to one target.
type Adapter interface {
	// Call runs one operation. It never retries anything by itself.
	Call(ctx context.Context, operation string, params map[string]any) (any, *protocol.Error)
	// Events is the pushed event stream; it closes when the connection ends.
	Events() <-chan RawEvent
	// Done closes when the connection ends (for reconnection loops).
	Done() <-chan struct{}
	// Err is the terminal error, if any.
	Err() error
	// State reports the current connection metadata.
	State() State
	// Close terminates the connection.
	Close()
}

// CheckOperation applies the client-side capability and support gates before a
// request reaches the wire. The server enforces the same rules again.
func CheckOperation(operation string, capabilities []string, vantage string) *protocol.Error {
	definition, ok := protocol.OperationByName(operation)
	if !ok {
		return &protocol.Error{Code: protocol.CodeCapabilityNotSupported,
			Message: "unknown bridge method: " + operation}
	}
	if definition.Unsupported != "" {
		return &protocol.Error{Code: protocol.CodeCapabilityNotSupported,
			Message: definition.Unsupported}
	}
	if definition.Vantage != "" && vantage != "" && definition.Vantage != vantage {
		return &protocol.Error{Code: protocol.CodeCapabilityNotSupported,
			Message: fmt.Sprintf("operation %q is only available from the %s vantage; this target is %s",
				operation, definition.Vantage, vantage)}
	}
	if missing := protocol.MissingCapabilities(definition, capabilities); len(missing) > 0 {
		where := "the connected mod"
		if vantage != "" {
			where = "the connected " + vantage + "-vantage mod"
		}
		return &protocol.Error{Code: protocol.CodeCapabilityNotSupported,
			Message: fmt.Sprintf("operation %q is not available: %s does not advertise %s",
				operation, where, strings.Join(missing, ", "))}
	}
	return nil
}

// TimeoutFor returns the per-operation deadline, matching the Python bridge's
// allowances for slow operations.
func TimeoutFor(operation string, params map[string]any) time.Duration {
	switch operation {
	case "snapshot":
		return 120 * time.Second
	case "wait":
		ticks := numberParam(params, "ticks", 0)
		return time.Duration(ticks/20.0*float64(time.Second)) + 20*time.Second
	case "command_output":
		wait, _ := params["wait"].(float64)
		if wait <= 0 {
			wait = 2
		}
		if wait > 30 {
			wait = 30
		}
		return time.Duration(wait*float64(time.Second)) + 30*time.Second
	default:
		return 30 * time.Second
	}
}

func numberParam(params map[string]any, name string, fallback float64) float64 {
	value, ok := params[name]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed
		}
	}
	return fallback
}

func stringParam(params map[string]any, name string) string {
	if value, ok := params[name].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func oneLine(value string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(value)
}
