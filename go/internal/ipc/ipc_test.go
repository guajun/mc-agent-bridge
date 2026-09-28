package ipc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/ipc"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

func okHandler(_ context.Context, _ string, _ map[string]any) (any, *protocol.Error) {
	return map[string]any{"ok": true}, nil
}

func TestClientCloseWithFullEventQueueIsBounded(t *testing.T) {
	server, err := ipc.Start("127.0.0.1:0", "token", okHandler)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := ipc.Dial(ctx, server.Address(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, failure := client.Call(ctx, "subscribe", map[string]any{"events": []string{"*"}}); failure != nil {
		t.Fatal(failure)
	}
	// Never consume client.Events(): flood the connection so the client read
	// loop blocks on a full event channel. Close must still return.
	go func() {
		for index := 0; index < 5000; index++ {
			server.Broadcast("mark", map[string]any{"seq": index})
		}
	}()
	time.Sleep(300 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		client.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ipc.Client.Close deadlocked with a full event queue")
	}
}

func TestCallWithIDTimeoutIsPossiblyDispatched(t *testing.T) {
	release := make(chan struct{})
	var handled atomic.Int32
	server, err := ipc.Start("127.0.0.1:0", "token",
		func(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
			if method == "call" {
				handled.Add(1)
				<-release
			}
			return map[string]any{"ok": true}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	client, err := ipc.Dial(context.Background(), server.Address(), "token")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, failure := client.CallWithID(ctx, "call", map[string]any{"op": "command"}, "cli-request-1")
	if failure == nil {
		t.Fatal("a timed-out local call must fail")
	}
	if !failure.ResultUnknown || failure.Retryable {
		t.Fatalf("a dispatched write with a lost local reply must be result-unknown: %+v", failure)
	}
	if failure.RequestID != "cli-request-1" {
		t.Fatalf("the end-to-end request id was lost: %+v", failure)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for handled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if handled.Load() != 1 {
		t.Fatalf("the daemon handler did not run exactly once: %d", handled.Load())
	}
}

func TestPreCancelledCallSendsNothing(t *testing.T) {
	var handled atomic.Int32
	server, err := ipc.Start("127.0.0.1:0", "token",
		func(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
			handled.Add(1)
			return map[string]any{"ok": true}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	client, err := ipc.Dial(context.Background(), server.Address(), "token")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, failure := client.CallWithID(ctx, "call", map[string]any{"op": "command"}, "cli-request-2")
	if failure == nil || failure.ResultUnknown || !failure.Retryable {
		t.Fatalf("a not-sent local call is retryable: %+v", failure)
	}
	time.Sleep(100 * time.Millisecond)
	if handled.Load() != 0 {
		t.Fatal("a pre-cancelled call reached the daemon")
	}
}

func TestServerStopCancelsBlockedHandlers(t *testing.T) {
	exited := make(chan struct{}, 64)
	server, err := ipc.Start("127.0.0.1:0", "token",
		func(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
			<-ctx.Done()
			exited <- struct{}{}
			return nil, protocol.NewError(protocol.CodeInternal, "cancelled")
		})
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(context.Background(), server.Address(), "token")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 40; index++ {
		go func() { client.Call(context.Background(), "work", nil) }()
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	server.Stop()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Server.Stop took %s with full dispatcher slots", elapsed)
	}
	deadline := time.After(5 * time.Second)
	cancelled := 0
	for cancelled < 1 {
		select {
		case <-exited:
			cancelled++
		case <-deadline:
			t.Fatalf("blocked handlers were not cancelled by Stop (%d)", cancelled)
		}
	}
	client.Close()
}

// A real socket EOF after the daemon read a write must keep the end-to-end
// classification: possibly dispatched, with the request id, not a generic
// daemon_not_running error.
func TestEOFAfterDispatchKeepsPossiblyDispatched(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			return
		}
		var request ipc.Request
		if json.Unmarshal([]byte(line), &request) == nil {
			response, _ := json.Marshal(map[string]any{"type": "response", "id": request.ID,
				"ok": true, "result": map[string]any{"pong": true}})
			conn.Write(append(response, '\n'))
		}
		// Read the next request (the write) and close without replying.
		if _, err := reader.ReadString('\n'); err == nil {
			conn.Close()
			return
		}
		conn.Close()
	}()

	client, err := ipc.Dial(context.Background(), listener.Addr().String(), "token")
	if err != nil {
		t.Fatal(err)
	}
	_, failure := client.CallWithID(context.Background(), "call",
		map[string]any{"op": "command"}, "eof-request-1")
	if failure == nil {
		t.Fatal("an EOF before the reply must fail")
	}
	if !failure.ResultUnknown || failure.Retryable {
		t.Fatalf("EOF after dispatch must be possibly-dispatched: %+v", failure)
	}
	if failure.RequestID != "eof-request-1" {
		t.Fatalf("EOF lost the end-to-end id: %+v", failure)
	}
	// The connection is poisoned: the next call fails fast, not silently.
	_, followUp := client.CallWithID(context.Background(), "call",
		map[string]any{"op": "command"}, "eof-request-2")
	if followUp == nil {
		t.Fatal("a poisoned IPC connection accepted another call")
	}
	client.Close()
}

// More live events than the client queue capacity must not block responses.
func TestEventOverflowDoesNotBlockResponses(t *testing.T) {
	server, err := ipc.Start("127.0.0.1:0", "token",
		func(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
			if method == "events" {
				time.Sleep(300 * time.Millisecond)
				return map[string]any{"events": []any{}, "next": 0,
					"streamId": "test-stream", "truncated": false, "dropped": false}, nil
			}
			return map[string]any{"ok": true}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	client, err := ipc.Dial(context.Background(), server.Address(), "token")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, failure := client.Call(context.Background(), "subscribe", map[string]any{"events": []string{"*"}}); failure != nil {
		t.Fatal(failure)
	}
	go func() {
		// Pace the flood so the server's outbound queue drains: the point is
		// the *client* queue overflowing while a response is pending, not the
		// server dropping a slow client.
		for index := 0; index < 600; index++ {
			server.Broadcast("mark", map[string]any{"seq": index})
			if index%25 == 24 {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	done := make(chan *protocol.Error, 1)
	go func() {
		_, failure := client.Call(context.Background(), "events", nil)
		done <- failure
	}()
	select {
	case failure := <-done:
		if failure != nil {
			t.Fatalf("the events response was blocked by the live queue: %v", failure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the events response was blocked behind a full event queue")
	}
	if client.Dropped() == 0 {
		t.Fatal("overflow was not counted; the queue policy silently lost events")
	}
}

// Overflow must be observable without any subsequent event (the signal is
// independent), and the first event enqueued after the gap must carry the
// drop count so a consumer recovers before printing across the gap.
func TestQueueOverflowSignalsAndTagsNextEvent(t *testing.T) {
	server, err := ipc.Start("127.0.0.1:0", "token",
		func(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
			return map[string]any{"ok": true}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	client, err := ipc.Dial(context.Background(), server.Address(), "token")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, failure := client.Call(context.Background(), "subscribe", map[string]any{"events": []string{"*"}}); failure != nil {
		t.Fatal(failure)
	}
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		for index := 0; index < 600; index++ {
			server.Broadcast("mark", map[string]any{"seq": index, "text": fmt.Sprintf("e-%d", index)})
			if index%25 == 24 {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	select {
	case <-client.DropNotify():
	case <-time.After(5 * time.Second):
		t.Fatal("overflow was not signalled without a subsequent event")
	}
	<-floodDone
	// Free one slot, then enqueue exactly one more event: it must carry the
	// drop count accumulated while the queue was full.
	<-client.Events()
	server.Broadcast("mark", map[string]any{"seq": 600, "text": "tagged"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case event := <-client.Events():
			if event.DroppedBefore > 0 {
				return
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("the event after the overflow did not carry the drop count")
}
