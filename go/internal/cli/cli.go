// Package cli is the short-lived mc-agent command surface. Every game call
// goes through the daemon over the token-protected loopback IPC; the CLI
// itself never opens a socket to the game.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/ipc"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
	"github.com/guajun/mc-agent-bridge/internal/session"
	"github.com/guajun/mc-agent-bridge/internal/version"
	"github.com/guajun/mc-agent-bridge/internal/webhook"
)

type app struct {
	home     string
	target   string
	pretty   bool
	asJSON   bool
	stdout   io.Writer
	stderr   io.Writer
	exitCode int

	// End-to-end write ids are allocated before the request leaves the CLI so
	// a lost local reply still names the exact write.
	requestNonce string
	requestSeq   atomic.Uint64
}

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	a := &app{pretty: false, asJSON: true, stdout: os.Stdout, stderr: os.Stderr}
	remaining, err := a.parseGlobal(args)
	if err != nil {
		a.printError(protocol.NewError(protocol.CodeUsage, err.Error()))
		return protocol.ExitCode(protocol.CodeUsage)
	}
	if len(remaining) == 0 {
		a.printUsage()
		return protocol.ExitCode(protocol.CodeUsage)
	}
	command, rest := remaining[0], remaining[1:]
	if a.home == "" {
		resolved, err := config.Home()
		if err != nil {
			a.printError(protocol.NewError(protocol.CodeInternal, err.Error()))
			return protocol.ExitCode(protocol.CodeInternal)
		}
		a.home = resolved
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var result any
	var failure *protocol.Error
	switch command {
	case "version", "--version", "-v":
		result, failure = a.cmdVersion(rest)
	case "daemon":
		result, failure = a.cmdDaemon(ctx, rest)
	case "target":
		result, failure = a.cmdTarget(rest)
	case "capabilities", "caps":
		result, failure = a.daemonOp(ctx, "capabilities", map[string]any{})
	case "schema":
		result, failure = a.cmdSchema(ctx, rest)
	case "call":
		result, failure = a.cmdCall(ctx, rest)
	case "status":
		result, failure = a.daemonCall(ctx, "status", map[string]any{})
	case "events", "watch":
		result, failure = a.cmdEvents(ctx, rest)
	case "requests":
		result, failure = a.daemonCall(ctx, "requests", map[string]any{})
	case "doctor":
		result, failure = a.cmdDoctor(ctx, rest)
	case "help", "--help", "-h":
		a.printUsage()
		return 0
	default:
		result, failure = a.cmdConvenience(ctx, command, rest)
		if failure != nil && failure.Code == protocol.CodeCapabilityNotSupported &&
			strings.Contains(failure.Message, "unknown bridge method") {
			a.printUsage()
		}
	}
	if failure != nil {
		a.printError(failure)
		return protocol.ExitCode(failure.Code)
	}
	if result != nil {
		a.print(result)
	}
	return a.exitCode
}

func (a *app) parseGlobal(args []string) ([]string, error) {
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--home":
			if index+1 >= len(args) {
				return nil, errors.New("--home needs a directory")
			}
			index++
			a.home = args[index]
		case strings.HasPrefix(argument, "--home="):
			a.home = strings.TrimPrefix(argument, "--home=")
		case argument == "--target" || argument == "-t":
			if index+1 >= len(args) {
				return nil, errors.New("--target needs a name")
			}
			index++
			a.target = args[index]
		case strings.HasPrefix(argument, "--target="):
			a.target = strings.TrimPrefix(argument, "--target=")
		case argument == "--pretty":
			a.pretty = true
		case argument == "--json":
			a.asJSON = true
		default:
			remaining = append(remaining, argument)
		}
	}
	if a.target == "" {
		a.target = os.Getenv(config.EnvTarget)
	}
	if a.home == "" {
		a.home = os.Getenv(config.EnvHome)
	}
	return remaining, nil
}

func (a *app) withTarget(params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	if a.target != "" {
		params["target"] = a.target
	}
	return params
}

func (a *app) print(value any) {
	var payload []byte
	var err error
	if a.pretty {
		payload, err = json.MarshalIndent(value, "", "  ")
	} else {
		payload, err = json.Marshal(value)
	}
	if err != nil {
		a.printError(protocol.NewError(protocol.CodeInternal, err.Error()))
		return
	}
	fmt.Fprintln(a.stdout, string(payload))
}

func (a *app) printError(failure *protocol.Error) {
	if failure.ResultUnknown && failure.RequestID != "" && failure.Hint == "" {
		target := ""
		if a.target != "" {
			target = " --target " + a.target
		}
		failure.Hint = fmt.Sprintf("the write may have executed; check with `mc-agent request-status %s%s`",
			failure.RequestID, target)
	}
	payload, _ := json.Marshal(map[string]any{"ok": false, "error": failure})
	fmt.Fprintln(a.stderr, string(payload))
}

// newRequestID returns an id unique across CLI processes and runs. A caller
// may pass its own id explicitly; otherwise writes get one automatically.
func (a *app) newRequestID() string {
	if a.requestNonce == "" {
		buffer := make([]byte, 8)
		if _, err := rand.Read(buffer); err != nil {
			a.requestNonce = strconv.FormatInt(time.Now().UnixNano(), 36)
		} else {
			a.requestNonce = hex.EncodeToString(buffer)
		}
	}
	return fmt.Sprintf("cli-%s-%d", a.requestNonce, a.requestSeq.Add(1))
}

// reorderInterspersed moves flags before positional arguments so a subcommand
// accepts `target add name --pin x` as well as `target add --pin x name`.
// The Go flag package stops at the first positional argument by design.
func reorderInterspersed(args []string, valueFlags map[string]bool) []string {
	flagsPart := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if strings.HasPrefix(argument, "-") && argument != "-" {
			flagsPart = append(flagsPart, argument)
			name := strings.TrimLeft(argument, "-")
			if equals := strings.IndexByte(name, '='); equals >= 0 {
				name = name[:equals]
			}
			if !strings.Contains(argument, "=") && valueFlags[name] && index+1 < len(args) {
				index++
				flagsPart = append(flagsPart, args[index])
			}
			continue
		}
		positional = append(positional, argument)
	}
	return append(flagsPart, positional...)
}

func flagSet(name string) (*flag.FlagSet, map[string]bool) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags, map[string]bool{}
}

func (a *app) printUsage() {
	fmt.Fprintln(a.stderr, `mc-agent - Go CLI/daemon for the mc-agent Minecraft Toolkit

Usage: mc-agent [--home DIR] [--target NAME] [--pretty] <command> [args]

Discovery:
  version                  human-readable product/protocol/mod versions
  --pretty version         the same compatibility information as JSON
  help                     this text on stderr

Daemon:
  daemon run [--fake] [--api-address ADDR] [--buffer N] [--reconnect-delay DURATION]
  daemon start [...]        run detached and wait for readiness
  daemon stop|status|doctor
Targets:
  target add <name> --transport remote --address HOST:PORT (--pin sha256:HEX | --ca FILE)
                    [--token-stdin | --token-env VAR | --token-file FILE] [--default]
  target add <name> --transport legacy [--address HOST:PORT | --port-file FILE | --server-dir DIR]
                    [--vantage client|server]
  target list|show <name>|remove <name>|use <name>|reload
Calls:
  capabilities | schema [op] | call <op> [--params JSON] [--timeout SECONDS]
  status | state | player <name|uuid> | entities [--dimension ID] | context <id>
  command <line> | command-output <line> [--wait SECONDS] | mark <text>
  wait <ticks> | save | snapshot [--name NAME] [--dimension ID] | snapshots
  chat | screen | connect | world | lan | record-start | record-stop   (legacy client vantage)
  events [--since N] [--limit N] [--category C] [--follow] | requests | doctor

Finite data commands print one JSON value on stdout. Exceptions: version prints
text (use --pretty version for JSON); help prints text on stderr and exits 0; events --follow
streams JSON values; daemon run stays in the foreground without a final result.
Errors use JSON on stderr with a stable code and non-zero exit code; usage
failures may also print help text; invoking without a command prints only help
and exits 2.
Research boundary (Minecraft 26.2): entities/snapshot capture entity NBT and tick
order, not a full world or runtime checkpoint. Go world fork/restore is not yet
supported. save reads metadata; command "save-all" performs a game save and can
change runtime state through save maintenance. Such save effects are an accepted
boundary. For save-sensitive experiments, fork an earlier clean baseline using
appropriate tooling, advance the branch, and verify the required conditions;
this does not guarantee reproduction of every intermediate state.
In-flight piston and exact random continuation are not guaranteed; where applicable,
start movement after branching a stationary baseline. Known experiment limits:
https://guajun.github.io/mc-agent/known-limitations/
There is no MCP path and no Python requirement.`)
}

// ---------------------------------------------------------------- daemon IPC

func (a *app) daemonAddressAndToken() (string, string, error) {
	if address := os.Getenv(config.EnvDaemonAddr); address != "" {
		token := os.Getenv(config.EnvIPCToken)
		if token == "" {
			return "", "", protocol.NewError(protocol.CodeUnauthorized,
				config.EnvDaemonAddr+" is set but "+config.EnvIPCToken+" is missing")
		}
		return address, token, nil
	}
	state, err := config.LoadDaemonState(a.home)
	if err != nil {
		return "", "", protocol.NewError(protocol.CodeInternal, err.Error())
	}
	if state == nil {
		return "", "", protocol.NewError(protocol.CodeDaemonNotRunning,
			"no daemon is running; start one with `mc-agent daemon start`")
	}
	return state.Address, state.Token, nil
}

func (a *app) connectDaemon() (*ipc.Client, *protocol.Error) {
	address, token, err := a.daemonAddressAndToken()
	if err != nil {
		var protocolErr *protocol.Error
		if errors.As(err, &protocolErr) {
			return nil, protocolErr
		}
		return nil, protocol.NewError(protocol.CodeInternal, err.Error())
	}
	// Bound the connect+ping handshake: a socket that accepts but never
	// answers ping must not hang the CLI forever.
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelDial()
	client, dialErr := ipc.Dial(dialCtx, address, token)
	if dialErr != nil {
		var protocolErr *protocol.Error
		if errors.As(dialErr, &protocolErr) {
			return nil, protocolErr
		}
		return nil, protocol.NewError(protocol.CodeDaemonNotRunning, dialErr.Error())
	}
	return client, nil
}

func (a *app) daemonCall(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
	return a.daemonCallID(ctx, method, params, "")
}

func (a *app) daemonCallID(ctx context.Context, method string, params map[string]any,
	requestID string) (any, *protocol.Error) {
	client, failure := a.connectDaemon()
	if failure != nil {
		return nil, failure
	}
	defer client.Close()
	if params == nil {
		params = map[string]any{}
	}
	if a.target != "" {
		params["target"] = a.target
	}
	if requestID != "" {
		params["requestId"] = requestID
	}
	return client.CallWithID(ctx, method, params, requestID)
}

func (a *app) daemonOp(ctx context.Context, operation string, params map[string]any) (any, *protocol.Error) {
	requestID := ""
	if protocol.WriteOperation(operation) {
		requestID = a.newRequestID()
	}
	return a.daemonCallOp(ctx, operation, params, requestID)
}

func (a *app) daemonCallOp(ctx context.Context, operation string, params map[string]any,
	requestID string) (any, *protocol.Error) {
	client, failure := a.connectDaemon()
	if failure != nil {
		return nil, failure
	}
	defer client.Close()
	if params == nil {
		params = map[string]any{}
	}
	if a.target != "" {
		params["target"] = a.target
	}
	if requestID != "" {
		params["requestId"] = requestID
	}
	return client.CallWithID(ctx, operation, params, requestID)
}

func (a *app) cmdVersion(args []string) (any, *protocol.Error) {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	_ = flags.Parse(args)
	value := map[string]any{
		"version":               version.Version,
		"controlProtocol":       protocol.ControlProtocolVersion,
		"modMinVersion":         version.ModMinVersion,
		"userAgent":             version.UserAgent,
		"mcp":                   false,
		"pythonRuntimeRequired": false,
	}
	if !a.pretty {
		fmt.Fprintf(a.stdout, "mc-agent %s (control protocol %d, mod >= %s)\n",
			version.Version, protocol.ControlProtocolVersion, version.ModMinVersion)
		return nil, nil
	}
	return value, nil
}

// ---------------------------------------------------------------- daemon cmd

func (a *app) cmdDaemon(ctx context.Context, args []string) (any, *protocol.Error) {
	if len(args) == 0 {
		return nil, protocol.NewError(protocol.CodeUsage, "daemon needs run|start|stop|status|doctor")
	}
	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "run":
		return a.daemonRun(rest)
	case "start":
		return a.daemonStart(rest)
	case "stop":
		result, failure := a.daemonCall(ctx, "stop", map[string]any{})
		if failure != nil {
			return nil, failure
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			state, _ := config.LoadDaemonState(a.home)
			if state == nil {
				return map[string]any{"stopped": true}, nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return result, nil
	case "status":
		return a.daemonStatus(ctx)
	case "doctor":
		return a.cmdDoctor(ctx, rest)
	default:
		return nil, protocol.NewError(protocol.CodeUsage, "unknown daemon command: "+subcommand)
	}
}

func daemonRunCreateWebhook(webhookURL, webhookSecret, webhookEvents, webhookFile string,
	webhookQueue, webhookAttempts int) (*webhook.Config, error) {
	if webhookURL == "" && webhookFile == "" && os.Getenv(webhook.EnvURL) == "" {
		return nil, nil
	}
	config, err := webhook.LoadConfig(webhookFile, splitList(webhookEvents), webhookQueue,
		webhookAttempts, 0, 0, 0)
	if err == nil && webhookSecret != "" {
		config.Secret = webhookSecret
	}
	if err != nil && webhookSecret != "" {
		config = &webhook.Config{URL: webhookURL, Secret: webhookSecret,
			Events: webhook.DefaultEvents, QueueSize: webhook.DefaultQueueSize,
			MaxAttempts: webhook.DefaultMaxAttempts, Backoff: webhook.DefaultBackoff,
			MaxBackoff: webhook.DefaultMaxBackoff, Timeout: webhook.DefaultTimeout}
		if len(splitList(webhookEvents)) > 0 {
			config.Events = splitList(webhookEvents)
		}
		err = config.Validate()
	}
	if err != nil {
		return nil, err
	}
	return config, nil
}

func (a *app) daemonRun(args []string) (any, *protocol.Error) {
	flags := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	apiAddress := flags.String("api-address", "", "loopback address for the local IPC")
	fake := flags.Bool("fake", false, "start an in-process fake mod and register a `fake` target")
	buffer := flags.Int("buffer", 1000, "events kept in the replay buffer")
	reconnect := flags.Duration("reconnect-delay", 2*time.Second, "reconnect delay")
	webhookURL := flags.String("webhook-url", "", "optional webhook URL")
	webhookSecret := flags.String("webhook-secret", "", "webhook HMAC secret")
	webhookEvents := flags.String("webhook-events", "", "comma-separated categories")
	webhookQueue := flags.Int("webhook-queue", 0, "webhook queue size")
	webhookAttempts := flags.Int("webhook-attempts", 0, "webhook max attempts")
	webhookFile := flags.String("webhook-config", "", "webhook JSON config file")
	if err := flags.Parse(args); err != nil {
		return nil, protocol.NewError(protocol.CodeUsage, err.Error())
	}
	var webhookConfig *webhook.Config
	webhookConfig, webhookErr := daemonRunCreateWebhook(*webhookURL, *webhookSecret, *webhookEvents,
		*webhookFile, *webhookQueue, *webhookAttempts)
	if webhookErr != nil {
		return nil, protocol.NewError(protocol.CodeUsage, webhookErr.Error())
	}
	options := daemonOptions{
		apiAddress:     *apiAddress,
		fake:           *fake,
		buffer:         *buffer,
		reconnectDelay: *reconnect,
		webhook:        webhookConfig,
	}
	return nil, a.runDaemon(options)
}

// daemonOptions is the shared daemon configuration for run/start.
type daemonOptions struct {
	apiAddress     string
	fake           bool
	buffer         int
	reconnectDelay time.Duration
	webhook        *webhook.Config
}

func (a *app) daemonStart(args []string) (any, *protocol.Error) {
	flags := flag.NewFlagSet("daemon start", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	apiAddress := flags.String("api-address", "", "loopback address for the local IPC")
	fake := flags.Bool("fake", false, "start an in-process fake mod")
	buffer := flags.Int("buffer", 1000, "events kept in the replay buffer")
	reconnect := flags.Duration("reconnect-delay", 2*time.Second, "reconnect delay")
	if err := flags.Parse(args); err != nil {
		return nil, protocol.NewError(protocol.CodeUsage, err.Error())
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, err.Error())
	}
	logPath := filepath.Join(a.home, "daemon.log")
	if err := os.MkdirAll(a.home, config.DirMode); err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, "cannot create "+a.home+": "+err.Error())
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, config.FileMode)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, "cannot open "+logPath+": "+err.Error())
	}
	defer logFile.Close()
	childArgs := []string{"--home", a.home, "daemon", "run", "--buffer", strconv.Itoa(*buffer),
		"--reconnect-delay", reconnect.String()}
	if *apiAddress != "" {
		childArgs = append(childArgs, "--api-address", *apiAddress)
	}
	if *fake {
		childArgs = append(childArgs, "--fake")
	}
	command := exec.Command(executable, childArgs...)
	command.Stdout = logFile
	command.Stderr = logFile
	command.Stdin = nil
	command.SysProcAttr = detachAttributes()
	if err := command.Start(); err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, "cannot start the daemon: "+err.Error())
	}
	// Do not Wait(): the child is detached.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state, loadErr := config.LoadDaemonState(a.home)
		if loadErr == nil && state != nil {
			client, dialErr := ipc.Dial(context.Background(), state.Address, state.Token)
			if dialErr == nil {
				status, failure := client.Call(context.Background(), "status", nil)
				client.Close()
				if failure == nil {
					return map[string]any{"started": true, "log": logPath, "status": status}, nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, protocol.NewError(protocol.CodeConnectionFailed,
		fmt.Sprintf("the daemon did not become ready; see %s", logPath))
}

func (a *app) daemonStatus(ctx context.Context) (any, *protocol.Error) {
	state, err := config.LoadDaemonState(a.home)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, err.Error())
	}
	if state == nil {
		return map[string]any{"running": false, "home": a.home}, nil
	}
	client, dialErr := ipc.Dial(ctx, state.Address, state.Token)
	if dialErr != nil {
		return map[string]any{"running": false, "staleState": true, "address": state.Address,
			"pid": state.PID, "error": dialErr.Error()}, nil
	}
	defer client.Close()
	status, failure := client.Call(ctx, "status", nil)
	if failure != nil {
		return nil, failure
	}
	return map[string]any{"running": true, "address": state.Address, "pid": state.PID,
		"startedAt": state.StartedAt, "version": state.Version, "status": status}, nil
}

// ------------------------------------------------------------------- targets

func (a *app) cmdTarget(args []string) (any, *protocol.Error) {
	if len(args) == 0 {
		return nil, protocol.NewError(protocol.CodeUsage,
			"target needs add|list|show|remove|use|reload")
	}
	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "add":
		return a.targetAdd(rest)
	case "list":
		return a.targetList()
	case "show":
		if len(rest) == 0 {
			return nil, protocol.NewError(protocol.CodeUsage, "target show needs a name")
		}
		return a.targetShow(rest[0])
	case "remove":
		if len(rest) == 0 {
			return nil, protocol.NewError(protocol.CodeUsage, "target remove needs a name")
		}
		return a.targetRemove(rest[0])
	case "use":
		if len(rest) == 0 {
			return nil, protocol.NewError(protocol.CodeUsage, "target use needs a name")
		}
		return a.targetUse(rest[0])
	case "reload":
		return a.daemonCall(context.Background(), "reload_targets", map[string]any{})
	default:
		return nil, protocol.NewError(protocol.CodeUsage, "unknown target command: "+subcommand)
	}
}

func (a *app) targetAdd(args []string) (any, *protocol.Error) {
	flags, values := flagSet("target add")
	transport := flags.String("transport", protocol.TransportRemote, "remote|legacy|fake")
	address := flags.String("address", "", "host:port")
	pin := flags.String("pin", "", "sha256:<hex> certificate pin")
	caFile := flags.String("ca", "", "PEM CA/certificate file")
	serverName := flags.String("server-name", "", "TLS server name when using --ca")
	tokenEnv := flags.String("token-env", "", "environment variable holding the token")
	tokenFile := flags.String("token-file", "", "file holding the token")
	tokenStdin := flags.Bool("token-stdin", false, "read the token from stdin")
	portFile := flags.String("port-file", "", "legacy port.txt")
	serverDir := flags.String("server-dir", "", "legacy game dir")
	vantage := flags.String("vantage", "", "legacy vantage: client|server")
	isDefault := flags.Bool("default", false, "make this the default target")
	force := flags.Bool("force", false, "replace an existing target with the same name")
	for _, name := range []string{"transport", "address", "pin", "ca", "server-name", "token-env",
		"token-file", "port-file", "server-dir", "vantage"} {
		values[name] = true
	}
	if err := flags.Parse(reorderInterspersed(args, values)); err != nil {
		return nil, protocol.NewError(protocol.CodeUsage, err.Error())
	}
	if flags.NArg() != 1 {
		return nil, protocol.NewError(protocol.CodeUsage, "target add needs exactly one name")
	}
	name := flags.Arg(0)
	if name == "" || strings.ContainsAny(name, "/\\ \t") {
		return nil, protocol.NewError(protocol.CodeUsage, "target name must be a simple word")
	}
	document, err := config.LoadTargets(a.home)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeBadRequest, err.Error())
	}
	if _, exists := document.Targets[name]; exists && !*force {
		return nil, protocol.NewError(protocol.CodeBadRequest,
			"target "+name+" already exists; pass --force to replace it")
	}
	target := &config.Target{
		Name:       name,
		Transport:  *transport,
		Address:    *address,
		Pin:        *pin,
		CAFile:     *caFile,
		ServerName: *serverName,
		TokenEnv:   *tokenEnv,
		TokenFile:  *tokenFile,
		PortFile:   *portFile,
		ServerDir:  *serverDir,
		Vantage:    *vantage,
	}
	if err := target.Validate(); err != nil {
		return nil, protocol.NewError(protocol.CodeBadRequest, err.Error())
	}
	if *tokenStdin {
		secret, readErr := io.ReadAll(io.LimitReader(os.Stdin, 1024*1024))
		if readErr != nil {
			return nil, protocol.NewError(protocol.CodeBadRequest, "cannot read the token from stdin: "+readErr.Error())
		}
		value := strings.TrimSpace(string(secret))
		if value == "" {
			return nil, protocol.NewError(protocol.CodeBadRequest, "the token from stdin is empty")
		}
		secrets, loadErr := config.LoadSecrets(a.home)
		if loadErr != nil {
			return nil, protocol.NewError(protocol.CodeBadRequest, loadErr.Error())
		}
		secrets.Tokens[name] = value
		if saveErr := config.SaveSecrets(a.home, secrets); saveErr != nil {
			return nil, protocol.NewError(protocol.CodeInternal, saveErr.Error())
		}
	}
	document.Targets[name] = target
	if *isDefault || document.Default == "" {
		document.Default = name
	}
	if err := config.SaveTargets(a.home, document); err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, err.Error())
	}
	result := map[string]any{"added": name, "transport": target.Transport, "default": document.Default}
	if a.daemonRunning() {
		if _, failure := a.daemonCall(context.Background(), "reload_targets", map[string]any{}); failure != nil {
			result["reloadError"] = failure.Message
		} else {
			result["daemonReloaded"] = true
		}
	}
	return result, nil
}

func (a *app) targetList() (any, *protocol.Error) {
	document, err := config.LoadTargets(a.home)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeBadRequest, err.Error())
	}
	targets := make([]map[string]any, 0, len(document.Targets))
	for _, name := range config.TargetNames(document) {
		target := document.Targets[name]
		entry := map[string]any{
			"name":      name,
			"transport": target.Transport,
			"address":   target.Address,
			"default":   name == document.Default,
		}
		if target.Pin != "" {
			entry["pin"] = target.Pin
		}
		if target.CAFile != "" {
			entry["caFile"] = target.CAFile
		}
		_, source, tokenErr := config.Token(a.home, target)
		entry["credential"] = source
		if tokenErr != nil {
			entry["credentialError"] = tokenErr.Error()
		}
		targets = append(targets, entry)
	}
	return map[string]any{"targets": targets, "default": document.Default,
		"home": a.home, "count": len(targets)}, nil
}

func (a *app) targetShow(name string) (any, *protocol.Error) {
	document, err := config.LoadTargets(a.home)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeBadRequest, err.Error())
	}
	target, ok := document.Targets[name]
	if !ok {
		return nil, protocol.NewError(protocol.CodeTargetUnknown, "no target named "+name)
	}
	entry := map[string]any{"name": target.Name, "transport": target.Transport,
		"address": target.Address, "default": name == document.Default}
	_, source, tokenErr := config.Token(a.home, target)
	entry["credential"] = source
	if tokenErr != nil {
		entry["credentialError"] = tokenErr.Error()
	}
	if a.daemonRunning() {
		status, failure := a.daemonCall(context.Background(), "targets", map[string]any{})
		if failure == nil {
			entry["daemonView"] = status
		}
	}
	return entry, nil
}

func (a *app) targetRemove(name string) (any, *protocol.Error) {
	document, err := config.LoadTargets(a.home)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeBadRequest, err.Error())
	}
	if _, ok := document.Targets[name]; !ok {
		return nil, protocol.NewError(protocol.CodeTargetUnknown, "no target named "+name)
	}
	delete(document.Targets, name)
	if document.Default == name {
		document.Default = ""
		for _, candidate := range config.TargetNames(document) {
			document.Default = candidate
			break
		}
	}
	if err := config.SaveTargets(a.home, document); err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, err.Error())
	}
	secrets, _ := config.LoadSecrets(a.home)
	if secrets != nil {
		delete(secrets.Tokens, name)
		_ = config.SaveSecrets(a.home, secrets)
	}
	result := map[string]any{"removed": name}
	if a.daemonRunning() {
		if _, failure := a.daemonCall(context.Background(), "reload_targets", map[string]any{}); failure != nil {
			result["reloadError"] = failure.Message
		} else {
			result["daemonReloaded"] = true
		}
	}
	return result, nil
}

func (a *app) targetUse(name string) (any, *protocol.Error) {
	document, err := config.LoadTargets(a.home)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeBadRequest, err.Error())
	}
	if _, ok := document.Targets[name]; !ok {
		return nil, protocol.NewError(protocol.CodeTargetUnknown, "no target named "+name)
	}
	document.Default = name
	if err := config.SaveTargets(a.home, document); err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, err.Error())
	}
	return map[string]any{"default": name}, nil
}

func (a *app) daemonRunning() bool {
	state, err := config.LoadDaemonState(a.home)
	if err != nil || state == nil {
		return false
	}
	client, err := ipc.Dial(context.Background(), state.Address, state.Token)
	if err != nil {
		return false
	}
	client.Close()
	return true
}

// ---------------------------------------------------------------- doctor

func (a *app) cmdDoctor(ctx context.Context, args []string) (any, *protocol.Error) {
	checks := make([]map[string]any, 0, 8)
	failed := false
	add := func(name string, ok bool, detail string) {
		checks = append(checks, map[string]any{"check": name, "ok": ok, "detail": detail})
		if !ok {
			failed = true
		}
	}
	add("version", true, "mc-agent "+version.Version+" control protocol "+strconv.Itoa(protocol.ControlProtocolVersion))
	if err := os.MkdirAll(a.home, config.DirMode); err != nil {
		add("home", false, "cannot create "+a.home+": "+err.Error())
	} else {
		add("home", true, a.home)
	}
	document, err := config.LoadTargets(a.home)
	if err != nil {
		add("targets", false, err.Error())
	} else {
		add("targets", len(document.Targets) > 0,
			fmt.Sprintf("%d configured: %s", len(document.Targets), strings.Join(config.TargetNames(document), ", ")))
		for _, name := range config.TargetNames(document) {
			target := document.Targets[name]
			_, source, tokenErr := config.Token(a.home, target)
			if target.Transport == protocol.TransportLegacy {
				add("credential:"+name, true, "not needed for the legacy loopback adapter")
			} else if tokenErr != nil {
				add("credential:"+name, false, tokenErr.Error())
			} else {
				add("credential:"+name, true, "source "+source)
			}
		}
	}
	running := a.daemonRunning()
	add("daemon", true, map[bool]string{true: "running", false: "not running (optional; direct doctor checks run below)"}[running])
	if running {
		status, failure := a.daemonCall(ctx, "status", map[string]any{})
		if failure != nil {
			add("daemon-status", false, failure.Message)
		} else {
			add("daemon-status", true, "ok")
		}
		_ = status
		for _, name := range config.TargetNames(document) {
			result, failure := a.daemonOp(ctx, "capabilities", map[string]any{"target": name})
			if failure != nil {
				add("target:"+name, false, failure.Message)
				continue
			}
			add("target:"+name, true, "connected; capabilities: "+capabilitySummary(result))
		}
	} else if document != nil {
		// Direct TLS verification without a daemon, using a short deadline.
		for _, name := range config.TargetNames(document) {
			target := document.Targets[name]
			if target.Transport == protocol.TransportLegacy {
				add("target:"+name, true, "legacy adapter: start the daemon to verify the loopback port")
				continue
			}
			token, _, tokenErr := config.Token(a.home, target)
			if tokenErr != nil {
				add("target:"+name, false, tokenErr.Error())
				continue
			}
			checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			adapter, dialErr := session.DialRemote(checkCtx, target, token, 0, "")
			cancel()
			if dialErr != nil {
				var protocolErr *protocol.Error
				code := protocol.CodeConnectionFailed
				if errors.As(dialErr, &protocolErr) {
					code = protocolErr.Code
				}
				add("target:"+name, false, code+": "+dialErr.Error())
				continue
			}
			state := adapter.State()
			adapter.Close()
			add("target:"+name, true, fmt.Sprintf("TLS ok; instance=%s run=%s protocol-ok",
				state.InstanceID, state.RunID))
		}
	}
	if a.webhookConfigured() {
		add("webhook", true, "configured; see `mc-agent daemon status` for delivery stats")
	}
	result := map[string]any{"ok": !failed, "checks": checks, "home": a.home}
	if failed {
		a.exitCode = 1
	}
	return result, nil
}

func capabilitySummary(result any) string {
	object, _ := result.(map[string]any)
	if capabilities, ok := object["capabilities"].([]any); ok {
		parts := make([]string, 0, len(capabilities))
		for _, entry := range capabilities {
			parts = append(parts, fmt.Sprint(entry))
		}
		return strings.Join(parts, ",")
	}
	return "unknown"
}

func (a *app) webhookConfigured() bool {
	return os.Getenv(webhook.EnvURL) != "" || os.Getenv(webhook.EnvConfig) != ""
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
