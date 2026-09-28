package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
)

// UnknownWrite remembers a non-idempotent request whose outcome is not known.
// It is persisted before the request can leave the process, so a daemon crash
// between write and reply still leaves the request visible for reconciliation.
type UnknownWrite struct {
	Target    string    `json:"target"`
	Operation string    `json:"operation"`
	RequestID string    `json:"requestId"`
	At        time.Time `json:"at"`
	State     string    `json:"state"` // unknown | unresolved
	Message   string    `json:"message,omitempty"`
	// Scope ties the request to the connection that sent it. A restart or a
	// credential change is detected before a request_status query is trusted.
	InstanceID     string `json:"instanceId,omitempty"`
	RunID          string `json:"runId,omitempty"`
	CredentialHash string `json:"credentialHash,omitempty"`
}

// unknownWrites is the bounded, persisted ledger.
type unknownWrites struct {
	path  string
	mu    sync.Mutex
	items map[string]*UnknownWrite
}

const (
	unknownWritesFile = "unknown_writes.json"
	unknownWritesCap  = 512
)

func newUnknownWrites(home string) *unknownWrites {
	return &unknownWrites{
		path:  filepath.Join(home, unknownWritesFile),
		items: map[string]*UnknownWrite{},
	}
}

func key(target, requestID string) string {
	return target + "\x00" + requestID
}

// Load reads the ledger. A missing file is empty. A corrupt file is moved
// aside and reported so the daemon can keep running without silently losing
// recovery data; any other read failure is returned.
func (u *unknownWrites) Load() (string, error) {
	data, err := os.ReadFile(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var list []*UnknownWrite
	if err := json.Unmarshal(data, &list); err != nil {
		backup := fmt.Sprintf("%s.corrupt-%d", u.path, time.Now().Unix())
		if renameErr := os.Rename(u.path, backup); renameErr != nil {
			return "", fmt.Errorf("unknown-write ledger is corrupt and cannot be moved aside: %w", err)
		}
		return "moved the corrupt ledger to " + backup, nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, entry := range list {
		if entry == nil || entry.RequestID == "" {
			continue
		}
		u.items[key(entry.Target, entry.RequestID)] = entry
	}
	u.trimLocked()
	return "", nil
}

func (u *unknownWrites) saveLocked() error {
	list := make([]*UnknownWrite, 0, len(u.items))
	for _, entry := range u.items {
		list = append(list, entry)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.path), config.DirMode); err != nil {
		return err
	}
	temporary := u.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), config.FileMode); err != nil {
		return err
	}
	return os.Rename(temporary, u.path)
}

// trimLocked caps the ledger. Oldest entries are dropped first; terminal
// (resolved/unresolved) entries are dropped before still-unknown ones.
func (u *unknownWrites) trimLocked() {
	if len(u.items) <= unknownWritesCap {
		return
	}
	list := make([]*UnknownWrite, 0, len(u.items))
	for _, entry := range u.items {
		list = append(list, entry)
	}
	sort.Slice(list, func(i, j int) bool {
		terminalI := list[i].State != "unknown"
		terminalJ := list[j].State != "unknown"
		if terminalI != terminalJ {
			return terminalI
		}
		return list[i].At.Before(list[j].At)
	})
	for _, entry := range list {
		if len(u.items) <= unknownWritesCap {
			break
		}
		delete(u.items, key(entry.Target, entry.RequestID))
	}
}

// Record persists a request before it can be sent. A persistence failure is
// returned so the caller can refuse to send instead of losing recovery data.
func (u *unknownWrites) Record(target, operation, requestID, instanceID, runID, credentialHash string) error {
	if requestID == "" {
		return errors.New("refusing to track a write without a request id")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := &UnknownWrite{
		Target:         target,
		Operation:      operation,
		RequestID:      requestID,
		At:             time.Now().UTC(),
		State:          "unknown",
		InstanceID:     instanceID,
		RunID:          runID,
		CredentialHash: credentialHash,
	}
	u.items[key(target, requestID)] = entry
	if err := u.saveLocked(); err != nil {
		delete(u.items, key(target, requestID))
		return fmt.Errorf("cannot persist the unknown-write ledger: %w", err)
	}
	return nil
}

// NoteUnknown refreshes the message on a still-unknown request.
func (u *unknownWrites) NoteUnknown(target, requestID, message string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := u.items[key(target, requestID)]
	if entry == nil {
		return
	}
	entry.State = "unknown"
	entry.Message = message
	_ = u.saveLocked()
}

// MarkUnresolved keeps the entry after the server reports no record.
func (u *unknownWrites) MarkUnresolved(target, requestID, message string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := u.items[key(target, requestID)]
	if entry == nil {
		return nil
	}
	entry.State = "unresolved"
	entry.Message = message
	if err := u.saveLocked(); err != nil {
		return err
	}
	return nil
}

// Resolve removes a now-known request.
func (u *unknownWrites) Resolve(target, requestID string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry := u.items[key(target, requestID)]
	if entry == nil {
		return nil
	}
	delete(u.items, key(target, requestID))
	if err := u.saveLocked(); err != nil {
		u.items[key(target, requestID)] = entry
		return err
	}
	return nil
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

// credentialFingerprint hashes a bearer token so the ledger can detect a
// credential change without storing the secret.
func credentialFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}
