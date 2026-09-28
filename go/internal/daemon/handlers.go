package daemon

import (
	"context"
	"sort"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
	"github.com/guajun/mc-agent-bridge/internal/version"
)

// dispatch implements every local IPC method.
func (d *Daemon) dispatch(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
	if params == nil {
		params = map[string]any{}
	}
	switch method {
	case "ping":
		return map[string]any{"pong": true, "version": version.Version}, nil
	case "status":
		return d.status(), nil
	case "targets":
		return d.targetsPayload(), nil
	case "reload_targets":
		return d.reloadTargets()
	case "stop":
		go func() {
			time.Sleep(50 * time.Millisecond)
			d.Stop()
		}()
		return map[string]any{"stopping": true}, nil
	case "events":
		since := int64Param(params, "since", 0)
		limit := intParam(params, "limit", 200)
		category := stringParam(params, "category")
		return d.recentEvents(since, limit, category), nil
	case "requests":
		return map[string]any{
			"unknownWrites": d.unknown.List(),
		}, nil
	case "schema":
		if operation := stringParam(params, "op"); operation != "" {
			return schemaFor(operation)
		}
		return map[string]any{"operations": schemas(), "surfaceSchema": "toolkit-surface/1"}, nil
	case "call":
		operation := stringParam(params, "op")
		if operation == "" {
			return nil, protocol.NewError(protocol.CodeBadRequest, `call needs {"op": "...", "params": {...}}`)
		}
		inner, _ := params["params"].(map[string]any)
		return d.route(ctx, params, operation, inner)
	case "capabilities":
		return d.route(ctx, params, "capabilities", map[string]any{})
	}
	if protocol.KnownOperation(method) {
		inner := make(map[string]any, len(params))
		for key, value := range params {
			if key == "target" {
				continue
			}
			inner[key] = value
		}
		return d.route(ctx, params, method, inner)
	}
	return nil, &protocol.Error{Code: protocol.CodeCapabilityNotSupported,
		Message: "unknown bridge method: " + method}
}

// route resolves the target and runs the operation through the session.
func (d *Daemon) route(ctx context.Context, envelope map[string]any, operation string,
	params map[string]any) (any, *protocol.Error) {
	targetName := stringParam(envelope, "target")
	if params == nil {
		params = map[string]any{}
	}
	if timeout := numberParam(envelope, "timeoutSeconds", 0); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
		defer cancel()
	}
	return d.call(ctx, targetName, operation, params)
}

func (d *Daemon) targetsPayload() map[string]any {
	document := d.targetsSnapshot()
	targets := make([]map[string]any, 0, len(document.Targets))
	for _, name := range config.TargetNames(document) {
		target := document.Targets[name]
		entry := map[string]any{
			"name":      name,
			"transport": target.Transport,
			"address":   target.Address,
			"default":   name == document.Default,
		}
		if target.Pin != "" {
			entry["pin"] = target.Pin
		}
		if target.CAFile != "" {
			entry["caFile"] = target.CAFile
		}
		if target.TokenEnv != "" {
			entry["tokenEnv"] = target.TokenEnv
		}
		if target.TokenFile != "" {
			entry["tokenFile"] = target.TokenFile
		}
		if target.PortFile != "" {
			entry["portFile"] = target.PortFile
		}
		if target.ServerDir != "" {
			entry["serverDir"] = target.ServerDir
		}
		if target.Vantage != "" {
			entry["vantage"] = target.Vantage
		}
		d.sessionsMu.RLock()
		targetSession := d.sessions[name]
		d.sessionsMu.RUnlock()
		if targetSession != nil {
			targetSession.mu.Lock()
			state := targetSession.state
			targetSession.mu.Unlock()
			entry["connected"] = state.Connected
			if state.LastError != "" {
				entry["lastError"] = state.LastError
			}
			if state.LastErrorCode != "" {
				entry["lastErrorCode"] = state.LastErrorCode
			}
			entry["instanceId"] = state.InstanceID
			entry["runId"] = state.RunID
		}
		// Never print a token: only how it is sourced.
		_, source, tokenErr := config.Token(d.home, target)
		entry["credential"] = source
		if tokenErr != nil {
			entry["credentialError"] = tokenErr.Error()
		}
		targets = append(targets, entry)
	}
	return map[string]any{
		"targets": targets,
		"default": document.Default,
		"count":   len(targets),
	}
}

func (d *Daemon) reloadTargets() (any, *protocol.Error) {
	document, err := config.LoadTargets(d.home)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	d.sessionsMu.Lock()
	for _, targetSession := range d.sessions {
		targetSession.stop()
	}
	d.sessions = map[string]*targetSession{}
	d.sessionsMu.Unlock()
	d.targetsMu.Lock()
	d.targets = document
	d.targetsMu.Unlock()
	d.startAllSessions()
	return map[string]any{"reloaded": true, "targets": config.TargetNames(document)}, nil
}

func schemas() []map[string]any {
	operations := make([]map[string]any, 0, len(protocol.Operations))
	for _, operation := range protocol.Operations {
		entry, _ := schemaFor(operation.Name)
		operations = append(operations, entry)
	}
	sort.Slice(operations, func(i, j int) bool {
		return operations[i]["op"].(string) < operations[j]["op"].(string)
	})
	return operations
}

func schemaFor(operation string) (map[string]any, *protocol.Error) {
	definition, ok := protocol.OperationByName(operation)
	if !ok {
		return nil, protocol.NewError(protocol.CodeCapabilityNotSupported, "unknown operation: "+operation)
	}
	parameters := map[string]any{}
	required := []string{}
	add := func(name, kind, description string, isRequired bool) {
		parameters[name] = map[string]any{"type": kind, "description": description}
		if isRequired {
			required = append(required, name)
		}
	}
	switch operation {
	case "player":
		add("player", "string", "player name or UUID", false)
	case "entities":
		add("radius", "number", "optional radius around the first player; 0 means everything", false)
	case "context":
		add("contextId", "string", "chat-time context id", true)
	case "command", "command_output":
		add("command", "string", "command line without a leading slash", true)
		if operation == "command_output" {
			add("wait", "number", "seconds to collect command output from game events", false)
		}
	case "chat":
		add("message", "string", "chat text", true)
	case "record_start":
		add("ticks", "integer", "number of client ticks to sample", false)
		add("radius", "number", "sample radius", false)
		add("interval", "integer", "tick interval between samples", false)
	case "record_stop":
	case "mark":
		add("text", "string", "marker text", false)
	case "wait":
		add("ticks", "integer", "ticks to wait", true)
	case "connect":
		add("address", "string", "host:port", true)
	case "world":
		add("level", "string", "save folder name", true)
	case "lan":
		add("port", "integer", "LAN port; 0 picks one", false)
		add("mode", "string", "online or offline", false)
	case "snapshot":
		add("radius", "number", "entity radius; 0 means everything", false)
		add("name", "string", "snapshot directory name", false)
		add("dimension", "string", "dimension id, defaults to the primary level", false)
	case "request_status":
		add("requestId", "string", "the request id to resolve", true)
	case "exclusive_acquire", "exclusive_renew":
		add("key", "string", "lease key", true)
		add("ttlSeconds", "integer", "lease lifetime seconds", false)
		add("label", "string", "human label for the holder", false)
	case "exclusive_release":
		add("key", "string", "lease key", true)
	case "exclusive_status":
		add("key", "string", "optional lease key", false)
	case "events":
		add("since", "integer", "last seen daemon sequence", false)
		add("limit", "integer", "maximum events", false)
		add("category", "string", "category filter", false)
	}
	entry := map[string]any{
		"op":          operation,
		"description": definition.Description,
		"requires":    definition.Requires,
		"write":       definition.Write,
		"vantage":     definition.Vantage,
		"parameters":  parameters,
		"required":    required,
		"supported":   definition.Unsupported == "",
	}
	if definition.Unsupported != "" {
		entry["unsupportedReason"] = definition.Unsupported
	}
	return entry, nil
}

func stringParam(params map[string]any, name string) string {
	if value, ok := params[name].(string); ok {
		return value
	}
	return ""
}

func intParam(params map[string]any, name string, fallback int) int {
	switch value := params[name].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	}
	return fallback
}

func int64Param(params map[string]any, name string, fallback int64) int64 {
	switch value := params[name].(type) {
	case float64:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	}
	return fallback
}

func numberParam(params map[string]any, name string, fallback float64) float64 {
	switch value := params[name].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	}
	return fallback
}
