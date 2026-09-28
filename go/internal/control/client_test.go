package control_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/control"
	"github.com/guajun/mc-agent-bridge/internal/fakemod"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

const testToken = "mca1.tk_test.secret"

func startFake(t *testing.T, options fakemod.Options) *fakemod.Server {
	t.Helper()
	if options.Token == "" {
		options.Token = testToken
	}
	server, err := fakemod.Start(options)
	if err != nil {
		t.Fatalf("cannot start the fake mod: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

func TestHandshakeCallAndEvents(t *testing.T) {
	server := startFake(t, fakemod.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, welcome, err := control.Dial(ctx, control.Config{
		Address: server.Address(),
		Pin:     server.Pin(),
		Token:   testToken,
	})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer client.Close()
	if welcome.Protocol != protocol.ControlProtocolVersion {
		t.Fatalf("welcome protocol = %d", welcome.Protocol)
	}
	if welcome.InstanceID == "" || welcome.RunID == "" || welcome.SessionID == "" {
		t.Fatalf("welcome is missing identity: %+v", welcome)
	}
	if !strings.Contains(strings.Join(welcome.Capabilities, ","), "state") {
		t.Fatalf("welcome capabilities = %v", welcome.Capabilities)
	}

	result, failure := client.Call(ctx, "state", nil)
	if failure != nil {
		t.Fatalf("state failed: %v", failure)
	}
	if !strings.Contains(string(result), "fake-world") {
		t.Fatalf("state result = %s", result)
	}

	server.Push("mark", map[string]any{"text": "hello"})
	select {
	case event := <-client.Events():
		if event.Payload == nil || event.Seq != 1 {
			t.Fatalf("unexpected event: %+v", event)
		}
		if text, _ := event.Map()["text"].(string); text != "hello" {
			t.Fatalf("event payload = %v", event.Map())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event was delivered")
	}
}

func TestPinMismatchIsRejected(t *testing.T) {
	server := startFake(t, fakemod.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := control.Dial(ctx, control.Config{
		Address: server.Address(),
		Pin:     "sha256:" + strings.Repeat("00", 32),
		Token:   testToken,
	})
	if err == nil {
		t.Fatal("a wrong pin must fail the handshake")
	}
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) {
		t.Fatalf("error is not a protocol error: %T", err)
	}
	if !strings.Contains(protocolErr.Message, "pin mismatch") {
		t.Fatalf("error message = %q", protocolErr.Message)
	}
}

func TestCAFileVerification(t *testing.T) {
	server := startFake(t, fakemod.Options{})
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := writeFile(caFile, server.CertPEM()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := control.Dial(ctx, control.Config{
		Address:    server.Address(),
		CAFile:     caFile,
		ServerName: "localhost",
		Token:      testToken,
	})
	if err != nil {
		t.Fatalf("CA-verified dial failed: %v", err)
	}
	client.Close()
}

func TestWrongTokenIsRefused(t *testing.T) {
	server := startFake(t, fakemod.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := control.Dial(ctx, control.Config{
		Address: server.Address(),
		Pin:     server.Pin(),
		Token:   "mca1.tk_other.wrong",
	})
	if err == nil {
		t.Fatal("a wrong token must be refused")
	}
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.CodeUnauthorized {
		t.Fatalf("error = %v", err)
	}
}

func TestReplayAndGap(t *testing.T) {
	server := startFake(t, fakemod.Options{EventBuffer: 2})
	server.Push("mark", map[string]any{"text": "one"})
	server.Push("mark", map[string]any{"text": "two"})
	server.Push("mark", map[string]any{"text": "three"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, welcome, err := control.Dial(ctx, control.Config{
		Address: server.Address(), Pin: server.Pin(), Token: testToken, LastSeq: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if welcome.Replay.From != 2 {
		t.Fatalf("replay from = %d, want 2", welcome.Replay.From)
	}
	var replayed []int64
	deadline := time.After(5 * time.Second)
	for len(replayed) < 2 {
		select {
		case event := <-client.Events():
			if !event.Replay {
				t.Fatalf("event %d is not marked replay", event.Seq)
			}
			replayed = append(replayed, event.Seq)
		case <-deadline:
			t.Fatalf("only got %d replayed events", len(replayed))
		}
	}

	// Ask for the whole (tiny) buffer: the oldest events are gone.
	client2, welcome2, err := control.Dial(ctx, control.Config{
		Address: server.Address(), Pin: server.Pin(), Token: testToken, LastSeq: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	if !welcome2.Replay.Lost {
		t.Fatalf("expected a reported gap, got %+v", welcome2.Replay)
	}
}

func TestConnectionLossFailsPendingCalls(t *testing.T) {
	server := startFake(t, fakemod.Options{
		Ops: map[string]fakemod.OpFunc{
			"wait": func(params map[string]any) (any, *protocol.Error) {
				time.Sleep(2 * time.Second)
				return map[string]any{"type": "wait_ack"}, nil
			},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := control.Dial(ctx, control.Config{Address: server.Address(), Pin: server.Pin(), Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan *protocol.Error, 1)
	go func() {
		_, failure := client.Call(ctx, "wait", map[string]any{"ticks": 40})
		result <- failure
	}()
	time.Sleep(200 * time.Millisecond)
	server.DisconnectAll()
	select {
	case failure := <-result:
		if failure == nil {
			t.Fatal("the call should have failed after the disconnect")
		}
		if failure.Code != protocol.CodeConnectionLost {
			t.Fatalf("error code = %s", failure.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call did not fail")
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
