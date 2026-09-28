package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/fakemod"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

// Two daemons that share one credential must never collide in the server's
// (token, request id) write ledger; that used to make the second write return
// the first write's result without executing.
func TestSameTokenTwoDaemonsDistinctWriteIDs(t *testing.T) {
	fake, err := fakemod.Start(fakemod.Options{Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	h1 := startHarnessWith(t, fake, fakeToken, nil)
	h1.waitForTargetConnected()
	h2 := startHarnessWith(t, fake, fakeToken, nil)
	h2.waitForTargetConnected()
	ctx := context.Background()

	marker1 := fmt.Sprintf("alpha-%d", time.Now().UnixNano())
	marker2 := fmt.Sprintf("beta-%d", time.Now().UnixNano())
	r1, failure := h1.call(ctx, "call", map[string]any{
		"target": "fake", "op": "command", "params": map[string]any{"command": "say " + marker1}})
	if failure != nil {
		t.Fatalf("daemon A command failed: %v", failure)
	}
	r2, failure := h2.call(ctx, "call", map[string]any{
		"target": "fake", "op": "command", "params": map[string]any{"command": "say " + marker2}})
	if failure != nil {
		t.Fatalf("daemon B command failed: %v", failure)
	}
	text1 := fmt.Sprint(r1)
	text2 := fmt.Sprint(r2)
	if !strings.Contains(text1, marker1) || strings.Contains(text1, marker2) {
		t.Fatalf("daemon A got the wrong reply: %s", text1)
	}
	if !strings.Contains(text2, marker2) || strings.Contains(text2, marker1) {
		t.Fatalf("daemon B got the wrong reply: %s", text2)
	}
	if strings.Contains(text2, `"duplicate":true`) {
		t.Fatalf("daemon B's write was de-duplicated against daemon A: %s", text2)
	}
}

// A write must be persisted before it can leave the process; a daemon crash
// between send and reply must still leave a recoverable unknown write.
func TestWriteLedgerRecordedBeforeSendAndReconciledAfterCrash(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	fake, err := fakemod.Start(fakemod.Options{Token: fakeToken, Ops: map[string]fakemod.OpFunc{
		"command": func(params map[string]any) (any, *protocol.Error) {
			started <- struct{}{}
			<-release
			return map[string]any{"type": "cmd_ack", "detail": params["command"],
				"output": []any{"late-result"}}, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	h := startHarnessWith(t, fake, fakeToken, nil)
	h.waitForTargetConnected()

	failureCh := make(chan *protocol.Error, 1)
	go func() {
		_, failure := h.call(context.Background(), "call", map[string]any{
			"target": "fake", "op": "command",
			"params": map[string]any{"command": "say slow"}, "timeoutSeconds": 0.3})
		failureCh <- failure
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the command never reached the fake mod")
	}
	ledgerPath := filepath.Join(h.home, "unknown_writes.json")
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("the write ledger was not persisted before the reply: %v", err)
	}
	if !strings.Contains(string(data), "command") {
		t.Fatalf("ledger does not describe the in-flight write: %s", data)
	}
	failure := <-failureCh
	if failure == nil || !failure.ResultUnknown || failure.RequestID == "" {
		t.Fatalf("expected result-unknown with a request id, got %v", failure)
	}
	requestID := failure.RequestID

	h.cancel()
	if err := h.stop(); err != nil {
		t.Fatalf("daemon stop: %v", err)
	}
	close(release)

	h2 := startHarnessHome(t, h.home, fake, fakeToken, nil)
	h2.waitForTargetConnected()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		list, callFailure := h2.call(context.Background(), "requests", nil)
		if callFailure == nil && !strings.Contains(fmt.Sprint(list), requestID) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	list, _ := h2.call(context.Background(), "requests", nil)
	if strings.Contains(fmt.Sprint(list), requestID) {
		t.Fatalf("the persisted request was never resolved: %v", list)
	}
	events, _ := h2.call(context.Background(), "events", map[string]any{"since": 0, "limit": 200})
	if !strings.Contains(fmt.Sprint(events), "request_resolved") {
		t.Fatalf("no request_resolved event after recovery: %v", events)
	}
}

// If the ledger cannot be persisted, a non-idempotent write must not be sent.
func TestPersistenceFailureRefusesWrite(t *testing.T) {
	var invoked int32
	fake, err := fakemod.Start(fakemod.Options{Token: fakeToken, Ops: map[string]fakemod.OpFunc{
		"command": func(params map[string]any) (any, *protocol.Error) {
			atomic.AddInt32(&invoked, 1)
			return map[string]any{"type": "cmd_ack"}, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	h := startHarnessWith(t, fake, fakeToken, nil)
	h.waitForTargetConnected()

	ledgerPath := filepath.Join(h.home, "unknown_writes.json")
	if err := os.Remove(ledgerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledgerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, failure := h.call(context.Background(), "call", map[string]any{
		"target": "fake", "op": "command", "params": map[string]any{"command": "say blocked"}})
	if failure == nil || failure.Code != protocol.CodeInternal ||
		!strings.Contains(failure.Message, "refusing to send") {
		t.Fatalf("expected a fail-closed ledger error, got %v", failure)
	}
	if atomic.LoadInt32(&invoked) != 0 {
		t.Fatal("the write reached the game even though the ledger could not be persisted")
	}
}

// A cursor from a previous run must not hide the new run's events.
func TestCursorResetsAcrossGameRuns(t *testing.T) {
	fake, err := fakemod.Start(fakemod.Options{Token: fakeToken, RunID: "run_first", EventBuffer: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	h := startHarnessWith(t, fake, fakeToken, nil)
	h.waitForTargetConnected()
	for index := 0; index < 70; index++ {
		fake.Push("mark", map[string]any{"text": fmt.Sprintf("old-%d", index)})
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(h.home, "cursors.json"))
		if err == nil && strings.Contains(string(data), "run_first") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	data, err := os.ReadFile(filepath.Join(h.home, "cursors.json"))
	if err != nil || !strings.Contains(string(data), "run_first") {
		t.Fatalf("cursor was not persisted: %v %s", err, data)
	}

	fake.SetRunID("run_second")
	fake.Push("mark", map[string]any{"text": "new-run-event"})
	h.waitForTargetConnected()
	deadline = time.Now().Add(10 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		events, _ := h.call(context.Background(), "events", map[string]any{"since": 0, "limit": 300})
		if strings.Contains(fmt.Sprint(events), "new-run-event") {
			seen = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !seen {
		t.Fatal("an event from the new run was skipped after reconnect")
	}
	events, _ := h.call(context.Background(), "events", map[string]any{"since": 0, "limit": 300})
	text := fmt.Sprint(events)
	if !strings.Contains(text, "game_restarted") || !strings.Contains(text, "event_gap") {
		t.Fatalf("the run change and gap were not reported: %s", text)
	}
}

// Cursor pagination must return the oldest events after the cursor and say
// when more remain, instead of silently dropping the tail.
func TestEventsPaginationIsCursorSafe(t *testing.T) {
	h := startHarness(t, nil, nil)
	h.waitForTargetConnected()
	for index := 0; index < 10; index++ {
		h.fake.Push("mark", map[string]any{"text": fmt.Sprintf("page-%d", index)})
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		probe, _ := h.call(context.Background(), "events", map[string]any{"since": 0, "limit": 500})
		if strings.Contains(fmt.Sprint(probe), "page-9") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	first, failure := h.call(context.Background(), "events", map[string]any{"since": 0, "limit": 4})
	if failure != nil {
		t.Fatal(failure)
	}
	firstObject, _ := first.(map[string]any)
	if firstObject["truncated"] != true {
		t.Fatalf("a truncated page must say so: %v", firstObject)
	}
	page, _ := firstObject["events"].([]any)
	if len(page) != 4 {
		t.Fatalf("page size = %d", len(page))
	}
	next := int64(firstObject["next"].(float64))
	lastSeq := int64(page[len(page)-1].(map[string]any)["seq"].(float64))
	if next != lastSeq {
		t.Fatalf("next = %d, want the last delivered sequence %d", next, lastSeq)
	}
	second, failure := h.call(context.Background(), "events", map[string]any{"since": next, "limit": 500})
	if failure != nil {
		t.Fatal(failure)
	}
	secondObject, _ := second.(map[string]any)
	secondPage, _ := secondObject["events"].([]any)
	if len(secondPage) == 0 {
		t.Fatal("second page is empty")
	}
	firstSeq := int64(secondPage[0].(map[string]any)["seq"].(float64))
	if firstSeq != next+1 {
		t.Fatalf("second page starts at %d, want %d", firstSeq, next+1)
	}
}

// Shutdown must stay bounded while thousands of events are in flight.
func TestDaemonStopIsBoundedWhileEventsFlood(t *testing.T) {
	h := startHarness(t, nil, nil)
	h.waitForTargetConnected()
	for index := 0; index < 3000; index++ {
		h.fake.Push("mark", map[string]any{"text": "flood"})
	}
	start := time.Now()
	h.cancel()
	if err := h.stop(); err != nil {
		t.Fatalf("daemon stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("daemon stop took %s with a full event pipeline", elapsed)
	}
}
