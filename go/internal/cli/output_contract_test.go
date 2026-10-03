package cli

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/guajun/mc-agent-bridge/internal/protocol"
	"github.com/guajun/mc-agent-bridge/internal/version"
)

// Capture Main's actual process-facing streams and exit code. These tests must
// not run in parallel because Main uses the process stdout and stderr.
func captureMain(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		outRead.Close()
		outWrite.Close()
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outWrite, errWrite
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	stdout, stderr := make(chan string, 1), make(chan string, 1)
	go func() { data, _ := io.ReadAll(outRead); outRead.Close(); stdout <- string(data) }()
	go func() { data, _ := io.ReadAll(errRead); errRead.Close(); stderr <- string(data) }()
	code := Main(args)
	outWrite.Close()
	errWrite.Close()
	return code, <-stdout, <-stderr
}

func TestVersionOutputContract(t *testing.T) {
	t.Setenv("MC_AGENT_HOME", t.TempDir())
	code, out, errOut := captureMain(t, "version")
	if code != 0 || errOut != "" || !strings.HasPrefix(out, "mc-agent "+version.Version+" (control protocol ") || json.Valid([]byte(out)) {
		t.Fatalf("text version: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = captureMain(t, "--pretty", "version")
	var result struct {
		Version         string `json:"version"`
		ControlProtocol int    `json:"controlProtocol"`
		ModMinVersion   string `json:"modMinVersion"`
	}
	if code != 0 || errOut != "" || json.Unmarshal([]byte(out), &result) != nil || result.Version != version.Version || result.ControlProtocol != protocol.ControlProtocolVersion || result.ModMinVersion != version.ModMinVersion {
		t.Fatalf("JSON version: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestHelpAndUsageOutputContract(t *testing.T) {
	t.Setenv("MC_AGENT_HOME", t.TempDir())
	for _, command := range []string{"help", "--help", "-h"} {
		code, out, errOut := captureMain(t, command)
		if code != 0 || out != "" || !strings.Contains(errOut, "Usage: mc-agent") || !strings.Contains(errOut, "--pretty version") {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", command, code, out, errOut)
		}
	}
	code, out, errOut := captureMain(t)
	if code != 2 || out != "" || !strings.Contains(errOut, "Usage: mc-agent") {
		t.Fatalf("no command: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = captureMain(t, "--home")
	var failure struct {
		OK    bool           `json:"ok"`
		Error protocol.Error `json:"error"`
	}
	if code != 2 || out != "" || json.Unmarshal([]byte(errOut), &failure) != nil || failure.OK || failure.Error.Code != protocol.CodeUsage {
		t.Fatalf("usage error: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = captureMain(t, "not-a-command")
	lines := strings.Split(strings.TrimSpace(errOut), "\n")
	if code == 0 || out != "" || !strings.Contains(errOut, "Usage: mc-agent") || json.Unmarshal([]byte(lines[len(lines)-1]), &failure) != nil || failure.OK || failure.Error.Code == "" {
		t.Fatalf("unknown command: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}
