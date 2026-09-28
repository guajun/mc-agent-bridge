package daemon

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func newEventTestDaemon(t *testing.T, buffer int) *Daemon {
	t.Helper()
	return &Daemon{
		home:       t.TempDir(),
		logger:     func(string, ...any) {},
		streamID:   "stream-test-1",
		bufferSize: buffer,
		unknown:    newUnknownWrites(t.TempDir()),
		cursors:    newCursorStore(t.TempDir()),
		stopCh:     make(chan struct{}),
	}
}

// Two targets/goroutines must never deliver seq 2 before seq 1.
func TestIngestSequencesInOrderAcrossTargets(t *testing.T) {
	daemon := newEventTestDaemon(t, 10000)
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(id int) {
			defer wait.Done()
			for index := 0; index < 100; index++ {
				daemon.ingest(fmt.Sprintf("target-%d", id%2), map[string]any{"type": "mark"})
			}
		}(worker)
	}
	wait.Wait()
	result := daemon.recentEvents(0, 20000, "", "", "")
	events := result["events"].([]map[string]any)
	if len(events) != 800 {
		t.Fatalf("ring holds %d events, want 800", len(events))
	}
	for index, event := range events {
		sequence, _ := event["seq"].(int64)
		if sequence != int64(index+1) {
			t.Fatalf("event at position %d has seq %d; sequence or order is broken", index, sequence)
		}
	}
}

// Paging while events are being ingested must never skip one, and an empty
// page must not advance past an event that was assigned but not yet appended.
func TestPaginationBoundaryUnderConcurrentIngest(t *testing.T) {
	daemon := newEventTestDaemon(t, 10000)
	const total = 600
	var produced atomic.Int64
	done := make(chan struct{})
	go func() {
		for index := 0; index < total; index++ {
			daemon.ingest("target", map[string]any{"type": "mark"})
			produced.Add(1)
		}
		close(done)
	}()

	var sequences []int64
	since := int64(0)
	for {
		result := daemon.recentEvents(since, 25, "", "", "")
		events := result["events"].([]map[string]any)
		for _, event := range events {
			sequence, _ := event["seq"].(int64)
			sequences = append(sequences, sequence)
		}
		if next, ok := result["next"].(int64); ok {
			since = next
		}
		if result["truncated"] == true {
			continue
		}
		select {
		case <-done:
			if len(sequences) >= int(produced.Load()) {
				goto finished
			}
		default:
		}
	}
finished:
	if len(sequences) != total {
		t.Fatalf("collected %d events, want %d", len(sequences), total)
	}
	for index, sequence := range sequences {
		if sequence != int64(index+1) {
			t.Fatalf("collected sequence %d at position %d; events were skipped", sequence, index)
		}
	}
}

func TestRecentEventsFiltersAndReset(t *testing.T) {
	daemon := newEventTestDaemon(t, 100)
	daemon.ingest("alpha", map[string]any{"type": "mark"})
	daemon.ingest("beta", map[string]any{"type": "mark"})
	daemon.ingest("alpha", map[string]any{"type": "game"})

	filtered := daemon.recentEvents(0, 100, "mark", "alpha", "")
	events := filtered["events"].([]map[string]any)
	if len(events) != 1 || events[0]["target"] != "alpha" {
		t.Fatalf("target/category filter failed: %+v", filtered)
	}
	if filtered["reset"] == true {
		t.Fatalf("a live cursor must not reset: %+v", filtered)
	}
	// A cursor ahead of this stream can only belong to another daemon run.
	stale := daemon.recentEvents(9999, 100, "", "", "")
	if stale["reset"] != true {
		t.Fatalf("a future cursor must report a reset: %+v", stale)
	}
	otherStream := daemon.recentEvents(0, 100, "", "", "stream-from-another-run")
	if otherStream["reset"] != true {
		t.Fatalf("a different stream id must report a reset: %+v", otherStream)
	}
}
