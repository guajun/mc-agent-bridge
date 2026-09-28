// Package control is the Go client of the authenticated same-port control
// protocol (mc-agent-interface-mod issue #8).
//
// The client verifies the server identity with a pinned certificate SHA-256
// fingerprint or with a CA file loaded as the only trust root. Certificate
// verification is never disabled: the pin path sets InsecureSkipVerify only to
// replace Go's hostname-based check with an explicit fingerprint comparison in
// VerifyPeerCertificate (which also enforces the validity window).
package control

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/protocol"
	"github.com/guajun/mc-agent-bridge/internal/version"
)

// Config describes one remote server endpoint.
type Config struct {
	Address    string
	Pin        string // sha256:<hex> or bare hex; accepted with colons
	CAFile     string // PEM bundle used as the only trust root
	ServerName string // required with CAFile when the address is not the cert name
	Token      string
	LastSeq    int64
	// ExpectRunID tells the server which run the LastSeq cursor belongs to.
	// When it differs from the server's run, the server treats LastSeq as 0
	// and reports the replay as lost instead of silently skipping the new
	// run's events.
	ExpectRunID string
	ClientName  string
	DialTimeout time.Duration
	IOTimeout   time.Duration
}

// Client is one live control connection. Requests are serialized; replies are
// routed by request id; events are delivered on Events().
type Client struct {
	config Config
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer

	// nonce makes request ids unique across processes that share a credential.
	// The server de-duplicates writes by (token, request id), so two daemons on
	// the same token must never both start at "1".
	nonce  string
	nextID atomic.Uint64

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan *Reply

	events chan Event

	done      chan struct{}
	stop      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error

	welcome *Welcome
}

// Dial connects, completes the hello/welcome handshake and returns a client
// whose read loop is already running.
func Dial(ctx context.Context, config Config) (*Client, *Welcome, error) {
	if config.Token == "" {
		return nil, nil, protocol.NewError(protocol.CodeUnauthorized,
			"no credential was loaded for this target")
	}
	tlsConfig, err := tlsConfigFor(config)
	if err != nil {
		return nil, nil, &protocol.Error{Code: protocol.CodeUsage, Message: err.Error()}
	}
	timeout := config.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	dialContext := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		dialContext, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	dialer := &tls.Dialer{Config: tlsConfig, NetDialer: &net.Dialer{Timeout: timeout}}
	conn, err := dialer.DialContext(dialContext, "tcp", config.Address)
	if err != nil {
		return nil, nil, &protocol.Error{Code: protocol.CodeConnectionFailed,
			Message: fmt.Sprintf("cannot reach %s over TLS: %v", config.Address, unwrapTLS(err))}
	}
	client := &Client{
		config:  config,
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 64*1024),
		writer:  bufio.NewWriterSize(conn, 64*1024),
		nonce:   newNonce(),
		pending: map[string]chan *Reply{},
		events:  make(chan Event, 1024),
		done:    make(chan struct{}),
		stop:    make(chan struct{}),
	}
	name := config.ClientName
	if name == "" {
		name = "mc-agent"
	}
	hello := Hello{
		Type:     FrameHello,
		Protocol: protocol.ControlProtocolVersion,
		Token:    config.Token,
		LastSeq:  config.LastSeq,
		RunID:    config.ExpectRunID,
		Client:   ClientInfo{Name: name, Version: protocolVersion()},
	}
	// The hello/welcome exchange is bounded: after the TLS handshake the
	// context no longer covers the socket, so both directions get a deadline.
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, nil, &protocol.Error{Code: protocol.CodeConnectionFailed,
			Message: fmt.Sprintf("cannot arm the control write deadline: %v", err)}
	}
	if err := WriteFrame(client.writer, hello); err != nil {
		conn.Close()
		return nil, nil, &protocol.Error{Code: protocol.CodeConnectionFailed,
			Message: fmt.Sprintf("cannot send hello: %v", err)}
	}
	if err := client.writer.Flush(); err != nil {
		conn.Close()
		return nil, nil, &protocol.Error{Code: protocol.CodeConnectionFailed,
			Message: fmt.Sprintf("cannot send hello: %v", err)}
	}
	// The welcome (or a fatal error) is the first frame.
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, nil, &protocol.Error{Code: protocol.CodeConnectionFailed,
			Message: fmt.Sprintf("cannot arm the control read deadline: %v", err)}
	}
	payload, err := ReadFrame(client.reader)
	if err != nil {
		conn.Close()
		return nil, nil, &protocol.Error{Code: protocol.CodeConnectionFailed,
			Message: fmt.Sprintf("no welcome from %s within %s: %v", config.Address, timeout, unwrapTLS(err))}
	}
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		conn.Close()
		return nil, nil, protocol.NewError(protocol.CodeConnectionFailed, "the server sent an unreadable first frame")
	}
	switch envelope.Type {
	case FrameError:
		var fatal struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &fatal)
		conn.Close()
		code := fatal.Code
		if code == "" {
			code = protocol.CodeConnectionFailed
		}
		return nil, nil, &protocol.Error{Code: code, Message: fatal.Message}
	case FrameWelcome:
		welcome := &Welcome{Raw: payload}
		if err := json.Unmarshal(payload, welcome); err != nil {
			conn.Close()
			return nil, nil, protocol.NewError(protocol.CodeConnectionFailed, "the welcome frame is malformed")
		}
		if welcome.Protocol != protocol.ControlProtocolVersion {
			conn.Close()
			return nil, nil, &protocol.Error{Code: protocol.CodeCapabilityNotSupported,
				Message: fmt.Sprintf("the server speaks control protocol %d; this client speaks %d",
					welcome.Protocol, protocol.ControlProtocolVersion)}
		}
		client.welcome = welcome
	default:
		conn.Close()
		return nil, nil, protocol.NewError(protocol.CodeConnectionFailed,
			"expected a welcome, got frame type "+envelope.Type)
	}
	go client.readLoop()
	return client, client.welcome, nil
}

// Welcome returns the negotiated session metadata.
func (c *Client) Welcome() *Welcome { return c.welcome }

// Events returns the sequenced event stream.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended.
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		return errClosed
	}
	return c.err
}

// NextRequestID returns a request id that is unique across this process,
// this client instance and reconnects: <random nonce>-<counter>.
func (c *Client) NextRequestID() string {
	return c.nonce + "-" + strconv.FormatUint(c.nextID.Add(1), 10)
}

// Call sends one request with a freshly generated id and waits for its reply.
func (c *Client) Call(ctx context.Context, operation string, params map[string]any) (json.RawMessage, *protocol.Error) {
	return c.CallWithID(ctx, c.NextRequestID(), operation, params)
}

// CallWithID sends one request under a caller-chosen id. The daemon uses this
// to persist the request identity before the bytes leave the process, so a
// crash cannot lose which non-idempotent write may have run.
func (c *Client) CallWithID(ctx context.Context, id string, operation string, params map[string]any) (json.RawMessage, *protocol.Error) {
	if id == "" || len(id) > 128 {
		return nil, protocol.NewError(protocol.CodeBadRequest, "request id must be 1..128 characters")
	}
	if params == nil {
		params = map[string]any{}
	}
	replyCh := make(chan *Reply, 1)
	c.pendingMu.Lock()
	select {
	case <-c.stop:
		c.pendingMu.Unlock()
		return nil, protocol.NewError(protocol.CodeConnectionLost, "the control connection is closed")
	default:
	}
	if _, exists := c.pending[id]; exists {
		c.pendingMu.Unlock()
		return nil, &protocol.Error{Code: protocol.CodeBadRequest,
			Message: "request id " + id + " is already in flight", RequestID: id, Operation: operation}
	}
	c.pending[id] = replyCh
	c.pendingMu.Unlock()

	frame := Request{Type: FrameRequest, ID: id, Op: operation, Params: params}
	if timeout, ok := params["_timeoutMillis"].(int64); ok {
		frame.TimeoutMillis = timeout
	}
	// A write deadline keeps a stuck socket from holding writeMu forever and
	// makes an expired context able to fail instead of leaking a goroutine.
	writeDeadline := time.Now().Add(15 * time.Second)
	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(writeDeadline)
	err := WriteFrame(c.writer, frame)
	if err == nil {
		err = c.writer.Flush()
	}
	_ = c.conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()
	if err != nil {
		c.forget(id)
		cause := protocol.NewError(protocol.CodeConnectionLost,
			fmt.Sprintf("cannot send request %s: %v", operation, err))
		cause.RequestID = id
		cause.Operation = operation
		// The frame may have been partially written; a write is never
		// assumed safe to retry without asking the server.
		cause.ResultUnknown = protocol.WriteOperation(operation)
		return nil, cause
	}

	select {
	case reply := <-replyCh:
		if reply == nil {
			return nil, &protocol.Error{Code: protocol.CodeConnectionLost,
				Message:       "the control connection closed while waiting",
				ResultUnknown: protocol.WriteOperation(operation),
				RequestID:     id, Operation: operation}
		}
		if reply.OK {
			return reply.Result, nil
		}
		if reply.Error == nil {
			return nil, &protocol.Error{Code: protocol.CodeGameError,
				Message: "the server returned an empty error", RequestID: id, Operation: operation}
		}
		return nil, &protocol.Error{
			Code:          reply.Error.Code,
			Message:       reply.Error.Message,
			Retryable:     reply.Error.Retryable,
			ResultUnknown: reply.Error.ResultUnknown,
			RequestID:     id,
			Operation:     operation,
			Details:       reply.Error.Details,
		}
	case <-ctx.Done():
		c.forget(id)
		return nil, &protocol.Error{
			Code:          protocol.CodeTimeout,
			Message:       fmt.Sprintf("no answer for %s within the deadline", operation),
			Retryable:     !protocol.WriteOperation(operation),
			ResultUnknown: protocol.WriteOperation(operation),
			RequestID:     id,
			Operation:     operation,
		}
	case <-c.done:
		c.forget(id)
		return nil, &protocol.Error{
			Code:          protocol.CodeConnectionLost,
			Message:       fmt.Sprintf("the control connection closed while %s was in flight", operation),
			Retryable:     false,
			ResultUnknown: protocol.WriteOperation(operation),
			RequestID:     id,
			Operation:     operation,
		}
	}
}

func (c *Client) forget(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// Close terminates the connection and drains the read loop. It is safe to
// call with a full event channel and with no consumer: closing stop makes the
// read loop abandon a blocked event delivery instead of deadlocking.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.stop)
		c.conn.Close()
	})
	<-c.done
	return nil
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		payload, err := ReadFrame(c.reader)
		if err != nil {
			c.finish(err)
			return
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			c.finish(fmt.Errorf("unreadable frame: %w", err))
			return
		}
		switch envelope.Type {
		case FrameReply:
			var reply Reply
			if err := json.Unmarshal(payload, &reply); err != nil {
				c.finish(fmt.Errorf("malformed reply: %w", err))
				return
			}
			c.pendingMu.Lock()
			channel := c.pending[reply.ID]
			delete(c.pending, reply.ID)
			c.pendingMu.Unlock()
			if channel != nil {
				channel <- &reply
			}
		case FrameEvent:
			var envelope EventEnvelope
			if err := json.Unmarshal(payload, &envelope); err != nil {
				c.finish(fmt.Errorf("malformed event: %w", err))
				return
			}
			select {
			case c.events <- Event{
				Seq:      envelope.Seq,
				RunID:    envelope.RunID,
				StreamID: envelope.StreamID,
				Replay:   envelope.Replay,
				Payload:  envelope.Event,
			}:
			case <-c.stop:
				return
			}
		case FrameError:
			var fatal struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(payload, &fatal)
			c.finish(&protocol.Error{Code: fatal.Code, Message: fatal.Message})
			return
		case FrameWelcome:
			// A second welcome is a protocol violation.
			c.finish(protocol.NewError(protocol.CodeConnectionFailed, "the server sent a second welcome"))
			return
		default:
			c.finish(protocol.NewError(protocol.CodeConnectionFailed,
				"unknown frame type "+envelope.Type))
			return
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
	c.pendingMu.Lock()
	for id, channel := range c.pending {
		delete(c.pending, id)
		channel <- nil
	}
	c.pendingMu.Unlock()
}

func newNonce() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}

// tlsConfigFor builds the verified TLS configuration.
func tlsConfigFor(config Config) (*tls.Config, error) {
	base := &tls.Config{MinVersion: tls.VersionTLS12}
	if pin := strings.TrimSpace(config.Pin); pin != "" {
		normalized, err := normalizePin(pin)
		if err != nil {
			return nil, err
		}
		base.InsecureSkipVerify = true // replaced by explicit fingerprint verification below
		base.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("the server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			actual := hex.EncodeToString(sum[:])
			if actual != normalized {
				return fmt.Errorf("server certificate pin mismatch: expected sha256:%s, got sha256:%s",
					normalized, actual)
			}
			certificate, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("cannot parse the server certificate: %w", err)
			}
			now := time.Now()
			if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
				return fmt.Errorf("the server certificate is not valid at %s", now.Format(time.RFC3339))
			}
			return nil
		}
		return base, nil
	}
	if config.CAFile == "" {
		return nil, errors.New("a remote target needs --pin sha256:<hex> or --ca <pem>")
	}
	pem, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read CA file %s: %w", config.CAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA file %s contains no PEM certificate", config.CAFile)
	}
	name := config.ServerName
	if name == "" {
		host, _, err := net.SplitHostPort(config.Address)
		if err != nil {
			host = config.Address
		}
		name = strings.Trim(host, "[]")
	}
	base.RootCAs = pool
	base.ServerName = name
	return base, nil
}

func normalizePin(pin string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(pin))
	value = strings.TrimPrefix(value, "sha256:")
	value = strings.ReplaceAll(value, ":", "")
	value = strings.ReplaceAll(value, " ", "")
	if len(value) != 64 {
		return "", fmt.Errorf("pin %q is not a SHA-256 fingerprint (expected 64 hex characters)", pin)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("pin %q is not hex: %w", pin, err)
	}
	return value, nil
}

// unwrapTLS keeps the certificate failure visible instead of hiding it behind
// a generic connection reset.
func unwrapTLS(err error) error {
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &recordHeaderError) {
		return fmt.Errorf("%w (the endpoint does not look like the TLS control protocol; "+
			"a Minecraft player port does not answer control connections)", err)
	}
	return err
}

func protocolVersion() string {
	return version.Version
}
