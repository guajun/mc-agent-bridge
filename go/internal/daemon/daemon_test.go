package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/daemon"
	"github.com/guajun/mc-agent-bridge/internal/fakemod"
	"github.com/guajun/mc-agent-bridge/internal/ipc"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
	"github.com/guajun/mc-agent-bridge/internal/webhook"
)

const fakeToken = "mca1.tk_fake.secret"

type harness struct {
	t       *testing.T
	home    string
	fake    *fakemod.Server
	cancel  context.CancelFunc
	done    chan error
	stopped bool
	address string
	token   string
}

// startHarness boots a fake mod, a two-target config and a real daemon.
func startHarness(t *testing.T, scripted map[string]fakemod.OpFunc, webhookConfig *webhook.Config) *harness {
	t.Helper()
	fake, err := fakemod.Start(fakemod.Options{Token: fakeToken, Ops: scripted})
	if err != nil {
		t.Fatalf("cannot start the fake mod: %v", err)
	}
	t.Cleanup(fake.Close)
	return startHarnessWith(t, fake, fakeToken, webhookConfig)
}

// startHarnessWith runs a daemon against an existing fake mod and token, in a
// fresh private state directory.
func startHarnessWith(t *testing.T, fake *fakemod.Server, token string, webhookConfig *webhook.Config) *harness {
	return startHarnessHome(t, "", fake, token, webhookConfig)
}

// startHarnessHome is startHarnessWith with an optional existing state dir.
func startHarnessHome(t *testing.T, home string, fake *fakemod.Server, token string,
	webhookConfig *webhook.Config) *harness {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	document := &config.Targets{Version: 1, Targets: map[string]*config.Target{
		"fake": {Name: "fake", Transport: protocol.TransportRemote, Address: fake.Address(), Pin: fake.Pin()},
		"legacy-local": {Name: "legacy-local", Transport: protocol.TransportLegacy,
			PortFile: filepath.Join(home, "missing-port.txt"), Vantage: "client"},
	}}
	if err := config.SaveTargets(home, document); err != nil {
		t.Fatal(err)
	}
	secrets := &config.Secrets{Version: 1, Tokens: map[string]string{"fake": token}}
	if err := config.SaveSecrets(home, secrets); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{
			Home:           home,
			ReconnectDelay: 150 * time.Millisecond,
			BufferSize:     200,
			Webhook:        webhookConfig,
			Logger:         func(format string, args ...any) { t.Logf("daemon: "+format, args...) },
		})
	}()
	h := &harness{t: t, home: home, fake: fake, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		if h.stopped {
			return
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	h.waitForState()
	return h
}

// stop waits for the daemon to end and records that the channel was consumed.
func (h *harness) stop() error {
	if h.stopped {
		return nil
	}
	h.stopped = true
	select {
	case err := <-h.done:
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("daemon did not stop")
		return nil
	}
}

func (h *harness) waitForState() {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, err := config.LoadDaemonState(h.home)
		if err == nil && state != nil {
			h.address, h.token = state.Address, state.Token
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatal("daemon state file never appeared")
}

func (h *harness) client() *ipc.Client {
	h.t.Helper()
	client, err := ipc.Dial(context.Background(), h.address, h.token)
	if err != nil {
		h.t.Fatalf("cannot dial the daemon: %v", err)
	}
	return client
}

func (h *harness) call(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
	client := h.client()
	defer client.Close()
	return client.Call(ctx, method, params)
}

func (h *harness) waitForTargetConnected() {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		result, failure := h.call(context.Background(), "status", nil)
		if failure == nil {
			object, _ := result.(map[string]any)
			if targets, ok := object["targets"].([]any); ok {
				for _, entry := range targets {
					target, _ := entry.(map[string]any)
					if target["name"] == "fake" && target["connected"] == true {
						return
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatal("the fake target never connected")
}

func TestDaemonCallsEventsAndTargetSelection(t *testing.T) {
	h := startHarness(t, nil, nil)
	h.waitForTargetConnected()
	ctx := context.Background()

	// Explicit target works.
	result, failure := h.call(ctx, "call", map[string]any{
		"target": "fake", "op": "state", "params": map[string]any{}})
	if failure != nil {
		t.Fatalf("state failed: %v", failure)
	}
	if !strings.Contains(fmt.Sprint(result), "fake-world") {
		t.Fatalf("state = %v", result)
	}

	// Without a target and two configured targets, the daemon refuses to guess.
	_, failure = h.call(ctx, "call", map[string]any{"op": "state", "params": map[string]any{}})
	if failure == nil || failure.Code != protocol.CodeTargetRequired {
		t.Fatalf("expected target_required, got %v", failure)
	}

	// Command output flows through the daemon.
	result, failure = h.call(ctx, "call", map[string]any{
		"target": "fake", "op": "command_output",
		"params": map[string]any{"command": "say hi", "wait": 0.2}})
	if failure != nil {
		t.Fatalf("command-output failed: %v", failure)
	}
	object, _ := result.(map[string]any)
	if output, ok := object["output"].([]any); !ok || len(output) == 0 {
		t.Fatalf("command output = %v", result)
	}

	// Subscribe only to the category under test: background connection events
	// may arrive at any time and do not establish a mark-delivery failure.
	client := h.client()
	defer client.Close()
	if _, failure = client.Call(ctx, "subscribe", map[string]any{"events": []string{"mark"}}); failure != nil {
		t.Fatal(failure)
	}
	h.fake.Push("game", map[string]any{"text": "unrelated-event"})
	h.fake.Push("mark", map[string]any{"text": "from-fake"})
	select {
	case event := <-client.Events():
		if event.Event != "mark" {
			t.Fatalf("event category = %s", event.Event)
		}
		if event.Data["text"] != "from-fake" {
			t.Fatalf("event payload = %v", event.Data)
		}
		if event.Data["streamId"] == nil || event.Data["eventId"] == nil {
			t.Fatalf("event has no stream identity: %v", event.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived over the IPC")
	}

	// The replay method sees the same event.
	replay, failure := h.call(ctx, "events", map[string]any{"since": 0, "limit": 50})
	if failure != nil {
		t.Fatal(failure)
	}
	if !strings.Contains(fmt.Sprint(replay), "from-fake") {
		t.Fatalf("replay = %v", replay)
	}
}

func TestDaemonReconnectAndUnknownWriteReconciliation(t *testing.T) {
	// command blocks long enough for the daemon-side timeout to fire.
	commandStarted := make(chan struct{}, 4)
	scripted := map[string]fakemod.OpFunc{
		"command": func(params map[string]any) (any, *protocol.Error) {
			commandStarted <- struct{}{}
			time.Sleep(1200 * time.Millisecond)
			return map[string]any{"type": "cmd_ack", "detail": params["command"],
				"output": []any{"late-output"}}, nil
		},
	}
	h := startHarness(t, scripted, nil)
	h.waitForTargetConnected()
	ctx := context.Background()

	result, failure := h.call(ctx, "call", map[string]any{
		"target": "fake", "op": "command", "params": map[string]any{"command": "slow"},
		"timeoutSeconds": 0.2})
	_ = result
	if failure == nil {
		t.Fatal("the slow command should have timed out on the daemon side")
	}
	if failure.Code != protocol.CodeTimeout {
		t.Fatalf("error = %v", failure)
	}
	if !failure.ResultUnknown || failure.RequestID == "" {
		t.Fatalf("timeout must report resultUnknown with a request id: %v", failure)
	}
	requestID := failure.RequestID
	select {
	case <-commandStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the command never reached the fake mod")
	}

	// The ledger records the unknown write.
	list, failure := h.call(ctx, "requests", nil)
	if failure != nil {
		t.Fatal(failure)
	}
	if !strings.Contains(fmt.Sprint(list), requestID) {
		t.Fatalf("unknown write not recorded: %v", list)
	}

	// Wait for the server-side completion, then drop the link; the daemon's
	// reconnect reconciliation must resolve the write without retrying it.
	time.Sleep(1500 * time.Millisecond)
	h.fake.DisconnectAll()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		list, failure = h.call(ctx, "requests", nil)
		if failure == nil && !strings.Contains(fmt.Sprint(list), requestID) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if strings.Contains(fmt.Sprint(list), requestID) {
		t.Fatalf("unknown write was never resolved: %v", list)
	}
	events, _ := h.call(ctx, "events", map[string]any{"since": 0, "limit": 100})
	if !strings.Contains(fmt.Sprint(events), "request_resolved") {
		t.Fatalf("no request_resolved event: %v", events)
	}
	if !strings.Contains(fmt.Sprint(events), "bridge_connected") {
		t.Fatalf("no reconnect event: %v", events)
	}
}

func TestDaemonIPCAuthentication(t *testing.T) {
	h := startHarness(t, nil, nil)
	h.waitForTargetConnected()
	if _, err := ipc.Dial(context.Background(), h.address, "wrong-token"); err == nil {
		t.Fatal("a wrong IPC token must be refused")
	}
	state, err := config.LoadDaemonState(h.home)
	if err != nil || state == nil {
		t.Fatal("daemon state missing")
	}
	client, err := ipc.Dial(context.Background(), h.address, state.Token)
	if err != nil {
		t.Fatalf("the real token was refused: %v", err)
	}
	client.Close()
}

func TestDaemonWebhookForwarding(t *testing.T) {
	type delivery struct {
		signature string
		eventID   string
		body      string
	}
	deliveries := make(chan delivery, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := make([]byte, request.ContentLength)
		_, _ = request.Body.Read(body)
		deliveries <- delivery{
			signature: request.Header.Get(webhook.SignatureHeader),
			eventID:   request.Header.Get(webhook.EventIDHeader),
			body:      string(body),
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	config := &webhook.Config{URL: server.URL, Secret: "hook-secret", Events: []string{"mark"},
		QueueSize: 8, MaxAttempts: 3, Backoff: 10 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond, Timeout: time.Second}
	h := startHarness(t, nil, config)
	h.waitForTargetConnected()
	h.fake.Push("mark", map[string]any{"text": "webhook-me"})
	select {
	case item := <-deliveries:
		if item.signature == "" || item.eventID == "" {
			t.Fatalf("delivery = %+v", item)
		}
		if !strings.Contains(item.body, "webhook-me") {
			t.Fatalf("body = %s", item.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook never delivered")
	}
}

func TestDaemonStopRemovesState(t *testing.T) {
	h := startHarness(t, nil, nil)
	h.waitForTargetConnected()
	if _, failure := h.call(context.Background(), "stop", nil); failure != nil {
		t.Fatal(failure)
	}
	if err := h.stop(); err != nil {
		t.Fatalf("daemon exited with %v", err)
	}
	if state, _ := config.LoadDaemonState(h.home); state != nil {
		t.Fatal("daemon state file was not removed")
	}
}

func TestDaemonRejectsSecondInstance(t *testing.T) {
	h := startHarness(t, nil, nil)
	h.waitForTargetConnected()
	err := daemon.Run(context.Background(), daemon.Options{Home: h.home})
	if err == nil {
		t.Fatal("a second daemon must not start")
	}
	var protocolErr *protocol.Error
	if !errorsAs(err, &protocolErr) || protocolErr.Code != protocol.CodeDaemonAlreadyRunning {
		t.Fatalf("error = %v", err)
	}
}

func errorsAs(err error, target **protocol.Error) bool {
	typed, ok := err.(*protocol.Error)
	if ok {
		*target = typed
	}
	return ok
}
