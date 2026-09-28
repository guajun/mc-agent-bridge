package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
	"github.com/guajun/mc-agent-bridge/internal/session"
)

// stubAdapter is a session whose CallID returns one scripted failure.
type stubAdapter struct {
	mu      sync.Mutex
	next    int
	lastID  string
	failure *protocol.Error
}

func (s *stubAdapter) Call(ctx context.Context, operation string, params map[string]any) (any, *protocol.Error) {
	return s.CallID(ctx, s.NextRequestID(), operation, params)
}

func (s *stubAdapter) CallID(ctx context.Context, requestID, operation string,
	params map[string]any) (any, *protocol.Error) {
	s.mu.Lock()
	s.lastID = requestID
	failure := s.failure
	s.mu.Unlock()
	copied := *failure
	copied.RequestID = requestID
	copied.Operation = operation
	return nil, &copied
}

func (s *stubAdapter) NextRequestID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("stub-%d", s.next)
}

func (s *stubAdapter) Events() <-chan session.RawEvent { return nil }
func (s *stubAdapter) Done() <-chan struct{}           { return nil }
func (s *stubAdapter) Err() error                      { return nil }
func (s *stubAdapter) State() session.State {
	return session.State{Connected: true, InstanceID: "inst-1", RunID: "run-1"}
}
func (s *stubAdapter) Close() {}

func newLedgerDaemon(t *testing.T, home string, adapter session.Adapter) *Daemon {
	t.Helper()
	target := &config.Target{Name: "t", Transport: protocol.TransportRemote,
		Address: "127.0.0.1:1", Pin: "sha256:" + strings.Repeat("0", 64)}
	if err := config.SaveSecrets(home, &config.Secrets{Version: 1,
		Tokens: map[string]string{"t": "test-token"}}); err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{
		home:     home,
		logger:   func(string, ...any) {},
		targets:  &config.Targets{Version: 1, Targets: map[string]*config.Target{"t": target}},
		sessions: map[string]*targetSession{},
		unknown:  newUnknownWrites(home),
		cursors:  newCursorStore(home),
		stopCh:   make(chan struct{}),
	}
	ts := &targetSession{
		daemon:   daemon,
		target:   target,
		adapter:  adapter,
		state:    session.State{Connected: true, InstanceID: "inst-1", RunID: "run-1"},
		stopCh:   make(chan struct{}),
		finished: make(chan struct{}),
	}
	daemon.sessions["t"] = ts
	return daemon
}

// A write failure after the send attempt began is result-unknown and must stay
// in the recovery ledger; only a definitely-not-sent failure may clear it.
func TestDaemonKeepsLedgerForPossiblySentWriteFailure(t *testing.T) {
	home := t.TempDir()
	stub := &stubAdapter{failure: &protocol.Error{Code: protocol.CodeConnectionLost,
		Retryable: false, ResultUnknown: true}}
	daemon := newLedgerDaemon(t, home, stub)

	_, failure := daemon.call(context.Background(), "t", "command", map[string]any{"command": "say x"})
	if failure == nil || !failure.ResultUnknown || failure.Retryable {
		t.Fatalf("a possibly-sent write must be result-unknown and not retryable: %+v", failure)
	}
	entries := daemon.unknown.ForTarget("t")
	if len(entries) != 1 || entries[0].RequestID != "stub-1" {
		t.Fatalf("the pre-send ledger entry was dropped: %+v", entries)
	}

	// Reads never enter the ledger; the transport classifies them retryable
	// even when the send was partial.
	stub.mu.Lock()
	stub.failure = &protocol.Error{Code: protocol.CodeConnectionLost, Retryable: true, ResultUnknown: false}
	stub.mu.Unlock()
	if err := daemon.unknown.Resolve("t", "stub-1"); err != nil {
		t.Fatal(err)
	}
	_, readFailure := daemon.call(context.Background(), "t", "state", nil)
	if readFailure == nil || readFailure.ResultUnknown || !readFailure.Retryable {
		t.Fatalf("a read is retryable even after a partial send: %+v", readFailure)
	}
	if entries := daemon.unknown.ForTarget("t"); len(entries) != 0 {
		t.Fatalf("reads must not enter the recovery ledger: %+v", entries)
	}

	// A definitely-not-sent failure is retryable and clears its entry.
	stub.mu.Lock()
	stub.failure = &protocol.Error{Code: protocol.CodeConnectionLost, Retryable: true, ResultUnknown: false}
	stub.mu.Unlock()
	_, notSent := daemon.call(context.Background(), "t", "command", map[string]any{"command": "say y"})
	if notSent == nil || notSent.ResultUnknown || !notSent.Retryable {
		t.Fatalf("a not-sent request must be retryable: %+v", notSent)
	}
	if entries := daemon.unknown.ForTarget("t"); len(entries) != 0 {
		t.Fatalf("a definitely-not-sent write must not stay in the ledger: %+v", entries)
	}
}
