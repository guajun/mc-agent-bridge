// Package daemon is the long-lived Go process that owns every game connection.
//
// The CLI and any harness talk to it over the loopback IPC channel; only the
// daemon opens sockets to the mod. The daemon never runs a model and never
// implements harness session management.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/fakemod"
	"github.com/guajun/mc-agent-bridge/internal/ipc"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
	"github.com/guajun/mc-agent-bridge/internal/session"
	"github.com/guajun/mc-agent-bridge/internal/version"
	"github.com/guajun/mc-agent-bridge/internal/webhook"
)

// Options configures one daemon run.
type Options struct {
	Home           string
	APIAddress     string // loopback host:port; empty = 127.0.0.1:0
	BufferSize     int
	ReconnectDelay time.Duration
	Webhook        *webhook.Config
	Fake           bool // start an in-process fake mod target for smoke tests
	Logger         func(format string, args ...any)
}

// Daemon is the running process state.
type Daemon struct {
	home           string
	apiAddress     string
	bufferSize     int
	reconnectDelay time.Duration
	logger         func(format string, args ...any)

	targetsMu sync.RWMutex
	targets   *config.Targets

	sessionsMu sync.RWMutex
	sessions   map[string]*targetSession

	eventsMu  sync.Mutex
	eventRing []map[string]any

	streamID  string
	eventSeq  atomic.Int64
	startedAt time.Time

	ipcServer *ipc.Server
	ipcToken  string
	webhook   *webhook.Forwarder
	unknown   *unknownWrites
	cursors   *cursorStore
	fake      *fakemod.Server

	stopCh   chan struct{}
	stopOnce sync.Once
}

type targetSession struct {
	daemon *Daemon
	target *config.Target

	mu                 sync.Mutex
	adapter            session.Adapter
	state              session.State
	lastSeq            int64
	eventsSincePersist int
	generation         int64
	everConnect        bool
	stopCh             chan struct{}
	finished           chan struct{}
	stopped            bool
}

// Run starts the daemon and blocks until the context ends or `stop` is called
// over the local IPC.
func Run(ctx context.Context, options Options) error {
	if options.Logger == nil {
		options.Logger = func(string, ...any) {}
	}
	if options.BufferSize <= 0 {
		options.BufferSize = 1000
	}
	if options.ReconnectDelay <= 0 {
		options.ReconnectDelay = 2 * time.Second
	}
	home := options.Home
	if home == "" {
		resolved, err := config.Home()
		if err != nil {
			return err
		}
		home = resolved
	}
	if err := os.MkdirAll(home, config.DirMode); err != nil {
		return err
	}
	targets, err := config.LoadTargets(home)
	if err != nil {
		return err
	}

	daemon := &Daemon{
		home:           home,
		apiAddress:     options.APIAddress,
		bufferSize:     options.BufferSize,
		reconnectDelay: options.ReconnectDelay,
		logger:         options.Logger,
		targets:        targets,
		sessions:       map[string]*targetSession{},
		streamID:       randomHex(16),
		startedAt:      time.Now(),
		stopCh:         make(chan struct{}),
	}

	if options.Fake {
		fakeToken := randomHex(24)
		fake, err := fakemod.Start(fakemod.Options{Token: fakeToken})
		if err != nil {
			return fmt.Errorf("cannot start the fake mod: %w", err)
		}
		daemon.fake = fake
		fakeTarget := &config.Target{
			Name:      "fake",
			Transport: protocol.TransportRemote,
			Address:   fake.Address(),
			Pin:       fake.Pin(),
		}
		targets.Targets["fake"] = fakeTarget
		if len(targets.Targets) == 1 {
			targets.Default = "fake"
		}
		// Persist the generated development credential in the private store so
		// the fake target behaves exactly like a configured remote target.
		secrets, _ := config.LoadSecrets(home)
		if secrets != nil {
			secrets.Tokens["fake"] = fakeToken
			_ = config.SaveSecrets(home, secrets)
		}
		options.Logger("fake mod listening on %s (%s)", fake.Address(), fake.Pin())
	}
	defer func() {
		if daemon.fake != nil {
			daemon.fake.Close()
		}
	}()

	if err := daemon.checkNotRunning(); err != nil {
		return err
	}
	daemon.unknown = newUnknownWrites(home)
	if message, err := daemon.unknown.Load(); err != nil {
		return fmt.Errorf("cannot read the unknown-write ledger: %w", err)
	} else if message != "" {
		options.Logger("unknown-write ledger: %s", message)
	}
	daemon.cursors = newCursorStore(home)
	daemon.cursors.Load()

	daemon.ipcToken = randomHex(32)
	ipcServer, err := ipc.Start(options.APIAddress, daemon.ipcToken, daemon.dispatch)
	if err != nil {
		return fmt.Errorf("cannot bind the local API: %w", err)
	}
	daemon.ipcServer = ipcServer
	stateFile := &config.DaemonState{
		PID:       os.Getpid(),
		Address:   ipcServer.Address(),
		Token:     daemon.ipcToken,
		Version:   version.Version,
		StartedAt: daemon.startedAt,
		Targets:   config.TargetNames(targets),
	}
	if err := config.SaveDaemonState(home, stateFile); err != nil {
		ipcServer.Stop()
		return fmt.Errorf("cannot write the daemon state file: %w", err)
	}
	options.Logger("local API on %s streaming %d target(s)", ipcServer.Address(), len(targets.Targets))
	daemon.logger("state file: %s (token in %s)", filepath.Join(home, config.DaemonFile),
		config.DaemonFile)

	if options.Webhook != nil {
		forwarder, err := webhook.NewForwarder(options.Webhook, func(format string, args ...any) {
			options.Logger(format, args...)
		})
		if err != nil {
			ipcServer.Stop()
			config.RemoveDaemonState(home)
			return fmt.Errorf("webhook configuration is invalid: %w", err)
		}
		daemon.webhook = forwarder
		go forwarder.Run(context.Background())
		daemon.logger("webhook forwarding to %s (events: %v)", webhook.RedactURL(options.Webhook.URL),
			options.Webhook.Events)
	}

	daemon.startAllSessions()
	daemon.ingest("daemon", map[string]any{"type": "daemon_started",
		"text": "daemon " + version.Version + " started, api=" + ipcServer.Address()})

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-daemon.stopCh:
		}
		close(done)
	}()
	<-done

	daemon.shutdown()
	return nil
}

// Stop requests a graceful shutdown (used by the CLI's `daemon stop`).
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() { close(d.stopCh) })
}

func (d *Daemon) shutdown() {
	d.logger("shutting down")
	d.sessionsMu.Lock()
	for _, targetSession := range d.sessions {
		targetSession.stop()
	}
	d.sessionsMu.Unlock()
	if err := d.cursors.Flush(); err != nil {
		d.logger("cannot persist event cursors: %v", err)
	}
	if d.webhook != nil {
		d.webhook.Stop()
	}
	if d.ipcServer != nil {
		d.ipcServer.Stop()
	}
	if d.fake != nil {
		d.fake.Close()
	}
	_ = config.RemoveDaemonState(d.home)
}

func (d *Daemon) checkNotRunning() error {
	state, err := config.LoadDaemonState(d.home)
	if err != nil || state == nil {
		return nil
	}
	client, err := ipc.Dial(context.Background(), state.Address, state.Token)
	if err == nil {
		client.Close()
		return &protocol.Error{Code: protocol.CodeDaemonAlreadyRunning,
			Message: fmt.Sprintf("a daemon is already running (pid %d, api %s)", state.PID, state.Address)}
	}
	// A stale state file from a crashed daemon is replaced.
	_ = config.RemoveDaemonState(d.home)
	return nil
}

// ----------------------------------------------------------------- sessions

func (d *Daemon) startAllSessions() {
	d.targetsMu.RLock()
	targets := make(map[string]*config.Target, len(d.targets.Targets))
	for name, target := range d.targets.Targets {
		targets[name] = target
	}
	d.targetsMu.RUnlock()
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	for name, target := range targets {
		if _, exists := d.sessions[name]; exists {
			continue
		}
		targetSession := &targetSession{
			daemon: d,
			target: target,
			state: session.State{
				Target:    name,
				Transport: target.Transport,
			},
			stopCh:   make(chan struct{}),
			finished: make(chan struct{}),
		}
		d.sessions[name] = targetSession
		go targetSession.run()
	}
}

func (ts *targetSession) run() {
	defer close(ts.finished)
	daemon := ts.daemon
	for {
		select {
		case <-ts.stopCh:
			return
		case <-daemon.stopCh:
			return
		default:
		}
		token, _, tokenErr := config.Token(daemon.home, ts.target)
		if tokenErr != nil && ts.target.Transport != protocol.TransportLegacy {
			ts.setFailure(protocol.CodeUnauthorized, tokenErr.Error())
			daemon.logger("target %s: %v", ts.target.Name, tokenErr)
			if !ts.sleep(daemon.reconnectDelay) {
				return
			}
			continue
		}
		previous := daemon.cursors.Get(ts.target.Name)
		var adapter session.Adapter
		var err error
		dialContext, cancelDial := context.WithTimeout(context.Background(), 15*time.Second)
		switch ts.target.Transport {
		case protocol.TransportRemote, protocol.TransportFake:
			// The cursor is only offered together with the run it belongs to;
			// a new run makes the server treat the client as fresh instead of
			// silently skipping the new run's events.
			adapter, err = session.DialRemote(dialContext, ts.target, token, previous.LastSeq, previous.RunID)
		case protocol.TransportLegacy:
			adapter, err = session.DialLegacy(dialContext, ts.target)
		default:
			err = &protocol.Error{Code: protocol.CodeUnsupportedTransport,
				Message: "unknown transport " + ts.target.Transport}
		}
		cancelDial()
		if err != nil {
			code := protocol.CodeConnectionFailed
			var protocolErr *protocol.Error
			if errors.As(err, &protocolErr) {
				code = protocolErr.Code
			}
			ts.setFailure(code, err.Error())
			daemon.logger("target %s: %v; retrying in %s", ts.target.Name, err, daemon.reconnectDelay)
			if !ts.sleep(daemon.reconnectDelay) {
				return
			}
			continue
		}
		ts.attach(adapter)
		state := adapter.State()
		sameRun := previous.RunID != "" && state.RunID != "" && previous.RunID == state.RunID
		restarted := ts.everConnect && previous.RunID != "" && state.RunID != "" && previous.RunID != state.RunID
		instanceChanged := ts.everConnect && previous.InstanceID != "" && state.InstanceID != "" && previous.InstanceID != state.InstanceID
		ts.mu.Lock()
		if !sameRun {
			// A different run (or no cursor) starts the sequence at zero.
			ts.lastSeq = 0
		}
		ts.eventsSincePersist = 0
		ts.mu.Unlock()
		daemon.cursors.Set(ts.target.Name, cursor{
			InstanceID: state.InstanceID,
			RunID:      state.RunID,
			LastSeq:    ts.lastSequence(),
		})
		if err := daemon.cursors.Flush(); err != nil {
			daemon.logger("target %s: cannot persist the event cursor: %v", ts.target.Name, err)
		}
		ts.everConnect = true
		daemon.ingest(ts.target.Name, map[string]any{
			"type":       "bridge_connected",
			"text":       fmt.Sprintf("connected to %s (%s) at %s", state.Instance, state.Transport, state.Endpoint),
			"transport":  state.Transport,
			"instanceId": state.InstanceID,
			"runId":      state.RunID,
			"sessionId":  state.SessionID,
		})
		if restarted || instanceChanged {
			daemon.ingest(ts.target.Name, map[string]any{
				"type":          "game_restarted",
				"text":          "the game run changed; no event replay crosses a restart",
				"oldRunId":      previous.RunID,
				"newRunId":      state.RunID,
				"oldInstanceId": previous.InstanceID,
				"newInstanceId": state.InstanceID,
				"lostReplay":    true,
			})
		}
		if state.ReplayLost {
			daemon.ingest(ts.target.Name, map[string]any{
				"type":  "event_gap",
				"text":  "the server could not replay every event after this reconnect",
				"lost":  true,
				"runId": state.RunID,
				"from":  ts.lastSequence() + 1,
			})
		}
		daemon.reconcileUnknown(ts)

		for {
			select {
			case event, ok := <-adapter.Events():
				if !ok {
					goto disconnected
				}
				last := ts.lastSequence()
				if last > 0 && event.Seq > last+1 && event.RunID != "" && event.RunID == state.RunID {
					daemon.ingest(ts.target.Name, map[string]any{
						"type":  "event_gap",
						"text":  "the event sequence jumped; events between are not available",
						"lost":  true,
						"runId": event.RunID,
						"from":  last + 1,
						"to":    event.Seq - 1,
					})
				}
				ts.noteRemoteSeq(event.Seq)
				daemon.ingest(ts.target.Name, event.Payload)
			case <-adapter.Done():
				goto disconnected
			case <-ts.stopCh:
				adapter.Close()
				return
			case <-daemon.stopCh:
				adapter.Close()
				return
			}
		}
	disconnected:
		err = adapter.Err()
		adapter.Close()
		ts.detach()
		ts.persistCursor()
		message := "connection closed"
		code := protocol.CodeConnectionLost
		if err != nil {
			message = err.Error()
			var protocolErr *protocol.Error
			if errors.As(err, &protocolErr) {
				code = protocolErr.Code
			}
		}
		ts.setFailure(code, message)
		daemon.ingest(ts.target.Name, map[string]any{
			"type":  "bridge_disconnected",
			"text":  "mod connection closed: " + message,
			"error": message,
		})
		daemon.logger("target %s: %s; reconnecting in %s", ts.target.Name, message, daemon.reconnectDelay)
		if !ts.sleep(daemon.reconnectDelay) {
			return
		}
	}
}

func (ts *targetSession) sleep(duration time.Duration) bool {
	select {
	case <-time.After(duration):
		return true
	case <-ts.stopCh:
		return false
	case <-ts.daemon.stopCh:
		return false
	}
}

func (ts *targetSession) attach(adapter session.Adapter) {
	ts.mu.Lock()
	ts.adapter = adapter
	ts.state = adapter.State()
	ts.mu.Unlock()
}

func (ts *targetSession) detach() {
	ts.mu.Lock()
	ts.adapter = nil
	ts.state.Connected = false
	ts.mu.Unlock()
}

func (ts *targetSession) setFailure(code, message string) {
	ts.mu.Lock()
	ts.state.Connected = false
	ts.state.LastError = message
	ts.state.LastErrorCode = code
	ts.mu.Unlock()
}

func (ts *targetSession) lastSequence() int64 {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.lastSeq
}

func (ts *targetSession) noteRemoteSeq(seq int64) {
	ts.mu.Lock()
	if seq > ts.lastSeq {
		ts.lastSeq = seq
	}
	ts.state.LastSeq = ts.lastSeq
	ts.eventsSincePersist++
	persist := ts.eventsSincePersist >= 64
	snapshot := cursor{InstanceID: ts.state.InstanceID, RunID: ts.state.RunID, LastSeq: ts.lastSeq}
	ts.mu.Unlock()
	if persist {
		ts.daemon.cursors.Set(ts.target.Name, snapshot)
		if err := ts.daemon.cursors.Flush(); err != nil {
			ts.daemon.logger("target %s: cannot persist the event cursor: %v", ts.target.Name, err)
		}
	}
}

// persistCursor flushes the current run cursor after a disconnect.
func (ts *targetSession) persistCursor() {
	ts.mu.Lock()
	snapshot := cursor{InstanceID: ts.state.InstanceID, RunID: ts.state.RunID, LastSeq: ts.lastSeq}
	ts.mu.Unlock()
	ts.daemon.cursors.Set(ts.target.Name, snapshot)
	if err := ts.daemon.cursors.Flush(); err != nil {
		ts.daemon.logger("target %s: cannot persist the event cursor: %v", ts.target.Name, err)
	}
}

func (ts *targetSession) stop() {
	ts.mu.Lock()
	if !ts.stopped {
		ts.stopped = true
		close(ts.stopCh)
	}
	adapter := ts.adapter
	ts.mu.Unlock()
	// Close outside the lock: session Close may wait for a pump that needs
	// this lock to record the terminal state. Then join the loop so a daemon
	// shutdown (or target reload) cannot leave a goroutine still writing the
	// cursor/ledger after Run returns.
	if adapter != nil {
		adapter.Close()
	}
	select {
	case <-ts.finished:
	case <-time.After(5 * time.Second):
		ts.daemon.logger("target %s: session loop did not stop after close", ts.target.Name)
	}
}

// call routes one operation to a target session with a daemon-generated id.
func (d *Daemon) call(ctx context.Context, targetName, operation string, params map[string]any) (any, *protocol.Error) {
	return d.callWithID(ctx, targetName, operation, params, "")
}

// callWithID routes one operation. A caller-provided requestID (the CLI/IPC
// path) becomes the end-to-end write identity: the same value is persisted in
// the recovery ledger and sent to the mod, so a crash or a lost reply can be
// reconciled against exactly that write.
func (d *Daemon) callWithID(ctx context.Context, targetName, operation string, params map[string]any,
	requestID string) (any, *protocol.Error) {
	if params == nil {
		params = map[string]any{}
	}
	target, err := config.Resolve(d.targetsSnapshot(), targetName, "")
	if err != nil {
		var protocolErr *protocol.Error
		if errors.As(err, &protocolErr) {
			return nil, protocolErr
		}
		return nil, protocol.NewError(protocol.CodeTargetUnknown, err.Error())
	}
	d.sessionsMu.RLock()
	targetSession := d.sessions[target.Name]
	d.sessionsMu.RUnlock()
	if targetSession == nil {
		return nil, &protocol.Error{Code: protocol.CodeTargetUnknown,
			Message: "target " + target.Name + " is not being managed by this daemon"}
	}
	targetSession.mu.Lock()
	adapter := targetSession.adapter
	state := targetSession.state
	targetSession.mu.Unlock()
	if adapter == nil || !state.Connected {
		message := "target " + target.Name + " is not connected"
		if state.LastError != "" {
			message += ": " + state.LastError
		}
		code := protocol.CodeConnectionFailed
		if state.LastErrorCode != "" && state.LastErrorCode != protocol.CodeConnectionFailed {
			code = state.LastErrorCode
		}
		return nil, &protocol.Error{Code: code, Message: message, Retryable: true}
	}
	write := protocol.WriteOperation(operation)
	// Local convenience operations the transport does not need to see are
	// handled by the session adapter itself (status/capabilities/save).
	if !write {
		result, failure := adapter.Call(ctx, operation, params)
		if failure != nil {
			return nil, failure
		}
		return result, nil
	}
	if requestID != "" && len(requestID) > 128 {
		return nil, protocol.NewError(protocol.CodeBadRequest, "request id must be 1..128 characters")
	}

	// Persist the request identity before it can leave the process. A crash
	// after this point leaves a visible unknown write; a persistence failure
	// refuses to send rather than losing recovery data.
	token, _, tokenErr := config.Token(d.home, target)
	if tokenErr != nil && target.Transport != protocol.TransportLegacy {
		return nil, protocol.NewError(protocol.CodeUnauthorized, tokenErr.Error())
	}
	if requestID == "" {
		requestID = adapter.NextRequestID()
	}
	if requestID == "" {
		return nil, protocol.NewError(protocol.CodeInternal,
			"the session cannot allocate a request id; refusing to send a non-idempotent write")
	}
	existing, admitErr := d.unknown.Admit(target.Name, operation, requestID,
		state.InstanceID, state.RunID, credentialFingerprint(token))
	if admitErr != nil {
		return nil, protocol.NewError(protocol.CodeInternal,
			"refusing to send a non-idempotent write: "+admitErr.Error())
	}
	if existing != nil {
		details, _ := json.Marshal(existing)
		return nil, &protocol.Error{
			Code: protocol.CodeResultUnknown,
			Message: "request id " + requestID + " already has an unreconciled write for target " +
				target.Name + " (state " + existing.State + "); resolve it with request_status " +
				"instead of reusing the id",
			Retryable:     false,
			ResultUnknown: true,
			RequestID:     requestID,
			Operation:     operation,
			Target:        target.Name,
			Details:       details,
		}
	}
	result, failure := adapter.CallID(ctx, requestID, operation, params)
	if failure == nil {
		if err := d.unknown.Resolve(target.Name, requestID); err != nil {
			d.logger("cannot clear resolved request %s: %v", requestID, err)
		}
		return result, nil
	}
	if failure.RequestID == "" {
		failure.RequestID = requestID
	}
	if !failure.ResultUnknown {
		// The server answered definitively (forbidden, bad_request, ...);
		// the request is not pending anywhere.
		if err := d.unknown.Resolve(target.Name, requestID); err != nil {
			d.logger("cannot clear answered request %s: %v", requestID, err)
		}
	} else if err := d.unknown.NoteUnknown(target.Name, requestID, failure.Message); err != nil {
		d.logger("target %s: cannot update unknown request %s: %v", target.Name, requestID, err)
	}
	return nil, failure
}

func (d *Daemon) targetsSnapshot() *config.Targets {
	d.targetsMu.RLock()
	defer d.targetsMu.RUnlock()
	copy := &config.Targets{Version: d.targets.Version, Default: d.targets.Default,
		Targets: make(map[string]*config.Target, len(d.targets.Targets))}
	for name, target := range d.targets.Targets {
		copy.Targets[name] = target
	}
	return copy
}

// reconcileUnknown resolves writes whose outcome was unknown after reconnect.
func (d *Daemon) reconcileUnknown(ts *targetSession) {
	entries := d.unknown.ForTarget(ts.target.Name)
	if len(entries) == 0 {
		return
	}
	token, _, _ := config.Token(d.home, ts.target)
	fingerprint := credentialFingerprint(token)
	for _, entry := range entries {
		ts.mu.Lock()
		adapter := ts.adapter
		state := ts.state
		ts.mu.Unlock()
		if adapter == nil {
			return
		}
		// A request is only resolvable against the connection that could
		// have run it. A restarted game, a different instance or a rotated
		// credential is reported instead of asking the wrong ledger.
		if entry.RunID != "" && state.RunID != "" && entry.RunID != state.RunID {
			d.markUnresolved(entry, "the request belongs to run "+entry.RunID+
				"; the server is now run "+state.RunID)
			continue
		}
		if entry.InstanceID != "" && state.InstanceID != "" && entry.InstanceID != state.InstanceID {
			d.markUnresolved(entry, "the request belongs to instance "+entry.InstanceID+
				"; the server is now instance "+state.InstanceID)
			continue
		}
		if entry.CredentialHash != "" && fingerprint != "" && entry.CredentialHash != fingerprint {
			d.markUnresolved(entry, "the credential changed since the request was sent")
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		result, failure := adapter.Call(ctx, "request_status", map[string]any{"requestId": entry.RequestID})
		cancel()
		if failure != nil {
			if failure.Code == protocol.CodeCapabilityNotSupported || failure.Code == protocol.CodeNotFound {
				d.markUnresolved(entry, "this transport cannot query the server-side request ledger")
				continue
			}
			d.logger("cannot reconcile %s request %s: %s", entry.Operation, entry.RequestID, failure.Message)
			continue
		}
		object, _ := result.(map[string]any)
		stateName, _ := object["state"].(string)
		switch stateName {
		case "completed":
			if err := d.unknown.Resolve(entry.Target, entry.RequestID); err != nil {
				d.logger("cannot clear resolved request %s: %v", entry.RequestID, err)
			}
			d.ingest(entry.Target, map[string]any{
				"type":      "request_resolved",
				"text":      "an earlier write completed after reconnecting",
				"operation": entry.Operation,
				"requestId": entry.RequestID,
				"state":     "completed",
				"result":    object["result"],
			})
		case "failed":
			if err := d.unknown.Resolve(entry.Target, entry.RequestID); err != nil {
				d.logger("cannot clear failed request %s: %v", entry.RequestID, err)
			}
			d.ingest(entry.Target, map[string]any{
				"type":      "request_resolved",
				"text":      "an earlier write failed after reconnecting",
				"operation": entry.Operation,
				"requestId": entry.RequestID,
				"state":     "failed",
				"error":     object["error"],
			})
		case "pending":
			// Still running; try again on the next reconnect or status query.
		case "unknown":
			// The server has no record (for example a game restart). Keep it
			// visible instead of pretending it succeeded or failed.
			d.markUnresolved(entry, "the server has no record of this request in the current run")
		}
	}
}

// markUnresolved keeps a request visible and emits the machine-readable state.
func (d *Daemon) markUnresolved(entry UnknownWrite, message string) {
	if err := d.unknown.MarkUnresolved(entry.Target, entry.RequestID, message); err != nil {
		d.logger("cannot persist unresolved request %s: %v", entry.RequestID, err)
	}
	d.ingest(entry.Target, map[string]any{
		"type":      "request_resolved",
		"text":      message + "; do not retry blindly",
		"operation": entry.Operation,
		"requestId": entry.RequestID,
		"state":     "unresolved",
	})
}

// ------------------------------------------------------------------- events

// ingest assigns the daemon-local stream identity and fans the event out.
func (d *Daemon) ingest(target string, payload map[string]any) {
	eventType, _ := payload["type"].(string)
	category := protocol.Categorize(eventType)

	// Sequence assignment, the replay ring and the fan-out are one critical
	// section: two targets or goroutines can never deliver seq 2 before seq 1,
	// and a page boundary taken here can never advance past an unseen event.
	d.eventsMu.Lock()
	sequence := d.eventSeq.Add(1)
	event := make(map[string]any, len(payload)+6)
	for key, value := range payload {
		event[key] = value
	}
	event["seq"] = sequence
	event["streamId"] = d.streamID
	event["eventId"] = fmt.Sprintf("%s:%d", d.streamID, sequence)
	event["category"] = category
	event["receivedAt"] = time.Now().UnixMilli()
	event["target"] = target
	d.eventRing = append(d.eventRing, event)
	if len(d.eventRing) > d.bufferSize {
		d.eventRing = d.eventRing[len(d.eventRing)-d.bufferSize:]
	}
	if d.ipcServer != nil {
		d.ipcServer.Broadcast(category, event)
	}
	if d.webhook != nil {
		d.webhook.Enqueue(event)
	}
	d.eventsMu.Unlock()
}

// recentEvents mirrors the Python daemon's events method.
func (d *Daemon) recentEvents(since int64, limit int, category, target string, cursorStream string) map[string]any {
	d.eventsMu.Lock()
	// The boundary is read under the same lock that assigns sequences: an
	// empty page can never advance past an event that was not yet appended.
	lastSeq := d.eventSeq.Load()
	ring := append([]map[string]any(nil), d.eventRing...)
	d.eventsMu.Unlock()
	reset := cursorStream != "" && cursorStream != d.streamID
	if since > lastSeq {
		// A cursor ahead of this stream can only come from a previous daemon
		// run (or a corrupted value): report it instead of an empty non-gap
		// page.
		reset = true
	}
	if reset {
		since = 0
	}
	var oldest int64
	if len(ring) > 0 {
		if value, ok := ring[0]["seq"].(int64); ok {
			oldest = value
		}
	}
	// dropped means the ring no longer reaches the requested cursor.
	dropped := since > 0 && oldest > 0 && oldest > since+1
	events := make([]map[string]any, 0, len(ring))
	for _, event := range ring {
		if category != "" {
			if value, _ := event["category"].(string); value != category {
				continue
			}
		}
		if target != "" {
			if value, _ := event["target"].(string); value != target {
				continue
			}
		}
		if since > 0 {
			if value, ok := event["seq"].(int64); ok && value <= since {
				continue
			}
		}
		events = append(events, event)
	}
	// Cursor pagination: always return the oldest events after the cursor so
	// `next` is a usable continuation (the highest delivered sequence), and say
	// explicitly when the caller must ask again instead of silently dropping
	// the tail.
	truncated := false
	if limit > 0 && len(events) > limit {
		events = events[:limit]
		truncated = true
	}
	next := lastSeq
	if len(events) > 0 {
		if value, ok := events[len(events)-1]["seq"].(int64); ok {
			next = value
		}
	}
	return map[string]any{"events": events, "next": next, "dropped": dropped,
		"truncated": truncated, "streamId": d.streamID, "reset": reset,
		"lastSeq": lastSeq}
}

func (d *Daemon) status() map[string]any {
	targets := make([]map[string]any, 0, len(d.sessions))
	d.sessionsMu.RLock()
	for name, targetSession := range d.sessions {
		targetSession.mu.Lock()
		state := targetSession.state
		targetSession.mu.Unlock()
		entry := map[string]any{
			"name":       name,
			"transport":  state.Transport,
			"connected":  state.Connected,
			"endpoint":   state.Endpoint,
			"vantage":    state.Vantage,
			"instanceId": state.InstanceID,
			"runId":      state.RunID,
			"sessionId":  state.SessionID,
			"lastSeq":    state.LastSeq,
		}
		if state.LastError != "" {
			entry["lastError"] = state.LastError
			entry["lastErrorCode"] = state.LastErrorCode
		}
		if state.Capabilities != nil {
			sorted := append([]string(nil), state.Capabilities...)
			sort.Strings(sorted)
			entry["capabilities"] = sorted
		}
		targets = append(targets, entry)
	}
	d.sessionsMu.RUnlock()
	sort.Slice(targets, func(i, j int) bool {
		return targets[i]["name"].(string) < targets[j]["name"].(string)
	})
	result := map[string]any{
		"daemon": map[string]any{
			"version":       version.Version,
			"startedAt":     d.startedAt.UTC().Format(time.RFC3339),
			"api":           d.ipcServerAddress(),
			"clients":       d.ipcClientCount(),
			"streamId":      d.streamID,
			"eventSeq":      d.eventSeq.Load(),
			"home":          d.home,
			"unknownWrites": d.unknown.List(),
		},
		"targets": targets,
		"events":  map[string]any{"buffered": d.bufferSize, "streamId": d.streamID, "lastSeq": d.eventSeq.Load()},
	}
	if d.webhook != nil {
		result["webhook"] = d.webhook.Status()
	}
	return result
}

func (d *Daemon) ipcServerAddress() string {
	if d.ipcServer == nil {
		return ""
	}
	return d.ipcServer.Address()
}

func (d *Daemon) ipcClientCount() int {
	if d.ipcServer == nil {
		return 0
	}
	return d.ipcServer.ClientCount()
}

func randomHex(bytes int) string {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}
