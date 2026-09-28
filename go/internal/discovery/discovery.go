// Package discovery mirrors the Python bridge's legacy endpoint discovery:
// the mod writes the port it bound to into <gameDir>/mc-agent-server/port.txt
// (server vantage) or <gameDir>/mc-agent/port.txt (client vantage).
//
// The server vantage never guesses a port when the file is missing; the
// client vantage keeps the Python bridge's historical default for explicit
// legacy use only.
package discovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	VantageServer = "server"
	VantageClient = "client"

	PortFileName  = "port.txt"
	ServerDirName = "mc-agent-server"
	ClientDirName = "mc-agent"
	DefaultServer = 25581
	DefaultClient = 25580

	EnvPortFile  = "MC_AGENT_PORT_FILE"
	EnvServerDir = "MC_AGENT_SERVER_DIR"
)

// Result reports where to dial the legacy mod and how that answer was reached.
type Result struct {
	Vantage string   `json:"vantage"`
	Port    int      `json:"port,omitempty"`
	Source  string   `json:"source"`
	Checked []string `json:"checked,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Resolved reports whether a port was found.
func (r Result) Resolved() bool { return r.Port != 0 }

// Resolve finds the legacy endpoint. Priority: explicit port, explicit port
// file, vantage-specific discovery.
func Resolve(explicitPort int, portFile, vantage, serverDir string) Result {
	if vantage == "" {
		vantage = VantageServer
	}
	if explicitPort != 0 {
		if explicitPort < 1 || explicitPort > 65535 {
			return Result{Vantage: vantage, Source: "argument",
				Error: fmt.Sprintf("--mod-port %d is not a valid TCP port (1-65535)", explicitPort)}
		}
		return Result{Vantage: vantage, Port: explicitPort, Source: "argument"}
	}
	file := portFile
	if file == "" {
		file = os.Getenv(EnvPortFile)
	}
	if file != "" {
		port, problem := readPort(file)
		if port != 0 {
			return Result{Vantage: vantage, Port: port, Source: "port-file:" + file, Checked: []string{file}}
		}
		if problem == "" {
			problem = "does not exist"
		}
		return Result{Vantage: vantage, Source: "port-file", Checked: []string{file},
			Error: fmt.Sprintf("the port file %q %s", file, problem)}
	}
	if vantage == VantageServer {
		dir := serverDir
		if dir == "" {
			dir = os.Getenv(EnvServerDir)
		}
		return resolveServer(dir)
	}
	return resolveClient()
}

func resolveServer(serverDir string) Result {
	explicit := serverDir != ""
	root := serverDir
	if root == "" {
		root, _ = os.Getwd()
	}
	root, _ = filepath.Abs(root)
	checked := []string{filepath.Join(root, ServerDirName, PortFileName)}
	if explicit {
		checked = append(checked, filepath.Join(root, PortFileName))
	}
	for _, path := range checked {
		port, problem := readPort(path)
		if port != 0 {
			return Result{Vantage: VantageServer, Port: port, Source: "port-file:" + path, Checked: checked}
		}
		if problem != "" {
			return serverError(root, checked, fmt.Sprintf("%q %s", path, problem))
		}
	}
	return serverError(root, checked, "")
}

func serverError(root string, checked []string, detail string) Result {
	message := "cannot find the server-vantage port file"
	if detail != "" {
		message += ": " + detail
	} else {
		message += fmt.Sprintf(": no readable %s", filepath.Join(root, ServerDirName, PortFileName))
	}
	message += "\nThe mod writes it when its server entrypoint starts. Fix one of:\n" +
		"  --server-dir <game-dir>   where mc-agent-server/ lives (or set " + EnvServerDir + ")\n" +
		"  --port-file <path>        the exact port.txt to read (or set " + EnvPortFile + ")\n" +
		"  --address host:port       the port itself (25581 by default, or the next free one)"
	return Result{Vantage: VantageServer, Source: "unresolved", Checked: checked, Error: message}
}

func resolveClient() Result {
	root, _ := os.Getwd()
	checked := []string{
		filepath.Join(root, PortFileName),
		filepath.Join(root, ClientDirName, PortFileName),
	}
	for _, path := range checked {
		port, problem := readPort(path)
		if port != 0 {
			return Result{Vantage: VantageClient, Port: port, Source: "port-file:" + path, Checked: checked}
		}
		if problem != "" {
			return Result{Vantage: VantageClient, Source: "unresolved", Checked: checked,
				Error: fmt.Sprintf("the client-vantage port file %q %s", path, problem)}
		}
	}
	return Result{Vantage: VantageClient, Port: DefaultClient, Source: "default-client", Checked: checked}
}

func readPort(path string) (int, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ""
		}
		return 0, fmt.Sprintf("cannot be read (%v)", err)
	}
	text := strings.TrimSpace(string(data))
	port, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Sprintf("does not contain a port number (found %q)", text)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Sprintf("contains %d, which is not a valid TCP port (1-65535)", port)
	}
	return port, ""
}
