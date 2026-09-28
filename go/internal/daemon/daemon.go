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
	fake      *fakemod.Server

	stopCh   chan struct{}
	stopOnce sync.Once
}

type targetSession struct {
	daemon *Daemon
	target *config.Target

	mu          sync.Mutex
	adapter     session.Adapter
	state       session.State
	lastSeq     int64
	lastRunID   string
	generation  int64
	everConnect bool
	stopCh      chan struct{}
	stopped     bool
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
	_ = daemon.unknown.Load()

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
			stopCh: make(chan struct{}),
		}
		d.sessions[name] = targetSession
		go targetSession.run()
	}
}

func (ts *targetSession) run() {
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
		var adapter session.Adapter
		var err error
		switch ts.target.Transport {
		case protocol.TransportRemote, protocol.TransportFake:
			adapter, err = session.DialRemote(context.Background(), ts.target, token, ts.lastSequence())
		case protocol.TransportLegacy:
			adapter, err = session.DialLegacy(context.Background(), ts.target)
		default:
			err = &protocol.Error{Code: protocol.CodeUnsupportedTransport,
				Message: "unknown transport " + ts.target.Transport}
		}
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
		restarted := ts.everConnect && ts.lastRunID != "" && state.RunID != "" && ts.lastRunID != state.RunID
		ts.lastRunID = state.RunID
		ts.everConnect = true
		daemon.ingest(ts.target.Name, map[string]any{
			"type":       "bridge_connected",
			"text":       fmt.Sprintf("connected to %s (%s) at %s", state.Instance, state.Transport, state.Endpoint),
			"transport":  state.Transport,
			"instanceId": state.InstanceID,
			"runId":      state.RunID,
			"sessionId":  state.SessionID,
		})
		if restarted {
			daemon.ingest(ts.target.Name, map[string]any{
				"type":       "game_restarted",
				"text":       "the game run changed; no event replay crosses a restart",
				"oldRunId":   ts.lastRunID,
				"newRunId":   state.RunID,
				"lostReplay": true,
			})
		}
		if state.ReplayLost {
			daemon.ingest(ts.target.Name, map[string]any{
				"type": "event_gap",
				"text": "the server could not replay every event after this reconnect",
				"lost": true,
			})
		}
		daemon.reconcileUnknown(ts)

		for {
			select {
			case event, ok := <-adapter.Events():
				if !ok {
					goto disconnected
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
	ts.mu.Unlock()
}

func (ts *targetSession) stop() {
	ts.mu.Lock()
	if !ts.stopped {
		ts.stopped = true
		close(ts.stopCh)
	}
	if ts.adapter != nil {
		ts.adapter.Close()
	}
	ts.mu.Unlock()
}

// call routes one operation to a target session.
func (d *Daemon) call(ctx context.Context, targetName, operation string, params map[string]any) (any, *protocol.Error) {
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
	result, failure := adapter.Call(ctx, operation, params)
	if failure != nil {
		if write && failure.ResultUnknown && failure.RequestID != "" {
			d.unknown.Record(target.Name, operation, failure.RequestID, failure.Message)
		}
		return nil, failure
	}
	return result, nil
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
	for _, entry := range entries {
		targetSession := ts
		targetSession.mu.Lock()
		adapter := targetSession.adapter
		targetSession.mu.Unlock()
		if adapter == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		result, failure := adapter.Call(ctx, "request_status", map[string]any{"requestId": entry.RequestID})
		cancel()
		if failure != nil {
			d.logger("cannot reconcile %s request %s: %s", entry.Operation, entry.RequestID, failure.Message)
			continue
		}
		object, _ := result.(map[string]any)
		state, _ := object["state"].(string)
		switch state {
		case "completed":
			d.unknown.Resolve(entry.Target, entry.RequestID)
			d.ingest(entry.Target, map[string]any{
				"type":      "request_resolved",
				"text":      "an earlier write completed after reconnecting",
				"operation": entry.Operation,
				"requestId": entry.RequestID,
				"state":     "completed",
				"result":    object["result"],
			})
		case "failed":
			d.unknown.Resolve(entry.Target, entry.RequestID)
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
			d.unknown.MarkUnresolved(entry.Target, entry.RequestID,
				"the server has no record of this request in the current run")
			d.ingest(entry.Target, map[string]any{
				"type":      "request_resolved",
				"text":      "an earlier write has no server-side record after a restart; do not retry blindly",
				"operation": entry.Operation,
				"requestId": entry.RequestID,
				"state":     "unresolved",
			})
		}
	}
}

// ------------------------------------------------------------------- events

// ingest assigns the daemon-local stream identity and fans the event out.
func (d *Daemon) ingest(target string, payload map[string]any) {
	eventType, _ := payload["type"].(string)
	category := protocol.Categorize(eventType)
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

	d.eventsMu.Lock()
	d.eventRing = append(d.eventRing, event)
	if len(d.eventRing) > d.bufferSize {
		d.eventRing = d.eventRing[len(d.eventRing)-d.bufferSize:]
	}
	d.eventsMu.Unlock()

	if d.ipcServer != nil {
		d.ipcServer.Broadcast(category, event)
	}
	if d.webhook != nil {
		d.webhook.Enqueue(event)
	}
}

// recentEvents mirrors the Python daemon's events method.
func (d *Daemon) recentEvents(since int64, limit int, category string) map[string]any {
	d.eventsMu.Lock()
	ring := append([]map[string]any(nil), d.eventRing...)
	d.eventsMu.Unlock()
	var oldest any
	if len(ring) > 0 {
		oldest = ring[0]["seq"]
	}
	dropped := false
	if since > 0 && oldest != nil {
		if value, ok := oldest.(int64); ok && value > since+1 {
			dropped = true
		}
	}
	events := make([]map[string]any, 0, len(ring))
	for _, event := range ring {
		if category != "" {
			if value, _ := event["category"].(string); value != category {
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
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	next := d.eventSeq.Load() + 1
	return map[string]any{"events": events, "next": next, "dropped": dropped,
		"streamId": d.streamID}
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
