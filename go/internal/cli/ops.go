package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

// cmdSchema returns the operation contract; with a running daemon the full
// parameter schema comes from the daemon, otherwise a compact local listing is
// returned so an offline agent can still discover the surface.
func (a *app) cmdSchema(ctx context.Context, args []string) (any, *protocol.Error) {
	flags := flag.NewFlagSet("schema", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return nil, protocol.NewError(protocol.CodeUsage, err.Error())
	}
	operation := ""
	if flags.NArg() > 0 {
		operation = flags.Arg(0)
	}
	if a.daemonRunning() {
		params := map[string]any{}
		if operation != "" {
			params["op"] = operation
		}
		return a.daemonCall(ctx, "schema", params)
	}
	if operation != "" {
		definition, ok := protocol.OperationByName(operation)
		if !ok {
			return nil, protocol.NewError(protocol.CodeCapabilityNotSupported, "unknown operation: "+operation)
		}
		return map[string]any{"op": definition.Name, "description": definition.Description,
			"requires": definition.Requires, "write": definition.Write,
			"supported": definition.Unsupported == "", "unsupportedReason": definition.Unsupported,
			"note": "start the daemon for the full parameter schema"}, nil
	}
	operations := make([]map[string]any, 0, len(protocol.Operations))
	for _, definition := range protocol.Operations {
		operations = append(operations, map[string]any{
			"op": definition.Name, "description": definition.Description,
			"requires": definition.Requires, "write": definition.Write,
			"supported": definition.Unsupported == "",
		})
	}
	return map[string]any{"operations": operations,
		"note": "start the daemon for parameter schemas"}, nil
}

// cmdCall runs one named operation with JSON or key=value parameters.
func (a *app) cmdCall(ctx context.Context, args []string) (any, *protocol.Error) {
	if len(args) == 0 {
		return nil, protocol.NewError(protocol.CodeUsage, "call needs an operation name")
	}
	operation := args[0]
	rest := args[1:]
	flags := flag.NewFlagSet("call", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	paramsJSON := flags.String("params", "", "JSON object with operation parameters")
	timeout := flags.Float64("timeout", 0, "operation timeout in seconds")
	requestIDFlag := flags.String("request-id", "", "stable id for a non-idempotent write")
	var keyValues multiFlag
	flags.Var(&keyValues, "param", "key=value parameter (repeatable)")
	if err := flags.Parse(reorderInterspersed(rest, map[string]bool{"params": true, "timeout": true,
		"param": true, "request-id": true})); err != nil {
		return nil, protocol.NewError(protocol.CodeUsage, err.Error())
	}
	params := map[string]any{}
	if *paramsJSON != "" {
		if err := json.Unmarshal([]byte(*paramsJSON), &params); err != nil {
			return nil, protocol.NewError(protocol.CodeBadRequest, "--params is not a JSON object: "+err.Error())
		}
	}
	for _, entry := range keyValues {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			return nil, protocol.NewError(protocol.CodeBadRequest, "expected key=value, got "+entry)
		}
		params[key] = parseScalar(value)
	}
	envelope := map[string]any{"op": operation, "params": params}
	if *timeout > 0 {
		envelope["timeoutSeconds"] = *timeout
	}
	requestID := *requestIDFlag
	if requestID != "" && len(requestID) > 128 {
		return nil, protocol.NewError(protocol.CodeBadRequest, "request id must be 1..128 characters")
	}
	if requestID == "" && protocol.WriteOperation(operation) {
		requestID = a.newRequestID()
	}
	if requestID != "" {
		envelope["requestId"] = requestID
	}
	return a.daemonCallID(ctx, "call", envelope, requestID)
}

func parseScalar(value string) any {
	if value == "true" {
		return true
	}
	if value == "false" {
		return false
	}
	if number, err := strconv.ParseFloat(value, 64); err == nil {
		return number
	}
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		return decoded
	}
	return value
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

// cmdEvents replays the daemon buffer and optionally follows new events.
//
// The cursor is a (streamId, seq) pair: streamId binds the sequence to one
// daemon run, so a stale cursor after a daemon restart is reported as a reset
// instead of silently returning nothing. --follow subscribes first, pages the
// whole replay (not just one page), then switches to live events with seq
// de-duplication, category and target filtering, and an explicit gap marker.
func (a *app) cmdEvents(ctx context.Context, args []string) (any, *protocol.Error) {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	since := flags.Int64("since", 0, "last seen daemon sequence")
	limit := flags.Int("limit", 200, "maximum events per replay page")
	category := flags.String("category", "", "category filter")
	follow := flags.Bool("follow", false, "keep streaming new events")
	streamID := flags.String("stream-id", "", "daemon stream id the cursor belongs to")
	if err := flags.Parse(reorderInterspersed(args, map[string]bool{"since": true, "limit": true,
		"category": true, "stream-id": true})); err != nil {
		return nil, protocol.NewError(protocol.CodeUsage, err.Error())
	}
	client, failure := a.connectDaemon()
	if failure != nil {
		return nil, failure
	}
	defer client.Close()

	pageParams := func(cursor int64, cursorStream string) map[string]any {
		params := map[string]any{"since": cursor, "limit": *limit, "streamId": cursorStream}
		if *category != "" {
			params["category"] = *category
		}
		if a.target != "" {
			params["target"] = a.target
		}
		return params
	}

	if !*follow {
		return client.Call(ctx, "events", pageParams(*since, *streamID))
	}

	cursor := *since
	expectedStream := *streamID
	var lastSeq int64

	// recover pages the daemon ring from the paging cursor until the page is
	// complete, printing every event once and moving the cursor to the highest
	// delivered sequence. Initial replay and live recovery share this path: a
	// live-queue drop and a ring eviction both replay from the last actually
	// delivered sequence instead of skipping the lost range.
	recover := func() *protocol.Error {
		for {
			result, failure := client.Call(ctx, "events", pageParams(cursor, expectedStream))
			if failure != nil {
				return failure
			}
			object, _ := result.(map[string]any)
			stream, _ := object["streamId"].(string)
			if expectedStream == "" {
				expectedStream = stream
			}
			if object["reset"] == true || (expectedStream != "" && stream != expectedStream) {
				a.print(gapMarker(expectedStream, 0, lastSeq, true, true))
				expectedStream = stream
				cursor = 0
				lastSeq = 0
				continue
			}
			if object["dropped"] == true {
				a.print(gapMarker(stream, 0, lastSeq, true, false))
			}
			if events, ok := object["events"].([]any); ok {
				for _, entry := range events {
					a.print(entry)
					if seq, ok := eventSeq(entry); ok && seq > lastSeq {
						lastSeq = seq
					}
				}
			}
			if next, ok := numberAsInt64(object["next"]); ok {
				cursor = next
			}
			if object["truncated"] != true {
				break
			}
		}
		return nil
	}

	// recoverLive folds in live-queue overflow: report it, then replay from
	// the last delivered sequence so recoverable events come back from the
	// daemon ring and unrecoverable ones surface as its explicit gap marker.
	recoverLive := func(dropped int64) *protocol.Error {
		for {
			if dropped > 0 {
				a.print(clientGapMarker(expectedStream, lastSeq, dropped))
			}
			cursor = lastSeq
			if failure := recover(); failure != nil {
				return failure
			}
			dropped = client.TakeDropped()
			if dropped == 0 {
				return nil
			}
		}
	}

	// Subscribe before replaying so nothing is lost between the snapshot and
	// the live stream; the server confirms the subscription synchronously.
	subscription := []string{"*"}
	if *category != "" {
		subscription = []string{*category}
	}
	if _, failure := client.Call(ctx, "subscribe", map[string]any{"events": subscription}); failure != nil {
		return nil, failure
	}

	// Replay until a pass observes no client-side overflow: live events can be
	// dropped by the bounded queue while a replay response is in flight, so
	// each pass re-pages from the cursor to recover them from the daemon ring.
	for {
		if failure := recover(); failure != nil {
			return nil, failure
		}
		overflow := client.TakeDropped()
		if overflow == 0 {
			break
		}
		a.print(clientGapMarker(expectedStream, lastSeq, overflow))
		cursor = lastSeq
	}
	a.print(map[string]any{"type": "stream", "event": "following", "data": map[string]any{
		"streamId": expectedStream, "lastSeq": lastSeq}})

	// Live delivery. A sequence jump is NOT evidence of loss when a category
	// or target filter is active: unrelated events consume sequence numbers
	// legitimately. Loss is reported only from explicit metadata: the queue
	// drop tag/signal, or the daemon's reset/dropped flags during recovery.
	for {
		// Observe overflow before processing further events so a gap is
		// recovered from the last delivered sequence instead of skipped.
		select {
		case <-client.DropNotify():
			if dropped := client.TakeDropped(); dropped > 0 {
				if failure := recoverLive(dropped); failure != nil {
					return nil, failure
				}
			}
			continue
		default:
		}
		select {
		case <-client.DropNotify():
			if dropped := client.TakeDropped(); dropped > 0 {
				if failure := recoverLive(dropped); failure != nil {
					return nil, failure
				}
			}
		case event, ok := <-client.Events():
			if !ok {
				return nil, protocol.NewError(protocol.CodeConnectionLost, "the daemon connection closed")
			}
			if event.DroppedBefore > 0 {
				if failure := recoverLive(event.DroppedBefore); failure != nil {
					return nil, failure
				}
			}
			if a.target != "" {
				if value, _ := event.Data["target"].(string); value != a.target {
					continue
				}
			}
			seq, hasSeq := eventSeq(event.Data)
			if hasSeq && seq <= lastSeq {
				continue // already delivered by a recovery pass
			}
			a.print(event.Data)
			if hasSeq {
				lastSeq = seq
			}
		case <-ctx.Done():
			return nil, nil
		}
	}
}

// clientGapMarker reports live events the bounded IPC queue dropped and the
// recovery pass that follows from the last delivered sequence.
func clientGapMarker(stream string, lastSeq int64, overflow int64) map[string]any {
	return map[string]any{"type": "stream", "event": "gap", "data": map[string]any{
		"streamId": stream, "from": 0, "to": lastSeq, "dropped": true,
		"reset": false, "clientDropped": overflow}}
}

func gapMarker(stream string, from, to int64, dropped, reset bool) map[string]any {
	return map[string]any{"type": "stream", "event": "gap", "data": map[string]any{
		"streamId": stream, "from": from, "to": to, "dropped": dropped, "reset": reset}}
}

func eventSeq(value any) (int64, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return 0, false
	}
	return numberAsInt64(object["seq"])
}

func numberAsInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	}
	return 0, false
}

// cmdConvenience maps the ergonomic subcommands onto daemon operations.
func (a *app) cmdConvenience(ctx context.Context, command string, args []string) (any, *protocol.Error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	timeout := flags.Float64("timeout", 0, "operation timeout in seconds")
	radius := flags.Float64("radius", 0, "radius")
	name := flags.String("name", "", "snapshot name")
	dimension := flags.String("dimension", "", "dimension id")
	port := flags.Int("port", 0, "LAN port")
	offline := flags.Bool("offline", false, "publish LAN as offline mode")
	waitSeconds := flags.Float64("wait", 2, "seconds to collect command output")
	ticks := flags.Int("ticks", 200, "record ticks")
	interval := flags.Int("interval", 1, "record interval")
	ttl := flags.Int("ttl", 300, "lease ttl seconds")
	if err := flags.Parse(reorderInterspersed(args, map[string]bool{"timeout": true, "radius": true,
		"name": true, "dimension": true, "port": true, "wait": true, "ticks": true,
		"interval": true, "ttl": true})); err != nil {
		return nil, protocol.NewError(protocol.CodeUsage, err.Error())
	}
	positional := flags.Args()
	operation := command
	params := map[string]any{}
	switch command {
	case "state", "snapshots", "save", "screen", "record-stop":
	case "entities":
		if *radius > 0 {
			params["radius"] = *radius
		}
	case "player":
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "player needs a name or uuid")
		}
		params["player"] = positional[0]
	case "context":
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "context needs an id")
		}
		params["contextId"] = positional[0]
	case "command", "command-output":
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, command+" needs a command line")
		}
		params["command"] = strings.Join(positional, " ")
		if command == "command-output" {
			operation = "command_output"
			params["wait"] = *waitSeconds
		}
	case "chat":
		params["message"] = strings.Join(positional, " ")
	case "mark":
		params["text"] = strings.Join(positional, " ")
	case "connect":
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "connect needs host:port")
		}
		params["address"] = positional[0]
	case "world":
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "world needs a level name")
		}
		params["level"] = positional[0]
	case "lan":
		if *port > 0 {
			params["port"] = *port
		}
		if *offline {
			params["mode"] = "offline"
		}
	case "record-start":
		operation = "record_start"
		params["ticks"] = *ticks
		if *radius > 0 {
			params["radius"] = *radius
		}
		params["interval"] = *interval
	case "wait":
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "wait needs a tick count")
		}
		value, err := strconv.Atoi(positional[0])
		if err != nil {
			return nil, protocol.NewError(protocol.CodeBadRequest, "wait needs a tick count")
		}
		params["ticks"] = value
	case "snapshot":
		if *radius > 0 {
			params["radius"] = *radius
		}
		if *name != "" {
			params["name"] = *name
		}
		if *dimension != "" {
			params["dimension"] = *dimension
		}
	case "request-status":
		operation = "request_status"
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "request-status needs a request id")
		}
		params["requestId"] = positional[0]
	case "exclusive-acquire":
		operation = "exclusive_acquire"
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "exclusive-acquire needs a key")
		}
		params["key"] = positional[0]
		params["ttlSeconds"] = *ttl
	case "exclusive-renew":
		operation = "exclusive_renew"
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "exclusive-renew needs a key")
		}
		params["key"] = positional[0]
		params["ttlSeconds"] = *ttl
	case "exclusive-release":
		operation = "exclusive_release"
		if len(positional) == 0 {
			return nil, protocol.NewError(protocol.CodeBadRequest, "exclusive-release needs a key")
		}
		params["key"] = positional[0]
	case "exclusive-status":
		operation = "exclusive_status"
		if len(positional) > 0 {
			params["key"] = positional[0]
		}
	default:
		return nil, protocol.NewError(protocol.CodeCapabilityNotSupported,
			"unknown bridge method: "+command)
	}
	envelope := map[string]any{"op": operation, "params": params}
	if *timeout > 0 {
		envelope["timeoutSeconds"] = *timeout
	}
	requestID := ""
	if protocol.WriteOperation(operation) {
		requestID = a.newRequestID()
	}
	if requestID != "" {
		envelope["requestId"] = requestID
	}
	return a.daemonCallID(ctx, "call", envelope, requestID)
}
func (a *app) followEvents(ctx context.Context) error {
	signalCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client, failure := a.connectDaemon()
	if failure != nil {
		return failure
	}
	defer client.Close()
	replay, failure := client.Call(signalCtx, "events", map[string]any{"since": 0, "limit": 0})
	if failure != nil {
		return failure
	}
	object, _ := replay.(map[string]any)
	if events, ok := object["events"].([]any); ok {
		for _, event := range events {
			a.print(event)
		}
	}
	_, failure = client.Call(signalCtx, "subscribe", map[string]any{"events": []string{"*"}})
	if failure != nil {
		return failure
	}
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				return fmt.Errorf("daemon connection closed")
			}
			a.print(event.Data)
		case <-signalCtx.Done():
			return nil
		}
	}
}
