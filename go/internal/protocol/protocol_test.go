package protocol

import (
	"testing"
	"time"
)

func TestOperationSurface(t *testing.T) {
	surface := Surface("server", []string{"state", "entities", "command", "events:game"}, "server")
	supported := surface["supported"].([]string)
	contains := func(name string) bool {
		for _, entry := range supported {
			if entry == name {
				return true
			}
		}
		return false
	}
	if !contains("state") || !contains("command") {
		t.Fatalf("supported = %v", supported)
	}
	if contains("chat") {
		t.Fatalf("chat is client-only and must not be supported on a server vantage: %v", supported)
	}
	if contains("fork") {
		t.Fatalf("fork is deliberately unsupported: %v", supported)
	}
	unsupported := surface["unsupported"].([]string)
	for _, expected := range []string{"fork", "restore", "verify", "order", "player"} {
		found := false
		for _, entry := range unsupported {
			if entry == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s missing from unsupported: %v", expected, unsupported)
		}
	}
}

func TestCapabilityAliasesAndWriteDetection(t *testing.T) {
	if !HasCapability([]string{"player:view"}, "player") {
		t.Fatal("player:view must satisfy the player capability")
	}
	if !HasCapability([]string{"context_bundle"}, "context") {
		t.Fatal("context_bundle must satisfy the context capability")
	}
	if HasCapability([]string{"state"}, "player") {
		t.Fatal("state must not satisfy player")
	}
	if HasCapability(nil, "anything") == false {
		t.Fatal("a legacy mod with unknown capabilities must not be gated")
	}
	for _, write := range []string{"command", "chat", "mark", "snapshot"} {
		if !WriteOperation(write) {
			t.Fatalf("%s must be a write operation", write)
		}
	}
	for _, read := range []string{"state", "entities", "events", "request_status"} {
		if WriteOperation(read) {
			t.Fatalf("%s must not be a write operation", read)
		}
	}
}

func TestErrorExitCodes(t *testing.T) {
	cases := map[string]int{
		"":                         0,
		CodeUsage:                  2,
		CodeTargetRequired:         3,
		CodeTargetUnknown:          3,
		CodeConnectionFailed:       4,
		CodeDaemonNotRunning:       4,
		CodeUnauthorized:           5,
		CodeCapabilityNotSupported: 6,
		CodeTimeout:                7,
		CodeResultUnknown:          7,
		"something-else":           1,
	}
	for code, expected := range cases {
		if got := ExitCode(code); got != expected {
			t.Fatalf("ExitCode(%q) = %d, want %d", code, got, expected)
		}
	}
}

func TestEventCategorization(t *testing.T) {
	cases := map[string]string{
		"chat": "chat", "game": "game", "mark": "mark", "hello": "hello",
		"sample_start": "sample", "context_error": "error", "weird": "other",
		"event_gap": "error",
	}
	for eventType, expected := range cases {
		if got := Categorize(eventType); got != expected {
			t.Fatalf("Categorize(%q) = %q, want %q", eventType, got, expected)
		}
	}
}

var _ = time.Second
