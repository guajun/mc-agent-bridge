package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/discovery"
	"github.com/guajun/mc-agent-bridge/internal/legacy"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

// Legacy is the explicit adapter to the pre-#8 local JSON-lines mod protocol.
type Legacy struct {
	target *config.Target
	client *legacy.Client

	mu     sync.Mutex
	state  State
	events chan RawEvent
	done   chan struct{}
	stop   chan struct{}
	closer sync.Once
	err    error

	nonce    string
	nextID   atomic.Uint64
	sequence int64
	ring     []bufferedEvent
}

type bufferedEvent struct {
	seq     int64
	payload map[string]any
}

// DialLegacy connects to the legacy loopback endpoint, resolving port.txt when
// the target does not name a port.
func DialLegacy(ctx context.Context, target *config.Target) (*Legacy, error) {
	address := target.Address
	portSource := "argument"
	if address == "" {
		resolution := discovery.Resolve(0, target.PortFile, target.Vantage, target.ServerDir)
		if !resolution.Resolved() {
			return nil, protocol.NewError(protocol.CodeConnectionFailed, resolution.Error)
		}
		address = fmt.Sprintf("127.0.0.1:%d", resolution.Port)
		portSource = resolution.Source
	}
	client, err := legacy.Dial(ctx, address, 10*time.Second)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.CodeConnectionFailed,
			Message: fmt.Sprintf("cannot reach the legacy mod on %s: %v", address, err)}
	}
	instance := client.Instance()
	if instance == "" {
		instance = discovery.VantageServer
	}
	session := &Legacy{
		target: target,
		client: client,
		nonce:  "lg-" + legacyNonce(),
		events: make(chan RawEvent, 1024),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		state: State{
			Target:       target.Name,
			Transport:    protocol.TransportLegacy,
			Vantage:      instance,
			Connected:    true,
			Endpoint:     address,
			PortSource:   portSource,
			ModVersion:   stringValue(client.Hello(), "version"),
			Minecraft:    stringValue(client.Hello(), "minecraft"),
			Instance:     instance,
			InstanceID:   stringValue(client.Hello(), "instance"),
			Capabilities: client.Capabilities(),
			ConnectedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	go session.pump()
	return session, nil
}

func (l *Legacy) pump() {
	defer close(l.done)
	defer close(l.events)
	for {
		select {
		case payload, ok := <-l.client.Events():
			if !ok {
				return
			}
			l.mu.Lock()
			l.sequence++
			seq := l.sequence
			l.ring = append(l.ring, bufferedEvent{seq: seq, payload: payload})
			if len(l.ring) > 512 {
				l.ring = l.ring[len(l.ring)-512:]
			}
			l.state.LastSeq = seq
			l.mu.Unlock()
			select {
			case l.events <- RawEvent{Seq: seq, Payload: payload}:
			case <-l.stop:
				return
			}
		case <-l.client.Done():
			l.mu.Lock()
			l.err = l.client.Err()
			l.state.Connected = false
			l.state.LastError = l.client.Err().Error()
			l.mu.Unlock()
			return
		case <-l.stop:
			return
		}
	}
}

// NextRequestID returns an id unique across processes and reconnects.
func (l *Legacy) NextRequestID() string {
	return l.nonce + "-" + strconv.FormatUint(l.nextID.Add(1), 10)
}

// Call maps one operation onto the legacy line protocol with a fresh id.
func (l *Legacy) Call(ctx context.Context, operation string, params map[string]any) (any, *protocol.Error) {
	return l.CallID(ctx, l.NextRequestID(), operation, params)
}

// CallID maps one operation onto the legacy line protocol under a stable id.
func (l *Legacy) CallID(ctx context.Context, requestID, operation string,
	params map[string]any) (any, *protocol.Error) {
	l.mu.Lock()
	state := l.state
	l.mu.Unlock()
	if gate := CheckOperation(operation, state.Capabilities, state.Vantage); gate != nil {
		return nil, gate
	}
	if params == nil {
		params = map[string]any{}
	}
	switch operation {
	case "fork", "restore", "verify", "order":
		// CheckOperation already refuses these; kept explicit for clarity.
		return nil, protocol.NewError(protocol.CodeCapabilityNotSupported,
			"this operation is not migrated to the Go runtime")
	case "request_status", "exclusive_acquire", "exclusive_renew", "exclusive_release", "exclusive_status":
		return nil, &protocol.Error{Code: protocol.CodeCapabilityNotSupported,
			Message: "the legacy local adapter has no server-side write ledger or exclusive leases; " +
				"connect this target over the authenticated control protocol for mc-agent#6 coordination"}
	case "capabilities":
		return map[string]any{
			"type":         "capabilities",
			"instance":     state.Instance,
			"modVersion":   state.ModVersion,
			"minecraft":    state.Minecraft,
			"capabilities": state.Capabilities,
			"transport":    protocol.TransportLegacy,
		}, nil
	case "status":
		return snapshotMap(state), nil
	case "command_output":
		return l.commandOutput(ctx, requestID, params)
	case "save":
		result, failure := l.line(ctx, requestID, "save", "STATE")
		if failure != nil {
			return nil, failure
		}
		return saveFromState(result), nil
	}

	line, failure := legacyLine(operation, params)
	if failure != nil {
		failure.RequestID = requestID
		failure.Operation = operation
		return nil, failure
	}
	return l.line(ctx, requestID, operation, line)
}

// line sends one legacy line and classifies its failure. A write that fails
// after the bytes left the process is result-unknown: the legacy protocol has
// no request ledger, so it is never assumed safe to retry.
func (l *Legacy) line(ctx context.Context, requestID, operation, line string) (any, *protocol.Error) {
	timeout := 30 * time.Second
	if strings.HasPrefix(line, "SNAPSHOT") {
		timeout = 120 * time.Second
	}
	reply, err := l.client.Request(ctx, line, timeout)
	if err != nil {
		code := protocol.CodeGameError
		message := err.Error()
		if strings.Contains(message, "no response") {
			code = protocol.CodeTimeout
		}
		return nil, &protocol.Error{Code: code, Message: message,
			RequestID: requestID, Operation: operation,
			ResultUnknown: protocol.WriteOperation(operation)}
	}
	if kind, _ := reply["type"].(string); kind == "error" {
		return nil, &protocol.Error{Code: protocol.CodeGameError,
			Message: fmt.Sprint(reply["message"]), RequestID: requestID, Operation: operation}
	}
	return reply, nil
}

func (l *Legacy) commandOutput(ctx context.Context, requestID string, params map[string]any) (any, *protocol.Error) {
	command := stringParam(params, "command")
	if command == "" {
		return nil, protocol.NewError(protocol.CodeBadRequest, `command_output needs a command`)
	}
	l.mu.Lock()
	cursor := l.sequence
	l.mu.Unlock()
	result, failure := l.line(ctx, requestID, "command_output", "CMD "+oneLine(command))
	if failure != nil {
		return nil, failure
	}
	if object, ok := result.(map[string]any); ok {
		if output, ok := object["output"].([]any); ok {
			lines := make([]string, 0, len(output))
			for _, entry := range output {
				lines = append(lines, fmt.Sprint(entry))
			}
			return map[string]any{
				"type":    "command_output",
				"command": command,
				"output":  lines,
				"source":  "ack",
			}, nil
		}
	}
	wait := numberParam(params, "wait", 2)
	if wait < 0.1 {
		wait = 0.1
	}
	if wait > 30 {
		wait = 30
	}
	select {
	case <-time.After(time.Duration(wait * float64(time.Second))):
	case <-ctx.Done():
	case <-l.done:
	}
	l.mu.Lock()
	var output []string
	for _, entry := range l.ring {
		if entry.seq <= cursor {
			continue
		}
		kind, _ := entry.payload["type"].(string)
		if kind == "game" || kind == "chat" {
			output = append(output, fmt.Sprint(entry.payload["text"]))
		}
	}
	l.mu.Unlock()
	return map[string]any{
		"type":    "command_output",
		"command": command,
		"output":  output,
		"source":  "events",
	}, nil
}

// Events returns the normalized event stream.
func (l *Legacy) Events() <-chan RawEvent { return l.events }

// Done closes when the connection ends.
func (l *Legacy) Done() <-chan struct{} { return l.done }

// Err reports the terminal cause.
func (l *Legacy) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		return fmt.Errorf("legacy session closed")
	}
	return l.err
}

// State returns a snapshot of the connection metadata.
func (l *Legacy) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	copy := l.state
	copy.Capabilities = append([]string(nil), l.state.Capabilities...)
	return copy
}

// Close ends the session and bounds its exit even when a consumer stopped
// reading the event channel.
func (l *Legacy) Close() {
	l.closer.Do(func() { close(l.stop) })
	l.client.Close()
}

// legacyLine maps a structured operation onto its stable line form.
func legacyLine(operation string, params map[string]any) (string, *protocol.Error) {
	badRequest := func(message string) (string, *protocol.Error) {
		return "", protocol.NewError(protocol.CodeBadRequest, message)
	}
	switch operation {
	case "ping":
		return "PING", nil
	case "state":
		return "STATE", nil
	case "entities":
		radius := numberParam(params, "radius", 0)
		if radius < 0 {
			return badRequest("radius cannot be negative")
		}
		if radius == 0 {
			return "ENTITIES", nil
		}
		return "ENTITIES " + strconv.FormatFloat(radius, 'f', -1, 64), nil
	case "player":
		identifier := firstNonEmpty(params, "player", "uuid", "name", "id")
		if identifier == "" {
			return badRequest("player needs a name or uuid")
		}
		return "PLAYER " + oneLine(identifier), nil
	case "context":
		identifier := firstNonEmpty(params, "contextId", "context_id", "id", "context")
		if identifier == "" {
			return badRequest("context needs an id")
		}
		return "CONTEXT_GET " + oneLine(identifier), nil
	case "command":
		command := stringParam(params, "command")
		if command == "" {
			return badRequest("command must not be empty")
		}
		return "CMD " + oneLine(command), nil
	case "chat":
		message := stringParam(params, "message")
		return "CHAT " + oneLine(message), nil
	case "mark":
		return "MARK " + oneLine(stringParam(params, "text")), nil
	case "wait":
		ticks := int(numberParam(params, "ticks", 0))
		if ticks <= 0 {
			return badRequest("ticks must be positive")
		}
		return "WAIT " + strconv.Itoa(ticks), nil
	case "screen":
		return "SCREEN", nil
	case "connect":
		address := stringParam(params, "address")
		if address == "" {
			return badRequest("connect needs an address")
		}
		return "CONNECT " + oneLine(address), nil
	case "world":
		level := stringParam(params, "level")
		if level == "" {
			return badRequest("world needs a level name")
		}
		return "WORLD " + oneLine(level), nil
	case "lan":
		port := int(numberParam(params, "port", 0))
		mode := strings.ToLower(stringParam(params, "mode"))
		if mode != "" && mode != "online" && mode != "offline" {
			return badRequest("lan mode must be online or offline")
		}
		line := "LAN"
		if port > 0 {
			line += " " + strconv.Itoa(port)
		}
		if mode != "" {
			line += " " + mode
		}
		return line, nil
	case "record_start":
		ticks := int(numberParam(params, "ticks", 200))
		radius := numberParam(params, "radius", 64)
		interval := int(numberParam(params, "interval", 1))
		return fmt.Sprintf("SAMPLE_START %d %s %d", ticks, strconv.FormatFloat(radius, 'f', -1, 64), interval), nil
	case "record_stop":
		return "SAMPLE_STOP", nil
	case "snapshot":
		name := stringParam(params, "name")
		radius := numberParam(params, "radius", 0)
		line := "SNAPSHOT"
		if radius > 0 {
			line += " " + strconv.FormatFloat(radius, 'f', -1, 64)
		}
		if name != "" {
			line += " " + oneLine(name)
		}
		return line, nil
	case "snapshots":
		return "SNAPSHOTS", nil
	}
	return "", protocol.NewError(protocol.CodeCapabilityNotSupported,
		"operation "+operation+" is not available on the legacy local adapter")
}

func firstNonEmpty(params map[string]any, names ...string) string {
	for _, name := range names {
		if value := stringParam(params, name); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(values map[string]any, name string) string {
	if value, ok := values[name].(string); ok {
		return value
	}
	return ""
}

func legacyNonce() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}
