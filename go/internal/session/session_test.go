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
				writer.WriteString(`{"type":"snapshot_ack","schema":"entity-nbt/1","id":"s1","entities":2}` + "\n")
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
	if _, failure = adapter.Call(ctx, "snapshot", map[string]any{"name": "s1"}); failure == nil || failure.Code != protocol.CodeCapabilityNotSupported {
		t.Fatalf("old/client snapshot must not be treated as new server NBT: %v", failure)
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
	adapter, err := session.DialRemote(context.Background(), target, "tok", 0, "")
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

func TestRemoteEntityNbtContractAndRadiusRejection(t *testing.T) {
	const nbt = `{Motion:[0.1d,0.0d,0.0d],Health:10.0f}`
	fake, err := fakemod.Start(fakemod.Options{Token: "tok", Ops: map[string]fakemod.OpFunc{
		"entities": func(params map[string]any) (any, *protocol.Error) {
			if _, exists := params["radius"]; exists {
				return nil, protocol.NewError(protocol.CodeBadRequest, "radius filtering was removed")
			}
			if params["dimension"] != "minecraft:the_nether" {
				return nil, protocol.NewError(protocol.CodeBadRequest, "dimension was not forwarded")
			}
			return map[string]any{"type": "entities", "schema": "entity-nbt/1", "dimension": params["dimension"], "orderHash": "fixture-hash", "entities": []any{map[string]any{"order": 0, "uuid": "fixture", "type": "minecraft:cow", "pos": []float64{1, 2, 3}, "vel": []float64{0.1, 0, 0}, "nbt": nbt, "passengers": []string{}, "vehicle": nil, "restorable": true}}}, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	target := &config.Target{Name: "remote", Transport: protocol.TransportRemote, Address: fake.Address(), Pin: fake.Pin()}
	adapter, err := session.DialRemote(context.Background(), target, "tok", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	for _, op := range []string{"entities", "snapshot"} {
		result, failure := adapter.Call(context.Background(), op, map[string]any{"dimension": "minecraft:the_nether"})
		if failure != nil {
			t.Fatal(failure)
		}
		object := result.(map[string]any)
		if object["schema"] != "entity-nbt/1" || object["dimension"] != "minecraft:the_nether" {
			t.Fatalf("%s: %v", op, object)
		}
		if op == "entities" {
			record := object["entities"].([]any)[0].(map[string]any)
			if record["nbt"] != nbt || record["order"] != float64(0) || record["restorable"] != true {
				t.Fatalf("lost NBT/order: %v", record)
			}
		}

		if _, failure := adapter.Call(context.Background(), op, map[string]any{"radius": 0}); failure == nil || failure.Code != protocol.CodeBadRequest {
			t.Fatalf("%s accepted removed radius: %v", op, failure)
		}
	}
}

func TestRemoteCloseWithFullEventChannelIsBounded(t *testing.T) {
	fake, err := fakemod.Start(fakemod.Options{Token: "tok", EventBuffer: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	target := &config.Target{Name: "remote", Transport: protocol.TransportRemote,
		Address: fake.Address(), Pin: fake.Pin()}
	remote, err := session.DialRemote(context.Background(), target, "tok", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	// Never consume remote.Events(): both queues fill up. Push from another
	// goroutine so a stalled socket cannot block the test before Close.
	go func() {
		for index := 0; index < 3000; index++ {
			fake.Push("mark", map[string]any{"text": "flood"})
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(remote.Events()) < 1000 {
		time.Sleep(20 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		remote.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Remote.Close deadlocked with full event queues")
	}
}

func TestLegacyWriteCancellationIsResultUnknown(t *testing.T) {
	// The legacy server never answers CMD slow; cancel the call.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		writer := bufio.NewWriter(connection)
		writer.WriteString(`{"type":"hello","instance":"server","capabilities":["command"]}` + "\n")
		writer.Flush()
		reader := bufio.NewReader(connection)
		for {
			if _, err := reader.ReadString('\n'); err != nil {
				return
			}
			// deliberately no reply
		}
	}()
	target := &config.Target{Name: "legacy", Transport: protocol.TransportLegacy,
		Address: listener.Addr().String()}
	adapter, err := session.DialLegacy(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, failure := adapter.CallID(ctx, "legacy-test-1", "command", map[string]any{"command": "slow"})
	if failure == nil {
		t.Fatal("the cancelled legacy write must fail")
	}
	if failure.RequestID != "legacy-test-1" {
		t.Fatalf("request id lost: %+v", failure)
	}
	if !failure.ResultUnknown {
		t.Fatalf("a cancelled legacy write may have run and must be result-unknown: %+v", failure)
	}
}
