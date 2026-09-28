package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/control"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

// Remote is a session over the authenticated TLS control protocol.
type Remote struct {
	target *config.Target
	token  string

	mu     sync.Mutex
	client *control.Client
	state  State
	events chan RawEvent
	done   chan struct{}
	stop   chan struct{}
	closer sync.Once
	err    error
}

// DialRemote connects and negotiates the control session. It is called by the
// daemon's reconnect loop; lastSeq is the highest server event sequence seen.
func DialRemote(ctx context.Context, target *config.Target, token string, lastSeq int64,
	expectRunID string) (*Remote, error) {
	client, welcome, err := control.Dial(ctx, control.Config{
		Address:     target.Address,
		Pin:         target.Pin,
		CAFile:      target.CAFile,
		ServerName:  target.ServerName,
		Token:       token,
		LastSeq:     lastSeq,
		ExpectRunID: expectRunID,
	})
	if err != nil {
		return nil, err
	}
	instance := "server"
	if strings.Contains(strings.ToLower(welcome.Mod+" "+welcome.Transport), "client") {
		instance = "client"
	}
	remote := &Remote{
		target: target,
		token:  token,
		client: client,
		events: make(chan RawEvent, 1024),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		state: State{
			Target:       target.Name,
			Transport:    protocol.TransportRemote,
			Vantage:      instance,
			Connected:    true,
			ModVersion:   welcome.ModVersion,
			Minecraft:    welcome.Minecraft,
			Instance:     instance,
			InstanceID:   welcome.InstanceID,
			RunID:        welcome.RunID,
			SessionID:    welcome.SessionID,
			StreamID:     welcome.RunID,
			Capabilities: welcome.Capabilities,
			Permissions:  welcome.Permissions,
			Endpoint:     target.Address,
			LastSeq:      lastSeq,
			ReplayLost:   welcome.Replay.Lost,
			ConnectedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	go remote.pump()
	return remote, nil
}

func (r *Remote) pump() {
	defer close(r.done)
	defer close(r.events)
	for {
		select {
		case event, ok := <-r.client.Events():
			if !ok {
				return
			}
			r.mu.Lock()
			r.state.LastSeq = event.Seq
			r.mu.Unlock()
			select {
			case r.events <- RawEvent{
				Seq:     event.Seq,
				RunID:   event.RunID,
				Replay:  event.Replay,
				Payload: event.Map(),
			}:
			case <-r.stop:
				return
			}
		case <-r.client.Done():
			r.mu.Lock()
			r.err = r.client.Err()
			r.state.Connected = false
			r.state.LastError = r.client.Err().Error()
			r.mu.Unlock()
			return
		case <-r.stop:
			return
		}
	}
}

// Call runs one operation with a fresh unique id.
func (r *Remote) Call(ctx context.Context, operation string, params map[string]any) (any, *protocol.Error) {
	return r.CallID(ctx, r.NextRequestID(), operation, params)
}

// NextRequestID returns an id unique across processes and reconnects.
func (r *Remote) NextRequestID() string {
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client == nil {
		return ""
	}
	return client.NextRequestID()
}

// CallID runs one operation under a caller-chosen stable id.
func (r *Remote) CallID(ctx context.Context, requestID, operation string,
	params map[string]any) (any, *protocol.Error) {
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	if gate := CheckOperation(operation, state.Capabilities, state.Vantage); gate != nil {
		gate.RequestID = requestID
		gate.Operation = operation
		return nil, gate
	}
	if params == nil {
		params = map[string]any{}
	}
	timeout := TimeoutFor(operation, params)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch operation {
	case "capabilities":
		return map[string]any{
			"type":         "capabilities",
			"instance":     state.Instance,
			"instanceId":   state.InstanceID,
			"runId":        state.RunID,
			"sessionId":    state.SessionID,
			"modVersion":   state.ModVersion,
			"minecraft":    state.Minecraft,
			"capabilities": state.Capabilities,
			"permissions":  state.Permissions,
			"transport":    protocol.TransportRemote,
		}, nil
	case "status":
		snapshot := r.State()
		return snapshotMap(snapshot), nil
	case "save":
		result, failure := r.callWithID(callCtx, requestID, "state", nil)
		if failure != nil {
			return nil, failure
		}
		return saveFromState(result), nil
	case "command_output":
		result, failure := r.callWithID(callCtx, requestID, "command",
			map[string]any{"command": params["command"]})
		if failure != nil {
			return nil, failure
		}
		output, _ := result.(map[string]any)["output"].([]any)
		lines := make([]string, 0, len(output))
		for _, entry := range output {
			lines = append(lines, fmt.Sprint(entry))
		}
		return map[string]any{
			"type":    "command_output",
			"command": params["command"],
			"output":  lines,
			"source":  "ack",
		}, nil
	}

	result, failure := r.callWithID(callCtx, requestID, operation, params)
	if failure != nil {
		return nil, failure
	}
	return result, nil
}

func (r *Remote) call(ctx context.Context, operation string, params map[string]any) (any, *protocol.Error) {
	return r.callWithID(ctx, r.NextRequestID(), operation, params)
}

func (r *Remote) callWithID(ctx context.Context, requestID, operation string,
	params map[string]any) (any, *protocol.Error) {
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client == nil {
		return nil, protocol.NewError(protocol.CodeConnectionLost, "the control session is not connected")
	}
	raw, failure := client.CallWithID(ctx, requestID, operation, params)
	if failure != nil {
		return nil, failure
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeInternal,
			Message:   fmt.Sprintf("the server sent an unreadable %s result: %v", operation, err),
			RequestID: requestID, Operation: operation}
	}
	return value, nil
}

// Events returns the normalized event stream.
func (r *Remote) Events() <-chan RawEvent { return r.events }

// Done closes when the connection ends.
func (r *Remote) Done() <-chan struct{} { return r.done }

// Err reports the terminal cause.
func (r *Remote) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		return fmt.Errorf("remote session closed")
	}
	return r.err
}

// State returns a snapshot of the connection metadata.
func (r *Remote) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := r.state
	copy.Capabilities = append([]string(nil), r.state.Capabilities...)
	copy.Permissions = append([]string(nil), r.state.Permissions...)
	return copy
}

// Close ends the session. It bounds the exit even with a full event channel
// and no consumer: the pump abandons a blocked delivery when stop closes.
func (r *Remote) Close() {
	r.closer.Do(func() { close(r.stop) })
	if r.client != nil {
		r.client.Close()
	}
}

func snapshotMap(state State) map[string]any {
	result := map[string]any{
		"connected":   state.Connected,
		"transport":   state.Transport,
		"vantage":     state.Vantage,
		"target":      state.Target,
		"endpoint":    state.Endpoint,
		"instanceId":  state.InstanceID,
		"runId":       state.RunID,
		"sessionId":   state.SessionID,
		"modVersion":  state.ModVersion,
		"minecraft":   state.Minecraft,
		"lastSeq":     state.LastSeq,
		"connectedAt": state.ConnectedAt,
	}
	if state.Capabilities != nil {
		sorted := append([]string(nil), state.Capabilities...)
		sort.Strings(sorted)
		result["capabilities"] = sorted
	}
	if state.LastError != "" {
		result["lastError"] = state.LastError
	}
	return result
}

func saveFromState(result any) map[string]any {
	object, _ := result.(map[string]any)
	saved := map[string]any{
		"type":       "world_save",
		"instance":   object["instance"],
		"levelName":  object["levelName"],
		"worldDir":   object["worldDir"],
		"tick":       object["tick"],
		"players":    object["players"],
		"playerList": object["playerList"],
		"levels":     object["levels"],
	}
	saved["worldDirRemote"] = true
	if serverVersion, ok := object["serverVersion"]; ok {
		saved["serverVersion"] = serverVersion
	}
	return saved
}
