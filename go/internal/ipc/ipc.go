// Package ipc is the local control channel between the CLI and the daemon.
//
// The listener is loopback-only and every request must carry the daemon's IPC
// token, which lives in the user's private state directory (0600). A wrong
// token closes the connection; a missing token is refused. This is the
// "explicit access boundary" for local calls, not an open local port.
package ipc

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

// MaxLineBytes matches the Python local API budget.
const MaxLineBytes = 16 * 1024 * 1024

// IdleTimeout closes clients that go quiet for too long.
const IdleTimeout = 10 * time.Minute

// Handler executes one authorized method.
type Handler func(ctx context.Context, method string, params map[string]any) (any, *protocol.Error)

// Request is one client frame.
type Request struct {
	ID     string         `json:"id,omitempty"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
	Auth   string         `json:"auth,omitempty"`
	// RequestID is the end-to-end id forwarded to the daemon so a write keeps
	// one stable identity from the CLI through the daemon to the mod.
	RequestID string `json:"requestId,omitempty"`
}

// Event is one pushed event frame.
type Event struct {
	Type  string         `json:"type"`
	Event string         `json:"event"`
	Data  map[string]any `json:"data"`
}

// Server serves the local JSON-lines API.
type Server struct {
	listener net.Listener
	token    string
	handler  Handler

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
}

// Start binds the loopback listener. An empty address uses 127.0.0.1 with an
// ephemeral port; the caller reads Address() afterwards.
func Start(address, token string, handler Handler) (*Server, error) {
	if address == "" {
		address = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	server := &Server{listener: listener, token: token, handler: handler, clients: map[*client]struct{}{}}
	server.ctx, server.cancel = context.WithCancel(context.Background())
	go server.acceptLoop()
	return server, nil
}

// Address is the bound host:port.
func (s *Server) Address() string { return s.listener.Addr().String() }

// ClientCount reports currently connected local clients.
func (s *Server) ClientCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// Stop closes the listener and all clients.
func (s *Server) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	s.listener.Close()
	if s.cancel != nil {
		s.cancel()
	}
	for _, c := range clients {
		c.close()
	}
}

// Broadcast pushes an event to subscribed clients.
func (s *Server) Broadcast(event string, data map[string]any) {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		if c.subscribed(event) {
			c.send(Event{Type: "event", Event: event, Data: data})
		}
	}
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		c := &client{
			conn:       conn,
			reader:     bufio.NewReaderSize(conn, 64*1024),
			writer:     bufio.NewWriterSize(conn, 64*1024),
			server:     s,
			events:     map[string]bool{},
			outbound:   make(chan []byte, 256),
			dispatcher: make(chan struct{}, 32),
			done:       make(chan struct{}),
		}
		s.mu.Lock()
		s.clients[c] = struct{}{}
		s.mu.Unlock()
		go c.writeLoop()
		go c.readLoop()
	}
}

type client struct {
	conn       net.Conn
	reader     *bufio.Reader
	writer     *bufio.Writer
	server     *Server
	mu         sync.Mutex
	events     map[string]bool
	outbound   chan []byte
	dispatcher chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
}

func (c *client) readLoop() {
	defer c.close()
	for {
		c.conn.SetReadDeadline(time.Now().Add(IdleTimeout))
		line, err := readLine(c.reader, MaxLineBytes)
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var request Request
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			c.send(map[string]any{"type": "response", "id": "", "ok": false,
				"error": map[string]any{"code": protocol.CodeBadRequest, "message": "malformed request line"}})
			continue
		}
		if subtle.ConstantTimeCompare([]byte(request.Auth), []byte(c.server.token)) != 1 {
			c.send(map[string]any{"type": "response", "id": request.ID, "ok": false,
				"error": map[string]any{"code": protocol.CodeUnauthorized,
					"message": "the local IPC token is missing or wrong; read it from the daemon state file or run the " +
						"mc-agent CLI, which sends it automatically"}})
			// A wrong token closes the connection: no online guessing.
			return
		}
		switch request.Method {
		case "subscribe":
			c.subscribe(eventsFrom(request.Params))
			c.send(map[string]any{"type": "response", "id": request.ID, "ok": true,
				"result": map[string]any{"subscribed": c.eventList()}})
		case "unsubscribe":
			c.unsubscribe(eventsFrom(request.Params))
			c.send(map[string]any{"type": "response", "id": request.ID, "ok": true,
				"result": map[string]any{"subscribed": c.eventList()}})
		case "ping":
			c.send(map[string]any{"type": "response", "id": request.ID, "ok": true,
				"result": map[string]any{"pong": true}})
		default:
			select {
			case c.dispatcher <- struct{}{}:
			case <-c.done:
				return
			}
			go func(request Request) {
				defer func() { <-c.dispatcher }()
				ctx, cancel := context.WithTimeout(c.server.ctx, 5*time.Minute)
				defer cancel()
				result, failure := c.server.handler(ctx, request.Method, request.Params)
				if failure != nil {
					c.send(map[string]any{"type": "response", "id": request.ID, "ok": false,
						"error": failure})
					return
				}
				c.send(map[string]any{"type": "response", "id": request.ID, "ok": true, "result": result})
			}(request)
		}
	}
}

func eventsFrom(params map[string]any) []string {
	var values []any
	if params == nil {
		return nil
	}
	switch typed := params["events"].(type) {
	case []any:
		values = typed
	case string:
		return strings.Split(typed, ",")
	}
	events := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			events = append(events, strings.TrimSpace(text))
		}
	}
	return events
}

func (c *client) subscribe(events []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, event := range events {
		c.events[event] = true
	}
}

func (c *client) unsubscribe(events []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, event := range events {
		delete(c.events, event)
	}
}

func (c *client) subscribed(event string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events[event] || c.events["*"]
}

func (c *client) eventList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := make([]string, 0, len(c.events))
	for event := range c.events {
		list = append(list, event)
	}
	return list
}

func (c *client) send(value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.outbound <- payload:
	case <-c.done:
	default:
		// A slow consumer is dropped instead of growing an unbounded queue.
		c.close()
	}
}

func (c *client) writeLoop() {
	for {
		select {
		case payload := <-c.outbound:
			c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := c.writer.Write(payload); err != nil {
				c.close()
				return
			}
			if err := c.writer.Flush(); err != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.server.mu.Lock()
		delete(c.server.clients, c)
		c.server.mu.Unlock()
		c.conn.Close()
	})
}

// readLine reads one newline-terminated line with a hard size cap.
func readLine(reader *bufio.Reader, max int) (string, error) {
	var builder strings.Builder
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			return "", err
		}
		if builder.Len()+len(chunk) > max {
			return "", errors.New("line exceeds the maximum size")
		}
		builder.Write(chunk)
		if !isPrefix {
			return builder.String(), nil
		}
	}
}

// Client is the CLI side of the local API.
type Client struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	token  string

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan response
	events  chan Event

	// stop is closed by Close independent of the read loop, so a read loop
	// blocked on a full event channel can always be released.
	stop       chan struct{}
	done       chan struct{}
	writeToken chan struct{}
	ioTimeout  time.Duration
	closeOnce  sync.Once
	errMu      sync.Mutex
	err        error
}

type response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *protocol.Error `json:"error"`
}

// Dial connects to the daemon IPC endpoint and verifies liveness with ping.
func Dial(ctx context.Context, address, token string) (*Client, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.CodeDaemonNotRunning,
			Message: fmt.Sprintf("no daemon on %s: %v", address, err)}
	}
	client := &Client{
		conn:       conn,
		reader:     bufio.NewReaderSize(conn, 64*1024),
		writer:     bufio.NewWriterSize(conn, 64*1024),
		token:      token,
		pending:    map[string]chan response{},
		events:     make(chan Event, 256),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		writeToken: make(chan struct{}, 1),
		ioTimeout:  15 * time.Second,
	}
	client.writeToken <- struct{}{}
	go client.readLoop()
	if _, failure := client.Call(ctx, "ping", nil); failure != nil {
		client.Close()
		return nil, failure
	}
	return client, nil
}

// Events exposes pushed events (subscribe first).
func (c *Client) Events() <-chan Event { return c.events }

// Done closes when the IPC connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Call runs one method with no end-to-end request id.
func (c *Client) Call(ctx context.Context, method string, params map[string]any) (any, *protocol.Error) {
	return c.CallWithID(ctx, method, params, "")
}

// CallWithID runs one method. When requestID is non-empty it is forwarded to
// the daemon so a write keeps one stable identity from the CLI to the mod; a
// timeout or connection loss after dispatch is then reported result-unknown
// with that id instead of a generic error.
func (c *Client) CallWithID(ctx context.Context, method string, params map[string]any,
	requestID string) (any, *protocol.Error) {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("%d", c.nextID)
	channel := make(chan response, 1)
	c.pending[id] = channel
	c.mu.Unlock()
	defer c.forget(id)

	if err := ctx.Err(); err != nil {
		return nil, notSent(method, requestID, err)
	}
	// Acquire the writer slot cancellably: a cancelled call must never send.
	select {
	case <-c.writeToken:
	case <-ctx.Done():
		return nil, notSent(method, requestID, ctx.Err())
	case <-c.stop:
		return nil, notSent(method, requestID, errors.New("the daemon connection is closed"))
	}
	released := false
	release := func() {
		if !released {
			released = true
			c.writeToken <- struct{}{}
		}
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, notSent(method, requestID, err)
	}

	request := Request{ID: id, Method: method, Params: params, Auth: c.token, RequestID: requestID}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeInternal, err.Error())
	}
	payload = append(payload, '\n')
	writeDeadline := time.Now().Add(c.ioTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(writeDeadline) {
		writeDeadline = deadline
	}
	if err := c.conn.SetWriteDeadline(writeDeadline); err != nil {
		return nil, notSent(method, requestID, err)
	}
	// Abort a blocked socket write on ctx.Done, not only on the deadline; join
	// the watcher before clearing the deadline and freeing the slot.
	watcherStop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_ = c.conn.SetWriteDeadline(time.Now())
		case <-watcherStop:
		}
	}()
	_, writeErr := c.writer.Write(payload)
	if writeErr == nil {
		writeErr = c.writer.Flush()
	}
	close(watcherStop)
	<-watcherDone
	_ = c.conn.SetWriteDeadline(time.Time{})
	release()
	if writeErr != nil {
		// A partial request line may be on the wire; this connection is
		// poisoned and the caller gets a possibly-dispatched classification.
		go c.Close()
		return nil, possiblyDispatched(method, requestID, writeErr)
	}

	select {
	case reply := <-channel:
		if !reply.OK {
			if reply.Error == nil {
				return nil, protocol.NewError(protocol.CodeInternal, "the daemon returned an empty error")
			}
			return nil, reply.Error
		}
		var value any
		if err := json.Unmarshal(reply.Result, &value); err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, "the daemon returned unreadable JSON")
		}
		return value, nil
	case <-ctx.Done():
		return nil, possiblyDispatched(method, requestID, ctx.Err())
	case <-c.stop:
		return nil, possiblyDispatched(method, requestID, errors.New("the daemon connection closed"))
	}
}

func notSent(method, requestID string, cause error) *protocol.Error {
	return &protocol.Error{
		Code:          protocol.CodeDaemonNotRunning,
		Message:       fmt.Sprintf("the local request %s was not sent: %v", method, cause),
		Retryable:     true,
		ResultUnknown: false,
		RequestID:     requestID,
		Operation:     method,
	}
}

func possiblyDispatched(method, requestID string, cause error) *protocol.Error {
	unknown := requestID != ""
	return &protocol.Error{
		Code:          protocol.CodeConnectionLost,
		Message:       fmt.Sprintf("the local request %s may have reached the daemon: %v", method, cause),
		Retryable:     !unknown,
		ResultUnknown: unknown,
		RequestID:     requestID,
		Operation:     method,
	}
}

func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Close ends the connection. It is safe with a full event channel and no
// consumer: the independent stop closes first, so the read loop can abandon a
// blocked delivery instead of deadlocking Close.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
		_ = c.conn.SetDeadline(time.Now())
		c.conn.Close()
	})
	<-c.done
}

func (c *Client) readLoop() {
	defer close(c.done)
	defer close(c.events)
	for {
		line, err := readLine(c.reader, MaxLineBytes)
		if err != nil {
			c.finish(err)
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var envelope struct {
			Type   string          `json:"type"`
			ID     string          `json:"id"`
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
			Error  *protocol.Error `json:"error"`
			Event  string          `json:"event"`
			Data   map[string]any  `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			continue
		}
		switch envelope.Type {
		case "response":
			c.mu.Lock()
			channel := c.pending[envelope.ID]
			delete(c.pending, envelope.ID)
			c.mu.Unlock()
			if channel != nil {
				channel <- response{OK: envelope.OK, Result: envelope.Result, Error: envelope.Error}
			}
		case "event":
			select {
			case c.events <- Event{Type: "event", Event: envelope.Event, Data: envelope.Data}:
			case <-c.stop:
				return
			}
		}
	}
}

func (c *Client) finish(cause error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = cause
	}
	c.errMu.Unlock()
	c.conn.Close()
	c.mu.Lock()
	for id, channel := range c.pending {
		delete(c.pending, id)
		channel <- response{OK: false, Error: protocol.NewError(protocol.CodeDaemonNotRunning,
			"the daemon connection closed")}
	}
	c.mu.Unlock()
}
