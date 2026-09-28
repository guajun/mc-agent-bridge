// Package protocol is the harness-neutral contract shared by the daemon, the
// CLI, the remote control transport and the legacy local adapter.
//
// Operation names and capability gates mirror the Python bridge's toolkit
// surface (src/mc_agent_bridge/toolkit.py) so existing callers keep the same
// method names and capability semantics after the Go migration.
package protocol

import (
	"fmt"
	"sort"
	"strings"
)

// ControlProtocolVersion is the wire version of the authenticated same-port
// control protocol (mc-agent-interface-mod issue #8). It is negotiated in the
// hello/welcome frames.
const ControlProtocolVersion = 1

// Transport names. A target is either the new TLS control protocol or the
// explicit legacy loopback JSON-lines adapter.
const (
	TransportRemote = "remote"
	TransportLegacy = "legacy"
	TransportFake   = "fake"
)

// Operation is one toolkit operation, its capability requirements and where it
// may run.
type Operation struct {
	Name        string
	Description string
	// Requires lists the mod capability tokens the operation needs. The
	// remote control protocol gates on these server-side as well.
	Requires []string
	// Write marks non-idempotent operations: the daemon never replays them
	// automatically after a disconnect and reports result-unknown instead.
	Write bool
	// Vantage restricts the operation: "" for both, "server" or "client".
	Vantage string
	// Unsupported documents an operation this runtime deliberately refuses
	// (for example because the capability is not migrated yet).
	Unsupported string
}

const (
	VantageServer = "server"
	VantageClient = "client"
)

// Operations is the full surface. Names match the Python bridge exactly so the
// migration table can map one-for-one.
var Operations = []Operation{
	{Name: "ping", Description: "Liveness check against the daemon/target."},
	{Name: "status", Description: "Daemon and target connection state."},
	{Name: "capabilities", Description: "The connected mod's capabilities and the filtered surface."},
	{Name: "state", Description: "World/server state and the online player list.", Requires: []string{"state"}},
	{Name: "player", Description: "Server-side context for one player: identity, position, rotation, view target.",
		Requires: []string{"player"}},
	{Name: "entities", Description: "Entities the instance ticks, in tick order.", Requires: []string{"entities"}},
	{Name: "command", Description: "Run a command as the mod's command source.", Requires: []string{"command"}, Write: true},
	{Name: "command_output", Description: "Run a command and collect the answer it produced.", Requires: []string{"command"}, Write: true},
	{Name: "chat", Description: "Send a chat message as the client-vantage player.", Requires: []string{"chat"}, Write: true, Vantage: VantageClient},
	{Name: "record_start", Description: "Start per-tick entity sampling (client vantage).", Requires: []string{"record"}, Write: true, Vantage: VantageClient},
	{Name: "record_stop", Description: "Stop the running sampling session (client vantage).", Requires: []string{"record"}, Write: true, Vantage: VantageClient},
	{Name: "wait", Description: "Block until the game advanced N ticks.", Requires: []string{"wait"}},
	{Name: "screen", Description: "Current client GUI screen (client vantage only).", Requires: []string{"screen"}, Vantage: VantageClient},
	{Name: "mark", Description: "Annotate the event stream with a text marker.", Requires: []string{"mark"}, Write: true},
	{Name: "connect", Description: "Join a server (client vantage only).", Requires: []string{"connect"}, Write: true, Vantage: VantageClient},
	{Name: "world", Description: "Open a single-player save (client vantage only).", Requires: []string{"world"}, Write: true, Vantage: VantageClient},
	{Name: "lan", Description: "Publish the single-player world to the LAN (client vantage only).", Requires: []string{"lan"}, Write: true, Vantage: VantageClient},
	{Name: "events", Description: "Replay buffered events since a cursor."},
	{Name: "context", Description: "Retrieve a chat-time player context bundle by its context id.", Requires: []string{"context"}},
	{Name: "save", Description: "World-save metadata reported by STATE: level, world directory, players.", Requires: []string{"state"}},
	{Name: "snapshot", Description: "Write the entity set in tick order to the instance's disk.", Requires: []string{"snapshot"}, Write: true},
	{Name: "snapshots", Description: "List the snapshots already on the connected instance.", Requires: []string{"snapshot"}},
	{Name: "fork", Description: "Freeze, snapshot, copy the world files, resume.",
		Requires: []string{"snapshot", "command"}, Write: true,
		Unsupported: "fork needs the daemon host to read the game's world directory; the remote control protocol " +
			"does not expose remote filesystem paths (mc-agent#39). The Python bridge remains the explicit legacy path for local fork/restore."},
	{Name: "restore", Description: "Validate a fork and summon entities in recorded order.", Requires: []string{"command"}, Write: true,
		Unsupported: "restore reads snapshot files from the daemon host and is not migrated to the Go runtime yet; " +
			"it is refused as an unsupported capability instead of guessing at remote paths (mc-agent#39)."},
	{Name: "verify", Description: "Compare a fork with a fresh snapshot of this instance.", Requires: []string{"snapshot"},
		Unsupported: "verify depends on the local fork file layout and is not migrated to the Go runtime yet."},
	{Name: "order", Description: "Compare a fresh snapshot's order hash with a saved fork.", Requires: []string{"snapshot"},
		Unsupported: "order depends on the local fork file layout and is not migrated to the Go runtime yet."},
	{Name: "request_status", Description: "Resolve the outcome of an earlier write by request id."},
	{Name: "exclusive_acquire", Description: "Acquire or renew an exclusive key lease.", Write: true},
	{Name: "exclusive_renew", Description: "Renew an exclusive key lease.", Write: true},
	{Name: "exclusive_release", Description: "Release an exclusive key lease.", Write: true},
	{Name: "exclusive_status", Description: "Read exclusive lease holders."},
	{Name: "stop", Description: "Shut the daemon down."},
}

var operationByName = func() map[string]Operation {
	index := make(map[string]Operation, len(Operations))
	for _, operation := range Operations {
		index[operation.Name] = operation
	}
	return index
}()

// Operation returns the operation definition and whether it exists.
func OperationByName(name string) (Operation, bool) {
	operation, ok := operationByName[name]
	return operation, ok
}

// CapabilityAliases maps a canonical capability group onto every spelling a
// released mod has used. The remote control protocol uses the plain names;
// the legacy adapter keeps the Python bridge's leniency.
var CapabilityAliases = map[string][]string{
	"player":  {"player", "player:view", "player_context", "playercontext", "context:player"},
	"context": {"context", "context_bundle", "contextbundle", "chat_context", "context:chat"},
}

// HasCapability reports whether a capability is advertised, accepting aliases.
func HasCapability(capabilities []string, name string) bool {
	if capabilities == nil {
		return true // legacy mod: unknown, so do not gate
	}
	aliases, ok := CapabilityAliases[name]
	if !ok {
		aliases = []string{name}
	}
	for _, capability := range capabilities {
		for _, alias := range aliases {
			if strings.EqualFold(capability, alias) {
				return true
			}
		}
	}
	return false
}

// MissingCapabilities returns the required tokens the mod does not advertise.
func MissingCapabilities(operation Operation, capabilities []string) []string {
	if capabilities == nil {
		return nil
	}
	var missing []string
	for _, required := range operation.Requires {
		if !HasCapability(capabilities, required) {
			missing = append(missing, required)
		}
	}
	return missing
}

// Surface is the filtered operation list for one connection, shaped like the
// Python bridge's toolkit surface so harnesses can migrate without re-learning
// the payload.
func Surface(instance string, capabilities []string, vantage string) map[string]any {
	entries := make([]map[string]any, 0, len(Operations))
	supported := make([]string, 0, len(Operations))
	unsupported := make([]string, 0, len(Operations))
	for _, operation := range Operations {
		missing := MissingCapabilities(operation, capabilities)
		entry := map[string]any{
			"name":        operation.Name,
			"description": operation.Description,
			"requires":    operation.Requires,
			"write":       operation.Write,
			"supported":   len(missing) == 0 && operation.Unsupported == "",
		}
		if operation.Unsupported != "" {
			entry["reason"] = operation.Unsupported
			entry["unsupported"] = true
		}
		if len(missing) > 0 {
			entry["missing"] = missing
			entry["reason"] = fmt.Sprintf("the connected mod does not advertise %s", strings.Join(missing, ", "))
		}
		if operation.Vantage != "" && vantage != "" && operation.Vantage != vantage {
			entry["supported"] = false
			entry["reason"] = "this operation is only available from the " + operation.Vantage + " vantage"
		}
		if entry["supported"].(bool) {
			supported = append(supported, operation.Name)
		} else {
			unsupported = append(unsupported, operation.Name)
		}
		entries = append(entries, entry)
	}
	sort.Strings(supported)
	sort.Strings(unsupported)
	result := map[string]any{
		"instance":      instance,
		"vantage":       vantage,
		"operations":    entries,
		"supported":     supported,
		"unsupported":   unsupported,
		"capabilities":  capabilities,
		"protocol":      ControlProtocolVersion,
		"surfaceSchema": "toolkit-surface/1",
	}
	if capabilities == nil {
		result["note"] = "the mod did not advertise capabilities; the daemon is not filtering its surface"
	}
	return result
}

// WriteOperation reports whether an operation is non-idempotent.
func WriteOperation(name string) bool {
	operation, ok := operationByName[name]
	return ok && operation.Write
}

// KnownOperation reports whether the operation exists in the contract.
func KnownOperation(name string) bool {
	_, ok := operationByName[name]
	return ok
}

// Event categories mirror the Python bridge so webhook filters and subscribers
// keep working.
func Categorize(eventType string) string {
	switch eventType {
	case "hello":
		return "hello"
	case "chat":
		return "chat"
	case "game":
		return "game"
	case "mark":
		return "mark"
	case "error", "disconnected", "bridge_disconnected", "event_gap":
		return "error"
	}
	if strings.HasPrefix(eventType, "sample") {
		return "sample"
	}
	if strings.HasSuffix(eventType, "_error") {
		return "error"
	}
	return "other"
}
