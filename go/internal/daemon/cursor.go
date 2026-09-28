package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/guajun/mc-agent-bridge/internal/config"
)

// cursorStore remembers, per target, which (instance, run) the event cursor
// belongs to. A cursor is only ever offered back to the server together with
// its run id, so a reconnecting daemon can never present a previous run's
// high-water mark to a restarted game and silently skip the new run's events.
type cursorStore struct {
	path  string
	mu    sync.Mutex
	state map[string]cursor
	dirty bool
}

type cursor struct {
	InstanceID string `json:"instanceId,omitempty"`
	RunID      string `json:"runId,omitempty"`
	LastSeq    int64  `json:"lastSeq"`
}

const cursorsFile = "cursors.json"

func newCursorStore(home string) *cursorStore {
	return &cursorStore{path: filepath.Join(home, cursorsFile), state: map[string]cursor{}}
}

// Load reads cursors.json; a missing file is empty and a corrupt file is
// treated as empty (cursors are an optimization; the server reports a gap).
func (c *cursorStore) Load() {
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		return
	}
	var loaded map[string]cursor
	if err := json.Unmarshal(data, &loaded); err != nil {
		return
	}
	c.mu.Lock()
	for key, value := range loaded {
		c.state[key] = value
	}
	c.mu.Unlock()
}

// Get returns the cursor for a target.
func (c *cursorStore) Get(target string) cursor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state[target]
}

// Set records the target's cursor; the caller decides when to persist.
func (c *cursorStore) Set(target string, value cursor) {
	c.mu.Lock()
	c.state[target] = value
	c.dirty = true
	c.mu.Unlock()
}

// Flush persists pending cursor updates.
func (c *cursorStore) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	data, err := json.MarshalIndent(c.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), config.DirMode); err != nil {
		return err
	}
	temporary := c.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), config.FileMode); err != nil {
		return err
	}
	if err := os.Rename(temporary, c.path); err != nil {
		return err
	}
	c.dirty = false
	return nil
}
