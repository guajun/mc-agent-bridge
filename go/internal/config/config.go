// Package config owns the on-disk contract of the Go daemon: the home
// directory, targets, the credential store and the daemon endpoint file.
//
// Nothing in this package talks to the game. Every file that can hold a secret
// is written 0600 and never echoed by the CLI in full.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

const (
	EnvHome       = "MC_AGENT_HOME"
	EnvDaemonAddr = "MC_AGENT_DAEMON_ADDR"
	EnvIPCToken   = "MC_AGENT_IPC_TOKEN"
	EnvTarget     = "MC_AGENT_TARGET"

	TargetsFile = "targets.json"
	SecretsFile = "secrets.json"
	DaemonFile  = "daemon.json"

	// FileMode is used for every file that may contain a credential.
	FileMode = 0o600
	DirMode  = 0o700
)

// Target is one game endpoint the daemon can connect to.
type Target struct {
	Name       string `json:"name"`
	Transport  string `json:"transport"` // remote | legacy | fake
	Address    string `json:"address"`   // host:port (remote and legacy TCP)
	Pin        string `json:"pin,omitempty"`
	CAFile     string `json:"caFile,omitempty"`
	ServerName string `json:"serverName,omitempty"`
	TokenEnv   string `json:"tokenEnv,omitempty"`
	TokenFile  string `json:"tokenFile,omitempty"`
	// Legacy-only endpoint discovery, mirroring the Python bridge.
	PortFile  string `json:"portFile,omitempty"`
	ServerDir string `json:"serverDir,omitempty"`
	Vantage   string `json:"vantage,omitempty"`
	// RequireExplicit documents that this target must be selected by name.
	RequireExplicit bool `json:"requireExplicit,omitempty"`
}

// Validate checks the fields the transport needs.
func (t *Target) Validate() error {
	if t.Name == "" {
		return errors.New("target needs a name")
	}
	switch t.Transport {
	case protocol.TransportRemote:
		if t.Address == "" {
			return fmt.Errorf("remote target %q needs --address host:port", t.Name)
		}
		if t.Pin == "" && t.CAFile == "" {
			return fmt.Errorf("remote target %q needs --pin sha256:<hex> or --ca <pem>; "+
				"the daemon never disables certificate verification", t.Name)
		}
	case protocol.TransportLegacy:
		if t.Address == "" && t.PortFile == "" && t.ServerDir == "" {
			return fmt.Errorf("legacy target %q needs --address, --port-file or --server-dir", t.Name)
		}
	case protocol.TransportFake:
	default:
		return fmt.Errorf("target %q has unknown transport %q", t.Name, t.Transport)
	}
	return nil
}

// Targets is the targets.json document.
type Targets struct {
	Version int                `json:"version"`
	Default string             `json:"default,omitempty"`
	Targets map[string]*Target `json:"targets"`
}

// Secrets is the secrets.json document: target name -> bearer token.
type Secrets struct {
	Version int               `json:"version"`
	Tokens  map[string]string `json:"tokens"`
}

// DaemonState is the daemon.json document. The token is the local IPC
// credential; it is only readable by the user that started the daemon.
type DaemonState struct {
	PID       int       `json:"pid"`
	Address   string    `json:"address"`
	Token     string    `json:"token"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	Targets   []string  `json:"targets"`
	LogFile   string    `json:"logFile,omitempty"`
}

// Home resolves the mc-agent state directory. MC_AGENT_HOME wins; otherwise the
// per-user configuration directory is used (LOCALAPPDATA on Windows,
// XDG_CONFIG_HOME or ~/.config on Unix, Application Support on macOS).
func Home() (string, error) {
	if value := strings.TrimSpace(os.Getenv(EnvHome)); value != "" {
		return filepath.Abs(value)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the user configuration directory: %w", err)
	}
	return filepath.Join(base, "mc-agent"), nil
}

func ensureDir(path string) error {
	return os.MkdirAll(path, DirMode)
}

// LoadTargets reads targets.json; a missing file is an empty, valid document.
func LoadTargets(home string) (*Targets, error) {
	document := &Targets{Version: 1, Targets: map[string]*Target{}}
	path := filepath.Join(home, TargetsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, document); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if document.Targets == nil {
		document.Targets = map[string]*Target{}
	}
	for name, target := range document.Targets {
		if target.Name == "" {
			target.Name = name
		}
	}
	return document, nil
}

// SaveTargets writes targets.json atomically.
func SaveTargets(home string, document *Targets) error {
	if document.Version == 0 {
		document.Version = 1
	}
	return writeJSON(filepath.Join(home, TargetsFile), document, 0o644)
}

// LoadSecrets reads secrets.json; a missing file is an empty store.
func LoadSecrets(home string) (*Secrets, error) {
	document := &Secrets{Version: 1, Tokens: map[string]string{}}
	path := filepath.Join(home, SecretsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, document); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if document.Tokens == nil {
		document.Tokens = map[string]string{}
	}
	return document, nil
}

// SaveSecrets writes secrets.json with restrictive permissions.
func SaveSecrets(home string, document *Secrets) error {
	if document.Version == 0 {
		document.Version = 1
	}
	return writeJSON(filepath.Join(home, SecretsFile), document, FileMode)
}

// SaveDaemonState writes daemon.json with restrictive permissions.
func SaveDaemonState(home string, state *DaemonState) error {
	return writeJSON(filepath.Join(home, DaemonFile), state, FileMode)
}

// LoadDaemonState reads daemon.json; a missing file means "no daemon".
func LoadDaemonState(home string) (*DaemonState, error) {
	path := filepath.Join(home, DaemonFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state := &DaemonState{}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return state, nil
}

// RemoveDaemonState deletes the endpoint file; missing is not an error.
func RemoveDaemonState(home string) error {
	err := os.Remove(filepath.Join(home, DaemonFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Token resolves the bearer token for a target: the secret store first, then
// the configured environment variable, then the token file. The token is never
// included in any status payload.
func Token(home string, target *Target) (string, string, error) {
	secrets, err := LoadSecrets(home)
	if err == nil {
		if value := strings.TrimSpace(secrets.Tokens[target.Name]); value != "" {
			return value, "store", nil
		}
	}
	if target.TokenEnv != "" {
		if value := strings.TrimSpace(os.Getenv(target.TokenEnv)); value != "" {
			return value, "env:" + target.TokenEnv, nil
		}
	}
	if target.TokenFile != "" {
		data, err := os.ReadFile(target.TokenFile)
		if err != nil {
			return "", "", fmt.Errorf("cannot read token file %s: %w", target.TokenFile, err)
		}
		if value := strings.TrimSpace(string(data)); value != "" {
			return value, "file:" + target.TokenFile, nil
		}
	}
	return "", "missing", fmt.Errorf("no credential for target %q: run `mc-agent target add ... --token-stdin` "+
		"or set %s", target.Name, target.TokenEnv)
}

// Resolve picks a target by name. An empty name uses the configured default, or
// the only configured target. More than one target without an explicit choice
// is an error: the daemon never silently picks a world.
func Resolve(document *Targets, name, environmentDefault string) (*Target, error) {
	if name == "" {
		name = strings.TrimSpace(os.Getenv(EnvTarget))
	}
	if name == "" {
		name = environmentDefault
	}
	if name == "" {
		name = document.Default
	}
	if name != "" {
		target, ok := document.Targets[name]
		if !ok {
			return nil, &protocol.Error{Code: protocol.CodeTargetUnknown,
				Message: fmt.Sprintf("no target named %q; configured: %s", name, strings.Join(TargetNames(document), ", "))}
		}
		if target.RequireExplicit && document.Default == target.Name {
			// still explicit: the name was given or is the configured default
		}
		return target, nil
	}
	names := TargetNames(document)
	switch len(names) {
	case 0:
		return nil, &protocol.Error{Code: protocol.CodeTargetUnknown,
			Message: "no targets configured; add one with `mc-agent target add <name> ...`"}
	case 1:
		return document.Targets[names[0]], nil
	default:
		return nil, &protocol.Error{Code: protocol.CodeTargetRequired,
			Message: "multiple targets are configured (" + strings.Join(names, ", ") + "); pass --target <name>"}
	}
}

// TargetNames returns the sorted target names.
func TargetNames(document *Targets) []string {
	names := make([]string, 0, len(document.Targets))
	for name := range document.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func writeJSON(path string, value any, mode os.FileMode) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// RestrictFile tightens permissions on platforms that support it; on Windows
// the per-user directory ACL is the boundary.
func RestrictFile(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return os.Chmod(path, FileMode)
}
