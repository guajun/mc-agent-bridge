package ipc_test

import (
	"context"
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
