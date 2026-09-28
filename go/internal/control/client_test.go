package control_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestDialTimeoutWhenServerNeverWelcomes(t *testing.T) {
	server := startFake(t, fakemod.Options{SilentHello: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := control.Dial(ctx, control.Config{
		Address:     server.Address(),
		Pin:         server.Pin(),
		Token:       testToken,
		DialTimeout: 500 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a server that never sends welcome must fail the dial")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("dial took %s; the hello/welcome deadline did not apply", elapsed)
	}
}

func TestRequestIDsAreUniqueAcrossClientsAndReconnects(t *testing.T) {
	server := startFake(t, fakemod.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, _, err := control.Dial(ctx, control.Config{Address: server.Address(), Pin: server.Pin(), Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	idA := first.NextRequestID()
	idB := first.NextRequestID()
	if idA == idB {
		t.Fatalf("ids within one client must differ: %s", idA)
	}
	first.Close()

	second, _, err := control.Dial(ctx, control.Config{Address: server.Address(), Pin: server.Pin(), Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	idC := second.NextRequestID()
	second.Close()
	if idC == idA || idC == idB {
		t.Fatalf("a reconnect reused an id: %s vs %s/%s", idC, idA, idB)
	}

	third, _, err := control.Dial(ctx, control.Config{Address: server.Address(), Pin: server.Pin(), Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	idD := third.NextRequestID()
	third.Close()
	if idD == idC {
		t.Fatalf("two client instances produced the same id: %s", idD)
	}
}

func TestCloseWithFullEventChannelIsBounded(t *testing.T) {
	server := startFake(t, fakemod.Options{EventBuffer: 4096})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, _, err := control.Dial(ctx, control.Config{Address: server.Address(), Pin: server.Pin(), Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	// Never consume Events(): the 1024-slot channel fills and the read loop
	// must still be able to abandon delivery on Close. The producer must run
	// in its own goroutine: pushing from the test goroutine would block on the
	// stalled socket before Close is ever called (the test hung on CI Windows
	// exactly that way).
	go func() {
		for index := 0; index < 3000; index++ {
			server.Push("mark", map[string]any{"text": "flood"})
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(client.Events()) < 1000 {
		time.Sleep(20 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Client.Close deadlocked with a full event channel")
	}
}

func TestRunIDMismatchReplaysAsFreshStream(t *testing.T) {
	server := startFake(t, fakemod.Options{RunID: "run_second", EventBuffer: 16})
	server.Push("mark", map[string]any{"text": "new-run-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A cursor from a different run must not hide the new run's events.
	client, welcome, err := control.Dial(ctx, control.Config{
		Address: server.Address(), Pin: server.Pin(), Token: testToken,
		LastSeq: 999, ExpectRunID: "run_first",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if !welcome.Replay.Lost {
		t.Fatalf("a cross-run cursor must report a gap: %+v", welcome.Replay)
	}
	if welcome.Replay.From != 1 {
		t.Fatalf("replay from = %d, want the start of the new run", welcome.Replay.From)
	}
	select {
	case event := <-client.Events():
		if !event.Replay || event.Map()["text"] != "new-run-1" {
			t.Fatalf("new run event not delivered: %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event from the new run")
	}
}

// flakyConn wraps a raw socket so tests can inject partial write failures and
// observe how many bytes were written.
type flakyConn struct {
	net.Conn
	failAfter atomic.Int64
	written   atomic.Int64
}

func (f *flakyConn) Write(p []byte) (int, error) {
	limit := f.failAfter.Load()
	if limit < 0 {
		n, err := f.Conn.Write(p)
		f.written.Add(int64(n))
		return n, err
	}
	if int64(len(p)) <= limit {
		n, err := f.Conn.Write(p)
		f.written.Add(int64(n))
		return n, err
	}
	n, err := f.Conn.Write(p[:limit])
	f.written.Add(int64(n))
	if err == nil {
		err = errors.New("injected partial write failure")
	}
	return n, err
}

func dialThroughFlakyConn(t *testing.T, server *fakemod.Server) (*control.Client, *flakyConn, error) {
	t.Helper()
	flaky := &flakyConn{}
	flaky.failAfter.Store(-1)
	client, _, err := control.Dial(context.Background(), control.Config{
		Address: server.Address(), Pin: server.Pin(), Token: testToken,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			flaky.Conn = conn
			return flaky, nil
		},
	})
	return client, flaky, err
}

func TestPartialWriteFailureIsResultUnknown(t *testing.T) {
	server := startFake(t, fakemod.Options{})
	client, flaky, err := dialThroughFlakyConn(t, server)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Fail the next socket write after a few bytes: the request may have been
	// partially delivered, so it must not be advertised as safe to retry.
	flaky.failAfter.Store(8)
	_, failure := client.CallWithID(context.Background(), "partial-1", "command",
		map[string]any{"command": "say partial"})
	if failure == nil {
		t.Fatal("an injected partial write must fail")
	}
	if !failure.ResultUnknown || failure.Retryable {
		t.Fatalf("a failed write after the attempt began must be result-unknown, not retryable: %+v", failure)
	}
	if failure.RequestID != "partial-1" {
		t.Fatalf("request id lost: %+v", failure)
	}
	// The poisoned connection is closed before the writer slot is released, so
	// a follow-up call must fail fast and never reach the server.
	before := server.Requests()
	_, followUp := client.CallWithID(context.Background(), "partial-2", "command",
		map[string]any{"command": "say again"})
	if followUp == nil {
		t.Fatal("a poisoned connection accepted another write")
	}
	time.Sleep(100 * time.Millisecond)
	if server.Requests() != before {
		t.Fatalf("a poisoned connection delivered another request (%d -> %d)",
			before, server.Requests())
	}
}

func TestConcurrentRequestsAreNotSerializedBySlowReply(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := startFake(t, fakemod.Options{Ops: map[string]fakemod.OpFunc{
		"state": func(params map[string]any) (any, *protocol.Error) {
			started <- struct{}{}
			<-release
			return map[string]any{"type": "state", "slow": true}, nil
		},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := control.Dial(ctx, control.Config{Address: server.Address(), Pin: server.Pin(), Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	slowDone := make(chan *protocol.Error, 1)
	go func() {
		_, failure := client.Call(ctx, "state", nil)
		slowDone <- failure
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow request never reached the server")
	}
	// The slow request holds only the socket writer while flushing; the ping
	// must reach the server and return before the slow reply exists.
	begin := time.Now()
	if _, failure := client.Call(ctx, "ping", nil); failure != nil {
		t.Fatalf("ping was blocked behind a slow reply: %v", failure)
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("ping took %s while a slow reply was pending", elapsed)
	}
	if got := server.Requests(); got < 2 {
		t.Fatalf("both requests must reach the server, got %d", got)
	}
	close(release)
	if failure := <-slowDone; failure != nil {
		t.Fatalf("slow request failed: %v", failure)
	}
}

func TestManualCancelInterruptsStalledWrite(t *testing.T) {
	server := startFake(t, fakemod.Options{StallRead: true})
	client, flaky, err := dialThroughFlakyConn(t, server)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("no client")
	}
	defer client.Close()
	// A long IO timeout so only the manual cancellation can stop the write.
	ctx, cancel := context.WithCancel(context.Background())
	// Bytes counted so far include the Dial hello; only the request write matters.
	flaky.written.Store(0)
	big := make([]byte, 8*1024*1024)
	for index := range big {
		big[index] = 'a'
	}
	failureCh := make(chan *protocol.Error, 1)
	go func() {
		_, failure := client.CallWithID(ctx, "cancel-stalled", "command",
			map[string]any{"command": string(big)})
		failureCh <- failure
	}()
	deadline := time.Now().Add(5 * time.Second)
	for flaky.written.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if flaky.written.Load() == 0 {
		t.Fatal("the write never began")
	}
	cancel()
	select {
	case failure := <-failureCh:
		if failure == nil {
			t.Fatal("a cancelled write must fail")
		}
		if !failure.ResultUnknown || failure.Retryable {
			t.Fatalf("a cancelled write may have been partially sent: %+v", failure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manual cancellation did not interrupt the stalled write")
	}
}

func TestPreCancelledCallSendsNothing(t *testing.T) {
	server := startFake(t, fakemod.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := control.Dial(ctx, control.Config{Address: server.Address(), Pin: server.Pin(), Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	cancelled, cancelCall := context.WithCancel(context.Background())
	cancelCall()
	_, failure := client.CallWithID(cancelled, "pre-cancelled-1", "command",
		map[string]any{"command": "say never"})
	if failure == nil {
		t.Fatal("a pre-cancelled call must fail")
	}
	if failure.ResultUnknown || !failure.Retryable {
		t.Fatalf("a request that was never sent is safe to retry: %+v", failure)
	}
	if got := server.Requests(); got != 0 {
		t.Fatalf("the server received %d requests from a cancelled call", got)
	}
	// The connection is still usable and the next call really is sent.
	if _, failure := client.Call(ctx, "ping", nil); failure != nil {
		t.Fatalf("the connection was poisoned by cancellation: %v", failure)
	}
	if got := server.Requests(); got != 1 {
		t.Fatalf("the follow-up ping was not sent (requests=%d)", got)
	}
}

func TestDialHonorsContextDeadline(t *testing.T) {
	server := startFake(t, fakemod.Options{SilentHello: true})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := control.Dial(ctx, control.Config{
		Address:     server.Address(),
		Pin:         server.Pin(),
		Token:       testToken,
		DialTimeout: 30 * time.Second,
	})
	if err == nil {
		t.Fatal("the cancelled handshake must fail")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("dial ignored the context deadline: %s", elapsed)
	}
}

func TestStalledWriterHonorsIOTimeout(t *testing.T) {
	server := startFake(t, fakemod.Options{StallRead: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := control.Dial(ctx, control.Config{
		Address: server.Address(), Pin: server.Pin(), Token: testToken,
		IOTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	big := make([]byte, 8*1024*1024)
	for index := range big {
		big[index] = 'a'
	}
	start := time.Now()
	_, failure := client.CallWithID(ctx, "stalled-1", "command",
		map[string]any{"command": string(big)})
	if failure == nil {
		t.Fatal("a write against a stalled reader must fail")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the write deadline was not applied: %s", elapsed)
	}
	select {
	case <-client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a failed partial write must terminate the connection")
	}
}
