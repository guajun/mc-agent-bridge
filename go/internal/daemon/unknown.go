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

// unknownWrites is the bounded, persisted ledger. The capacity is a hard
// admission bound: a full ledger refuses new writes before they can leave the
// process, and existing recovery entries are never silently evicted (an
// evicted unresolved request would be a request nobody can ever resolve).
type unknownWrites struct {
	path  string
	mu    sync.Mutex
	items map[string]UnknownWrite
}

const (
	unknownWritesFile = "unknown_writes.json"
	unknownWritesCap  = 512
)

func newUnknownWrites(home string) *unknownWrites {
	return &unknownWrites{
		path:  filepath.Join(home, unknownWritesFile),
		items: map[string]UnknownWrite{},
	}
}

func key(target, requestID string) string {
	return target + "\x00" + requestID
}

// Load reads the ledger. A missing file is empty. A corrupt file is moved
// aside and reported so the daemon can keep running without silently losing
// recovery data; any other read failure is returned. Entries beyond the
// capacity are preserved as-is (never dropped) and reported so the operator
// can drain them.
func (u *unknownWrites) Load() (string, error) {
	data, err := os.ReadFile(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var list []UnknownWrite
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
		if entry.RequestID == "" {
			continue
		}
		u.items[key(entry.Target, entry.RequestID)] = entry
	}
	if len(u.items) > unknownWritesCap {
		return fmt.Sprintf(
			"the unknown-write ledger holds %d entries, above the %d entry admission cap; "+
				"new non-idempotent writes are refused until these are reconciled",
			len(u.items), unknownWritesCap), nil
	}
	return "", nil
}

func (u *unknownWrites) saveLocked() error {
	list := make([]UnknownWrite, 0, len(u.items))
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

// Record persists a request before it can be sent. A full ledger or a
// persistence failure is returned so the caller refuses to send instead of
// losing recovery data.
func (u *unknownWrites) Record(target, operation, requestID, instanceID, runID, credentialHash string) error {
	if requestID == "" {
		return errors.New("refusing to track a write without a request id")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, exists := u.items[key(target, requestID)]; !exists && len(u.items) >= unknownWritesCap {
		return fmt.Errorf("the unknown-write ledger is full (%d entries); reconcile it before sending", unknownWritesCap)
	}
	entry := UnknownWrite{
		Target:         target,
		Operation:      operation,
		RequestID:      requestID,
		At:             time.Now().UTC(),
		State:          "unknown",
		InstanceID:     instanceID,
		RunID:          runID,
		CredentialHash: credentialHash,
	}
	previous, existed := u.items[key(target, requestID)]
	u.items[key(target, requestID)] = entry
	if err := u.saveLocked(); err != nil {
		if existed {
			u.items[key(target, requestID)] = previous
		} else {
			delete(u.items, key(target, requestID))
		}
		return fmt.Errorf("cannot persist the unknown-write ledger: %w", err)
	}
	return nil
}

// NoteUnknown refreshes the message on a still-unknown request.
func (u *unknownWrites) NoteUnknown(target, requestID, message string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry, exists := u.items[key(target, requestID)]
	if !exists {
		return nil
	}
	previous := entry
	entry.State = "unknown"
	entry.Message = message
	u.items[key(target, requestID)] = entry
	if err := u.saveLocked(); err != nil {
		u.items[key(target, requestID)] = previous
		return err
	}
	return nil
}

// MarkUnresolved keeps the entry after the server reports no record.
func (u *unknownWrites) MarkUnresolved(target, requestID, message string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry, exists := u.items[key(target, requestID)]
	if !exists {
		return nil
	}
	previous := entry
	entry.State = "unresolved"
	entry.Message = message
	u.items[key(target, requestID)] = entry
	if err := u.saveLocked(); err != nil {
		u.items[key(target, requestID)] = previous
		return err
	}
	return nil
}

// Resolve removes a now-known request.
func (u *unknownWrites) Resolve(target, requestID string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry, exists := u.items[key(target, requestID)]
	if !exists {
		return nil
	}
	delete(u.items, key(target, requestID))
	if err := u.saveLocked(); err != nil {
		u.items[key(target, requestID)] = entry
		return err
	}
	return nil
}

// ForTarget returns copies of one target's entries so reconciliation and
// status readers cannot race with concurrent mutations.
func (u *unknownWrites) ForTarget(target string) []UnknownWrite {
	u.mu.Lock()
	defer u.mu.Unlock()
	var result []UnknownWrite
	for _, entry := range u.items {
		if entry.Target == target {
			result = append(result, entry)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].At.Before(result[j].At) })
	return result
}

// List returns copies of every entry, oldest first.
func (u *unknownWrites) List() []UnknownWrite {
	u.mu.Lock()
	defer u.mu.Unlock()
	result := make([]UnknownWrite, 0, len(u.items))
	for _, entry := range u.items {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].At.Before(result[j].At) })
	return result
}

// Count returns the ledger size.
func (u *unknownWrites) Count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.items)
}

// credentialFingerprint hashes a bearer token so the ledger can detect a
// credential change without storing the secret.
func credentialFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}
