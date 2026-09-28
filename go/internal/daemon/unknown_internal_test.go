package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedLedger(t *testing.T, u *unknownWrites, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		if err := u.Record("target", "command", fmt.Sprintf("id-%d", index), "inst", "run", "cred"); err != nil {
			t.Fatalf("Record %d: %v", index, err)
		}
	}
}

func TestLedgerCapacityRefusesNewWrites(t *testing.T) {
	u := newUnknownWrites(t.TempDir())
	seedLedger(t, u, unknownWritesCap)
	if err := u.Record("target", "command", "overflow", "inst", "run", "cred"); err == nil {
		t.Fatal("a full ledger must refuse a new write instead of evicting a recovery entry")
	}
	if u.Count() != unknownWritesCap {
		t.Fatalf("ledger size = %d, want %d", u.Count(), unknownWritesCap)
	}
	// Resolving one entry frees a slot.
	if err := u.Resolve("target", "id-0"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := u.Record("target", "command", "after-resolve", "inst", "run", "cred"); err != nil {
		t.Fatalf("a free slot must accept a new write: %v", err)
	}
}

func TestLedgerLoadPreservesOverCapEntries(t *testing.T) {
	home := t.TempDir()
	list := make([]UnknownWrite, 0, unknownWritesCap+3)
	for index := 0; index < unknownWritesCap+3; index++ {
		list = append(list, UnknownWrite{
			Target: "target", Operation: "command",
			RequestID: fmt.Sprintf("old-%d", index),
			At:        time.Now().UTC().Add(time.Duration(index) * time.Millisecond),
			State:     "unknown",
		})
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, unknownWritesFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	u := newUnknownWrites(home)
	message, err := u.Load()
	if err != nil {
		t.Fatal(err)
	}
	if u.Count() != unknownWritesCap+3 {
		t.Fatalf("load dropped recovery entries: %d", u.Count())
	}
	if !strings.Contains(message, "above") {
		t.Fatalf("over-cap load must warn, got %q", message)
	}
	if err := u.Record("target", "command", "new", "inst", "run", "cred"); err == nil {
		t.Fatal("an over-cap ledger must refuse new writes")
	}
}

func TestLedgerReturnsCopiesAndSurfacesSaveFailures(t *testing.T) {
	u := newUnknownWrites(t.TempDir())
	if err := u.Record("target", "command", "copy-1", "inst", "run", "cred"); err != nil {
		t.Fatal(err)
	}
	entries := u.ForTarget("target")
	entries[0].State = "mutated"
	if got := u.ForTarget("target"); got[0].State != "unknown" {
		t.Fatalf("ForTarget returned a mutable pointer: %+v", got[0])
	}
	if err := u.NoteUnknown("target", "copy-1", "note"); err != nil {
		t.Fatal(err)
	}
	// Force persistence to fail: replace the file with a directory.
	if err := os.Remove(u.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(u.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := u.Resolve("target", "copy-1"); err == nil {
		t.Fatal("a failed save must be reported")
	}
	if u.Count() != 1 {
		t.Fatal("a failed Resolve must keep the recovery entry")
	}
	if err := u.NoteUnknown("target", "copy-1", "again"); err == nil {
		t.Fatal("a failed NoteUnknown save must be reported")
	}
	if u.Count() != 1 || u.ForTarget("target")[0].Message != "note" {
		t.Fatalf("a failed NoteUnknown must keep the previous entry: %+v", u.ForTarget("target"))
	}
}

func TestLedgerConcurrentReadersAndWriters(t *testing.T) {
	u := newUnknownWrites(t.TempDir())
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(id int) {
			defer wait.Done()
			for index := 0; index < 40; index++ {
				requestID := fmt.Sprintf("w%d-%d", id, index)
				_ = u.Record("target", "command", requestID, "inst", "run", "cred")
				_ = u.NoteUnknown("target", requestID, "checking")
				_ = u.ForTarget("target")
				_ = u.List()
				if index%3 == 0 {
					_ = u.Resolve("target", requestID)
				}
			}
		}(worker)
	}
	wait.Wait()
	for _, entry := range u.List() {
		if entry.RequestID == "" {
			t.Fatalf("list returned an empty entry: %+v", entry)
		}
	}
}
