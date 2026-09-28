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
	var keyValues multiFlag
	flags.Var(&keyValues, "param", "key=value parameter (repeatable)")
	if err := flags.Parse(rest); err != nil {
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
	return a.daemonCall(ctx, "call", envelope)
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
func (a *app) cmdEvents(ctx context.Context, args []string) (any, *protocol.Error) {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	since := flags.Int64("since", 0, "last seen daemon sequence")
	limit := flags.Int("limit", 200, "maximum events to replay")
	category := flags.String("category", "", "category filter")
	follow := flags.Bool("follow", false, "keep streaming new events")
	if err := flags.Parse(args); err != nil {
		return nil, protocol.NewError(protocol.CodeUsage, err.Error())
	}
	client, failure := a.connectDaemon()
	if failure != nil {
		return nil, failure
	}
	defer client.Close()
	params := map[string]any{"since": *since, "limit": *limit}
	if *category != "" {
		params["category"] = *category
	}
	replay, failure := client.Call(ctx, "events", params)
	if failure != nil {
		return nil, failure
	}
	if !*follow {
		return replay, nil
	}
	object, _ := replay.(map[string]any)
	if events, ok := object["events"].([]any); ok {
		for _, event := range events {
			a.print(event)
		}
	}
	if _, failure := client.Call(ctx, "subscribe", map[string]any{"events": []string{"*"}}); failure != nil {
		return nil, failure
	}
	streamId, _ := object["streamId"].(string)
	a.print(map[string]any{"type": "stream", "event": "following", "data": map[string]any{
		"streamId": streamId, "next": object["next"], "dropped": object["dropped"]}})
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				return nil, protocol.NewError(protocol.CodeConnectionLost, "the daemon connection closed")
			}
			a.print(event.Data)
		case <-ctx.Done():
			return nil, nil
		}
	}
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
	if err := flags.Parse(args); err != nil {
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
	return a.daemonCall(ctx, "call", envelope)
}

// followEvents is kept for scripts that expect a blocking stream API.
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
