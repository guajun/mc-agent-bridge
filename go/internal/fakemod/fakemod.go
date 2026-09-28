// Package fakemod is a scriptable in-process stand-in for the mc-agent
// interface mod's control protocol. It is used by the daemon tests and by the
// `fake` transport so the whole daemon/CLI path can be exercised without a
// Minecraft process, on a real TLS socket with the same framing, handshake,
// credential and event semantics as the Java server.
package fakemod

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/control"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

// OpFunc handles one scripted operation.
type OpFunc func(params map[string]any) (any, *protocol.Error)

// Options configures the fake server.
type Options struct {
	Token        string
	InstanceID   string
	RunID        string
	Capabilities []string
	Permissions  []string
	Ops          map[string]OpFunc
	EventBuffer  int
	// SilentHello accepts the TLS connection and reads the hello but never
	// sends a welcome, for handshake-timeout tests.
	SilentHello bool
	// StallRead stops reading after the welcome, so a large client write
	// blocks on the socket and exercises write deadlines/cancellation.
	StallRead bool
}

// Server is one fake control endpoint.
type Server struct {
	listener net.Listener
	certPEM  []byte
	pin      string
	address  string

	mu          sync.Mutex
	requests    atomic.Int64
	seq         int64
	writeSeq    int64
	ring        []map[string]any
	ringSize    int
	connections map[*connection]struct{}
	recent      map[string]map[string]any
	closed      bool

	options Options
}

type connection struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	mu     sync.Mutex
	server *Server
	runID  string
}

// Start binds a TLS control endpoint on 127.0.0.1 with a generated identity.
func Start(options Options) (*Server, error) {
	certificate, certPEM, pin, err := generateCertificate()
	if err != nil {
		return nil, err
	}
	if options.InstanceID == "" {
		options.InstanceID = "inst_fake0001"
	}
	if options.RunID == "" {
		options.RunID = "run_fake0001"
	}
	if options.Capabilities == nil {
		options.Capabilities = []string{"state", "entities", "player", "player:view", "command",
			"context", "wait", "mark", "snapshot", "events:game", "events:chat"}
	}
	if options.Permissions == nil {
		options.Permissions = []string{"read", "write"}
	}
	if options.EventBuffer <= 0 {
		options.EventBuffer = 256
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	server := &Server{
		listener:    listener,
		certPEM:     certPEM,
		pin:         pin,
		address:     listener.Addr().String(),
		ringSize:    options.EventBuffer,
		connections: map[*connection]struct{}{},
		recent:      map[string]map[string]any{},
		options:     options,
	}
	go server.acceptLoop()
	return server, nil
}

// Address is the host:port the fake server listens on.
func (s *Server) Address() string { return s.address }

// Pin is the sha256:<hex> fingerprint of the generated certificate.
func (s *Server) Pin() string { return s.pin }

// CertPEM is the generated certificate, for CA-file tests.
func (s *Server) CertPEM() []byte { return s.certPEM }

// Close stops the listener and all connections.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	connections := make([]*connection, 0, len(s.connections))
	for c := range s.connections {
		connections = append(connections, c)
	}
	s.mu.Unlock()
	s.listener.Close()
	for _, c := range connections {
		c.conn.Close()
	}
}

// SetRunID simulates a game restart: a new run id, an empty event stream and
// an empty write ledger, and every live connection dropped.
func (s *Server) SetRunID(runID string) {
	s.mu.Lock()
	s.options.RunID = runID
	s.seq = 0
	s.writeSeq = 0
	s.ring = nil
	s.recent = map[string]map[string]any{}
	s.mu.Unlock()
	s.DisconnectAll()
}

// DisconnectAll closes the live control connections but keeps listening. It
// simulates a dropped link so reconnect/reconciliation paths can be tested.
func (s *Server) DisconnectAll() {
	s.mu.Lock()
	connections := make([]*connection, 0, len(s.connections))
	for c := range s.connections {
		connections = append(connections, c)
	}
	s.mu.Unlock()
	for _, c := range connections {
		c.conn.Close()
	}
}

// Requests returns how many request frames the server has received.
func (s *Server) Requests() int64 { return s.requests.Load() }

// Push publishes a scripted event and returns its sequence number.
func (s *Server) Push(eventType string, fields map[string]any) int64 {
	payload := map[string]any{"type": eventType, "event": true}
	for key, value := range fields {
		payload[key] = value
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	envelope := map[string]any{
		"type":             "event",
		"seq":              seq,
		"runId":            s.options.RunID,
		"streamId":         s.options.RunID,
		"serverTimeMillis": time.Now().UnixMilli(),
		"event":            payload,
	}
	s.ring = append(s.ring, envelope)
	if len(s.ring) > s.ringSize {
		s.ring = s.ring[len(s.ring)-s.ringSize:]
	}
	connections := make([]*connection, 0, len(s.connections))
	for c := range s.connections {
		connections = append(connections, c)
	}
	s.mu.Unlock()
	for _, c := range connections {
		c.writeFrame(envelope)
	}
	return seq
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *Server) serve(conn net.Conn) {
	// Snapshot the mutable option fields under the lock: SetRunID can be
	// called concurrently with an accepted connection.
	s.mu.Lock()
	options := s.options
	s.mu.Unlock()
	client := &connection{
		conn:   conn,
		reader: bufio.NewReader(conn),
		writer: bufio.NewWriter(conn),
		server: s,
		runID:  options.RunID,
	}
	defer conn.Close()

	payload, err := control.ReadFrame(client.reader)
	if err != nil {
		return
	}
	var hello control.Hello
	if err := json.Unmarshal(payload, &hello); err != nil || hello.Type != "hello" {
		client.writeFrame(map[string]any{"type": "error", "code": "protocol_error",
			"message": "the first frame must be a hello"})
		client.flush()
		return
	}
	if hello.Protocol != protocol.ControlProtocolVersion {
		client.writeFrame(map[string]any{"type": "error", "code": "protocol_version",
			"message": "unsupported control protocol"})
		client.flush()
		return
	}
	if options.Token != "" && hello.Token != options.Token {
		client.writeFrame(map[string]any{"type": "error", "code": "unauthorized",
			"message": "the access credential was rejected"})
		client.flush()
		return
	}
	if options.SilentHello {
		// Read until the peer gives up; never send a welcome.
		_, _ = client.reader.ReadByte()
		return
	}
	s.mu.Lock()
	events := make([]map[string]any, 0, len(s.ring))
	// A cursor from a different run (or one ahead of this run's sequence) is
	// not usable: the client gets a fresh view and an explicit loss report.
	lost := false
	effectiveLastSeq := hello.LastSeq
	if hello.RunID != "" && hello.RunID != options.RunID {
		effectiveLastSeq = 0
		lost = true
	}
	if effectiveLastSeq > s.seq {
		effectiveLastSeq = 0
		lost = true
	}
	for _, envelope := range s.ring {
		if seq, _ := envelope["seq"].(int64); seq > effectiveLastSeq {
			events = append(events, envelope)
		}
	}
	oldest := int64(0)
	if len(s.ring) > 0 {
		oldest, _ = s.ring[0]["seq"].(int64)
	}
	newest := s.seq
	if newest > 0 && effectiveLastSeq+1 < oldest {
		lost = true
	}
	s.connections[client] = struct{}{}
	s.mu.Unlock()

	welcome := map[string]any{
		"type":               "welcome",
		"protocol":           protocol.ControlProtocolVersion,
		"mod":                "mc-agent-interface",
		"modVersion":         "0.8.0-fake",
		"minecraft":          "26.2",
		"instanceId":         options.InstanceID,
		"runId":              options.RunID,
		"runStartedAtMillis": time.Now().UnixMilli(),
		"sessionId":          fmt.Sprintf("sess_fake_%d", time.Now().UnixNano()),
		"transport":          "fake-tls",
		"serverTimeMillis":   time.Now().UnixMilli(),
		"permissions":        options.Permissions,
		"capabilities":       options.Capabilities,
		"replay": map[string]any{
			"requestedSince": hello.LastSeq,
			"from":           effectiveLastSeq + 1,
			"to":             newest,
			"lost":           lost,
			"bufferedEvents": len(events),
		},
		"limits": map[string]any{"maxFrameBytes": control.MaxFrameBytes, "controlProtocol": 1},
	}
	client.writeFrame(welcome)
	for _, event := range events {
		copy := map[string]any{}
		for key, value := range event {
			copy[key] = value
		}
		copy["replay"] = true
		client.writeFrame(copy)
	}
	if err := client.flush(); err != nil {
		s.removeConnection(client)
		return
	}
	if options.StallRead {
		// Do not read further: the peer's write buffer fills and its write
		// deadline decides how long that may take.
		time.Sleep(30 * time.Second)
		s.removeConnection(client)
		return
	}

	for {
		payload, err := control.ReadFrame(client.reader)
		if err != nil {
			s.removeConnection(client)
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(payload, &frame); err != nil {
			s.removeConnection(client)
			return
		}
		frameType, _ := frame["type"].(string)
		switch frameType {
		case "request":
			s.requests.Add(1)
			go s.handleRequest(client, frame, options)
		case "ping":
			id, _ := frame["id"].(string)
			client.writeReply(map[string]any{"pong": true})
			_ = id
		default:
			client.writeFrame(map[string]any{"type": "error", "code": "protocol_error",
				"message": "unknown frame type " + frameType})
			client.flush()
			s.removeConnection(client)
			return
		}
	}
}

func (s *Server) removeConnection(client *connection) {
	s.mu.Lock()
	delete(s.connections, client)
	s.mu.Unlock()
}

func (s *Server) handleRequest(client *connection, frame map[string]any, options Options) {
	id, _ := frame["id"].(string)
	operation, _ := frame["op"].(string)
	params, _ := frame["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	write := protocol.WriteOperation(operation)

	if write {
		if prior := s.recentWrite(id); prior != nil {
			reply := map[string]any{"type": "reply", "ok": true, "id": id, "result": prior["result"]}
			client.writeFrame(reply)
			return
		}
		s.mu.Lock()
		s.recent[id] = map[string]any{"state": "pending"}
		s.mu.Unlock()
	}

	if operation == "request_status" {
		requestID, _ := params["requestId"].(string)
		client.writeResult(id, s.statusFor(requestID))
		return
	}
	if operation == "ping" {
		client.writeResult(id, map[string]any{"pong": true, "runId": options.RunID})
		return
	}
	if strings.HasPrefix(operation, "exclusive_") {
		client.writeResult(id, map[string]any{"key": params["key"], "acquired": true})
		return
	}

	if handler, ok := options.Ops[operation]; ok {
		result, failure := handler(params)
		if failure != nil {
			client.writeError(id, failure)
			return
		}
		s.completeWrite(client, id, operation, result)
		return
	}
	result := map[string]any{"type": operation, "stub": true}
	for key, value := range params {
		result[key] = value
	}
	if operation == "command" {
		result["type"] = "cmd_ack"
		result["detail"] = params["command"]
		result["output"] = []string{"fake:" + fmt.Sprint(params["command"])}
	}
	if operation == "state" {
		result["worldDir"] = "C:\\fakemod\\world"
		result["levelName"] = "fake-world"
		result["tick"] = s.seq
	}
	s.completeWrite(client, id, operation, result)
}

func (s *Server) completeWrite(client *connection, id, operation string, result any) {
	if protocol.WriteOperation(operation) && !strings.HasPrefix(operation, "exclusive_") {
		s.mu.Lock()
		s.writeSeq++
		writeSeq := s.writeSeq
		s.recent[id] = map[string]any{"state": "completed", "result": result, "writeSeq": writeSeq}
		s.mu.Unlock()
		if object, ok := result.(map[string]any); ok {
			object["writeSeq"] = writeSeq
		}
		s.Push("write", map[string]any{"writeSeq": writeSeq, "op": operation, "requestId": id, "ok": true})
	}
	client.writeResult(id, result)
}

func (s *Server) recentWrite(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.recent[id]
	if entry == nil || entry["state"] != "completed" {
		return nil
	}
	return entry
}

func (s *Server) statusFor(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.recent[id]
	if entry == nil {
		return map[string]any{"requestId": id, "state": "unknown"}
	}
	result := map[string]any{"requestId": id, "state": entry["state"]}
	if value, ok := entry["result"]; ok {
		result["result"] = value
	}
	if value, ok := entry["writeSeq"]; ok {
		result["writeSeq"] = value
	}
	return result
}

func (c *connection) writeResult(id string, result any) {
	c.writeFrame(map[string]any{"type": "reply", "ok": true, "id": id, "result": result,
		"serverTimeMillis": time.Now().UnixMilli()})
}

func (c *connection) writeError(id string, failure *protocol.Error) {
	c.writeFrame(map[string]any{"type": "reply", "ok": false, "id": id,
		"error": map[string]any{"code": failure.Code, "message": failure.Message,
			"retryable": failure.Retryable, "resultUnknown": failure.ResultUnknown}})
}

func (c *connection) writeReply(fields map[string]any) {
	frame := map[string]any{"type": "reply", "ok": true, "id": "0"}
	for key, value := range fields {
		frame[key] = value
	}
	c.writeFrame(frame)
}

func (c *connection) writeFrame(frame map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = control.WriteFrame(c.writer, frame)
	_ = c.writer.Flush()
}

func (c *connection) flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writer.Flush()
}

// generateCertificate creates a self-signed ECDSA P-256 certificate with
// loopback SANs; the tests pin its SHA-256 fingerprint.
func generateCertificate() (tls.Certificate, []byte, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, "", err
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mc-agent-fakemod"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, "", err
	}
	sum := sha256.Sum256(der)
	return pair, certPEM, "sha256:" + hex.EncodeToString(sum[:]), nil
}
