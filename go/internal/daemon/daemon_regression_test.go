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

// A cursor is bound to one daemon run: after a restart the old seq is ahead
// of the new stream and must be reported as a reset, not as "no events".
func TestEventsCursorResetAfterDaemonRestart(t *testing.T) {
	home := t.TempDir()
	fake, err := fakemod.Start(fakemod.Options{Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	h1 := startHarnessHome(t, home, fake, fakeToken, nil)
	h1.waitForTargetConnected()
	for index := 0; index < 5; index++ {
		h1.fake.Push("mark", map[string]any{"text": fmt.Sprintf("before-%d", index)})
	}
	var first map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result, failure := h1.call(context.Background(), "events", map[string]any{"since": 0, "limit": 100})
		if failure == nil {
			first, _ = result.(map[string]any)
			if len(first["events"].([]any)) >= 5 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if first == nil {
		t.Fatal("no initial events")
	}
	oldStream, _ := first["streamId"].(string)
	oldNext, _ := numberToInt64(first["next"])
	h1.cancel()
	if err := h1.stop(); err != nil {
		t.Fatalf("stop daemon: %v", err)
	}

	h2 := startHarnessHome(t, home, fake, fakeToken, nil)
	h2.waitForTargetConnected()
	stale, failure := h2.call(context.Background(), "events",
		map[string]any{"since": oldNext, "streamId": oldStream, "limit": 100})
	if failure != nil {
		t.Fatal(failure)
	}
	staleObject, _ := stale.(map[string]any)
	if staleObject["reset"] != true {
		t.Fatalf("a stale cursor must report reset: %v", staleObject)
	}
	if stream, _ := staleObject["streamId"].(string); stream == oldStream {
		t.Fatalf("a restarted daemon must have a new stream id")
	}
	fresh, failure := h2.call(context.Background(), "events", map[string]any{"since": 0, "limit": 100})
	if failure != nil {
		t.Fatal(failure)
	}
	freshObject, _ := fresh.(map[string]any)
	if len(freshObject["events"].([]any)) == 0 {
		t.Fatalf("the new stream must return its buffer: %v", freshObject)
	}
}

// The CLI follow contract: subscribe first, page the whole replay, then switch
// to live with seq de-duplication and category filtering.
func TestFollowHandoffAcrossPagesAndCategories(t *testing.T) {
	h := startHarness(t, nil, nil)
	h.waitForTargetConnected()
	ctx := context.Background()
	client := h.client()
	defer client.Close()
	if _, failure := client.Call(ctx, "subscribe", map[string]any{"events": []string{"mark"}}); failure != nil {
		t.Fatal(failure)
	}
	for index := 0; index < 120; index++ {
		h.fake.Push("mark", map[string]any{"text": fmt.Sprintf("m-%d", index)})
	}
	// One extra event published between replay pages proves the hand-off does
	// not lose events that arrive after the first page.
	pushedMidway := false
	var replaySeqs []int64
	since := int64(0)
	for pages := 0; pages < 20; pages++ {
		result, failure := client.Call(ctx, "events", map[string]any{
			"since": since, "limit": 50, "category": "mark"})
		if failure != nil {
			t.Fatal(failure)
		}
		object, _ := result.(map[string]any)
		events, _ := object["events"].([]any)
		for _, entry := range events {
			replaySeqs = append(replaySeqs, lookupSeq(entry))
		}
		if next, ok := numberToInt64(object["next"]); ok {
			since = next
		}
		if !pushedMidway {
			pushedMidway = true
			h.fake.Push("mark", map[string]any{"text": "mid-handoff"})
			h.fake.Push("game", map[string]any{"text": "must-not-arrive"})
		}
		if object["truncated"] != true {
			break
		}
	}
	for index := 1; index < len(replaySeqs); index++ {
		if replaySeqs[index] <= replaySeqs[index-1] {
			t.Fatalf("replay seqs are not increasing: %v", replaySeqs)
		}
	}
	// The remaining marks arrive live after the replay pages; the hand-off
	// must deliver each exactly once (replay overlap de-duplicated by seq) and
	// must never deliver the game event to a mark-only subscription.
	seen := map[int64]bool{}
	for _, sequence := range replaySeqs {
		seen[sequence] = true
	}
	last := since
	const expectedMarks = 121 // 120 + mid-handoff
	deadline := time.After(10 * time.Second)
	for len(seen) < expectedMarks {
		select {
		case event, ok := <-client.Events():
			if !ok {
				t.Fatal("the IPC connection closed during the hand-off")
			}
			if category, _ := event.Data["category"].(string); category != "mark" {
				t.Fatalf("a non-mark event reached a mark-only subscription: %v", event.Data)
			}
			sequence := lookupSeq(event.Data)
			if sequence <= last {
				continue // de-duplicated replay overlap
			}
			if seen[sequence] {
				t.Fatalf("event seq %d was delivered twice", sequence)
			}
			seen[sequence] = true
			last = sequence
		case <-deadline:
			t.Fatalf("hand-off lost events: got %d of %d", len(seen), expectedMarks)
		}
	}
}

func lookupSeq(value any) int64 {
	object, _ := value.(map[string]any)
	sequence, _ := numberToInt64(object["seq"])
	return sequence
}

func numberToInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	}
	return 0, false
}

// Reusing a caller-provided request id must never overwrite or clear an
// unreconciled write; the retry is routed to status instead of being resent.
func TestRepeatedRequestIdPreservesUnknownLedger(t *testing.T) {
	release := make(chan struct{})
	fake, err := fakemod.Start(fakemod.Options{Token: fakeToken, Ops: map[string]fakemod.OpFunc{
		"command": func(params map[string]any) (any, *protocol.Error) {
			<-release
			return map[string]any{"type": "cmd_ack", "detail": params["command"], "output": []any{}}, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	h := startHarnessWith(t, fake, fakeToken, nil)
	h.waitForTargetConnected()
	ctx := context.Background()
	envelope := func(command, requestID string) map[string]any {
		return map[string]any{"target": "fake", "op": "command", "requestId": requestID,
			"params": map[string]any{"command": command}, "timeoutSeconds": 0.3}
	}
	_, failure := h.call(ctx, "call", envelope("say first", "dup-request-1"))
	if failure == nil || !failure.ResultUnknown || failure.RequestID != "dup-request-1" {
		t.Fatalf("the first write must be result-unknown: %+v", failure)
	}
	requestsAfterFirst := fake.Requests()
	list, callFailure := h.call(ctx, "requests", nil)
	if callFailure != nil || !strings.Contains(fmt.Sprint(list), "dup-request-1") {
		t.Fatalf("the ledger did not record the unknown write: %v %v", list, callFailure)
	}
	// Same id, different payload: rejected, not overwritten and not sent.
	_, second := h.call(ctx, "call", envelope("say second", "dup-request-1"))
	if second == nil || second.Code != protocol.CodeResultUnknown || second.RequestID != "dup-request-1" {
		t.Fatalf("a repeated id must be routed to status, not resent: %+v", second)
	}
	if fake.Requests() != requestsAfterFirst {
		t.Fatalf("the repeated id was sent to the game (%d -> %d)", requestsAfterFirst, fake.Requests())
	}
	close(release)
	time.Sleep(300 * time.Millisecond)
	_, third := h.call(ctx, "call", envelope("say third", "dup-request-1"))
	if third == nil || third.Code != protocol.CodeResultUnknown {
		t.Fatalf("the unknown record must survive the original completion: %+v", third)
	}
	if fake.Requests() != requestsAfterFirst {
		t.Fatalf("the third call was sent despite the unknown record (%d -> %d)",
			requestsAfterFirst, fake.Requests())
	}

	// Crash and recover: the reconciler resolves the entry through
	// request_status, after which the id can be used for a new write.
	h.cancel()
	if err := h.stop(); err != nil {
		t.Fatalf("stop daemon: %v", err)
	}
	h2 := startHarnessHome(t, h.home, fake, fakeToken, nil)
	h2.waitForTargetConnected()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		remaining, failure := h2.call(ctx, "requests", nil)
		if failure == nil && !strings.Contains(fmt.Sprint(remaining), "dup-request-1") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	final, failure := h2.call(ctx, "call", envelope("say after", "dup-request-1"))
	if failure != nil || final == nil {
		t.Fatalf("the id must be reusable after reconciliation: %+v %v", final, failure)
	}
}
