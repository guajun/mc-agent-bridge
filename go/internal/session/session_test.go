package session_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/fakemod"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
	"github.com/guajun/mc-agent-bridge/internal/session"
)

func startLegacy(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		writer := bufio.NewWriter(connection)
		writer.WriteString(`{"type":"hello","version":"0.7.0","instance":"client",` +
			`"capabilities":["state","command","chat","snapshot","connect","events:game"]}` + "\n")
		writer.Flush()
		reader := bufio.NewReader(connection)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			switch {
			case line == "STATE":
				writer.WriteString(`{"type":"state","levelName":"legacy"}` + "\n")
			case strings.HasPrefix(line, "CMD "):
				writer.WriteString(`{"type":"cmd_ack","detail":"x"}` + "\n")
			case strings.HasPrefix(line, "CHAT "):
				writer.WriteString(`{"type":"chat_ack","detail":"sent"}` + "\n")
			case strings.HasPrefix(line, "SNAPSHOT"):
				writer.WriteString(`{"type":"snapshot_ack","id":"s1","entities":2}` + "\n")
			default:
				writer.WriteString(`{"type":"error","message":"unknown ` + line + `"}` + "\n")
			}
			writer.Flush()
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().String()
}

func TestLegacySessionMappingAndGating(t *testing.T) {
	address := startLegacy(t)
	target := &config.Target{Name: "legacy", Transport: protocol.TransportLegacy, Address: address}
	adapter, err := session.DialLegacy(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx := context.Background()

	result, failure := adapter.Call(ctx, "state", nil)
	if failure != nil {
		t.Fatal(failure)
	}
	if object, _ := result.(map[string]any); object["levelName"] != "legacy" {
		t.Fatalf("state = %v", result)
	}

	if _, failure = adapter.Call(ctx, "command", map[string]any{"command": "say hi"}); failure != nil {
		t.Fatal(failure)
	}
	if _, failure = adapter.Call(ctx, "chat", map[string]any{"message": "hello"}); failure != nil {
		t.Fatal(failure)
	}
	result, failure = adapter.Call(ctx, "snapshot", map[string]any{"radius": 8.0})
	if failure != nil {
		t.Fatal(failure)
	}
	if object, _ := result.(map[string]any); object["entities"].(float64) != 2 {
		t.Fatalf("snapshot = %v", result)
	}

	// Capability gate: entities is not advertised.
	if _, failure = adapter.Call(ctx, "entities", nil); failure == nil ||
		failure.Code != protocol.CodeCapabilityNotSupported {
		t.Fatalf("entities should be refused: %v", failure)
	}
	// The legacy transport cannot recover unknown writes or hold leases.
	if _, failure = adapter.Call(ctx, "request_status", map[string]any{"requestId": "x"}); failure == nil || failure.Code != protocol.CodeCapabilityNotSupported {
		t.Fatalf("request_status should be refused: %v", failure)
	}
	if _, failure = adapter.Call(ctx, "fork", nil); failure == nil ||
		failure.Code != protocol.CodeCapabilityNotSupported {
		t.Fatalf("fork should be refused: %v", failure)
	}
}

func TestRemoteSessionClientOnlyOperationsAreRefused(t *testing.T) {
	fake, err := fakemod.Start(fakemod.Options{Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	target := &config.Target{Name: "remote", Transport: protocol.TransportRemote,
		Address: fake.Address(), Pin: fake.Pin()}
	adapter, err := session.DialRemote(context.Background(), target, "tok", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if _, failure := adapter.Call(context.Background(), "chat", map[string]any{"message": "hi"}); failure == nil || failure.Code != protocol.CodeCapabilityNotSupported {
		t.Fatalf("chat should be refused on the server vantage: %v", failure)
	}
	if _, failure := adapter.Call(context.Background(), "restore", nil); failure == nil || failure.Code != protocol.CodeCapabilityNotSupported {
		t.Fatalf("restore should be refused as unmigrated: %v", failure)
	}
	// capabilities is answered locally from the negotiated welcome.
	result, failure := adapter.Call(context.Background(), "capabilities", nil)
	if failure != nil {
		t.Fatal(failure)
	}
	if object, _ := result.(map[string]any); object["transport"] != protocol.TransportRemote {
		t.Fatalf("capabilities = %v", result)
	}
}

var _ = time.Second
