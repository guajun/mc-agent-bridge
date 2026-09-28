package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
)

// UnknownWrite remembers a non-idempotent request whose outcome is not known.
// It is persisted so a daemon restart still refuses to replay the write.
type UnknownWrite struct {
	Target    string    `json:"target"`
	Operation string    `json:"operation"`
	RequestID string    `json:"requestId"`
	At        time.Time `json:"at"`
	State     string    `json:"state"` // unknown | unresolved | pending
	Message   string    `json:"message,omitempty"`
}

// unknownWrites is the bounded, persisted ledger.
type unknownWrites struct {
	path  string
	mu    sync.Mutex
	items map[string]*UnknownWrite
}

const unknownWritesFile = "unknown_writes.json"

func newUnknownWrites(home string) *unknownWrites {
	return &unknownWrites{
		path:  filepath.Join(home, unknownWritesFile),
		items: map[string]*UnknownWrite{},
	}
}

func key(target, requestID string) string {
	return target + "\x00" + requestID
}

// Load reads the ledger; a missing or unreadable file starts empty and is
// reported through the daemon log by the caller's next status call.
func (u *unknownWrites) Load() error {
	data, err := os.ReadFile(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*UnknownWrite
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, entry := range list {
		u.items[key(entry.Target, entry.RequestID)] = entry
	}
	return nil
}

func (u *unknownWrites) saveLocked() {
	list := make([]*UnknownWrite, 0, len(u.items))
	for _, entry := range u.items {
		list = append(list, entry)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	// Bound the ledger: the newest 256 entries are plenty for manual review.
	if len(list) > 256 {
		list = list[len(list)-256:]
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(u.path), config.DirMode)
	temporary := u.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), config.FileMode); err != nil {
		return
	}
	_ = os.Rename(temporary, u.path)
}

// Record remembers one unknown write.
func (u *unknownWrites) Record(target, operation, requestID, message string) {
	if requestID == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.items[key(target, requestID)] = &UnknownWrite{
		Target:    target,
		Operation: operation,
		RequestID: requestID,
		At:        time.Now().UTC(),
		State:     "unknown",
		Message:   message,
	}
	u.saveLocked()
}

// MarkUnresolved keeps the entry after the server reports no record.
func (u *unknownWrites) MarkUnresolved(target, requestID, message string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := u.items[key(target, requestID)]
	if entry == nil {
		return
	}
	entry.State = "unresolved"
	entry.Message = message
	u.saveLocked()
}

// Resolve removes a now-known request.
func (u *unknownWrites) Resolve(target, requestID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.items, key(target, requestID))
	u.saveLocked()
}

// ForTarget returns the entries of one target.
func (u *unknownWrites) ForTarget(target string) []*UnknownWrite {
	u.mu.Lock()
	defer u.mu.Unlock()
	var result []*UnknownWrite
	for _, entry := range u.items {
		if entry.Target == target {
			result = append(result, entry)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].At.Before(result[j].At) })
	return result
}

// List returns every entry, newest last.
func (u *unknownWrites) List() []*UnknownWrite {
	u.mu.Lock()
	defer u.mu.Unlock()
	result := make([]*UnknownWrite, 0, len(u.items))
	for _, entry := range u.items {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].At.Before(result[j].At) })
	return result
}
