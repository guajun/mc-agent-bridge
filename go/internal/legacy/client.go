// Package legacy is the explicit adapter to the pre-#8 local JSON-lines mod
// protocol (the loopback port written to port.txt). It exists so released mod
// builds and the Python-era workflows keep working; the formal product path is
// the remote TLS control protocol.
package legacy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// MaxLineBytes matches the Python bridge's 16 MiB line budget.
const MaxLineBytes = 16 * 1024 * 1024

// Client is one JSON-lines connection to the legacy loopback endpoint.
type Client struct {
	address string
	conn    net.Conn
	reader  *bufio.Reader
	writer  *bufio.Writer

	requestMu sync.Mutex
	writeMu   sync.Mutex

	hello map[string]any

	replies chan map[string]any
	events  chan map[string]any

	done      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

// Dial connects and waits for the hello line.
func Dial(ctx context.Context, address string, timeout time.Duration) (*Client, error) {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	client := &Client{
		address: address,
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 64*1024),
		writer:  bufio.NewWriterSize(conn, 64*1024),
		replies: make(chan map[string]any, 4),
		events:  make(chan map[string]any, 1024),
		done:    make(chan struct{}),
	}
	go client.readLoop()
	select {
	case hello := <-client.replies:
		client.hello = hello
		return client, nil
	case <-time.After(timeout):
		conn.Close()
		return nil, errors.New("the legacy mod did not send a hello message")
	case <-client.done:
		return nil, fmt.Errorf("the legacy mod closed the connection before hello")
	}
}

// Hello is the mod's greeting frame.
func (c *Client) Hello() map[string]any { return c.hello }

// Capabilities extracts the advertised capability list, or nil when the hello
// has none (an old mod the daemon cannot filter).
func (c *Client) Capabilities() []string {
	value, ok := c.hello["capabilities"].([]any)
	if !ok {
		return nil
	}
	capabilities := make([]string, 0, len(value))
	for _, entry := range value {
		if text, ok := entry.(string); ok {
			capabilities = append(capabilities, text)
		}
	}
	return capabilities
}

// Instance is the vantage name from hello ("server" or "client").
func (c *Client) Instance() string {
	if value, ok := c.hello["instance"].(string); ok {
		return value
	}
	return ""
}

// Events exposes pushed events.
func (c *Client) Events() <-chan map[string]any { return c.events }

// Done closes when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports the terminal cause.
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		return errors.New("legacy connection is closed")
	}
	return c.err
}

// Request sends one legacy line and returns the next non-event reply. Requests
// are serialized because the mod answers one line at a time.
func (c *Client) Request(ctx context.Context, line string, timeout time.Duration) (map[string]any, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	c.writeMu.Lock()
	_, err := c.writer.WriteString(line + "\n")
	if err == nil {
		err = c.writer.Flush()
	}
	c.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	select {
	case reply := <-c.replies:
		return reply, nil
	case <-deadline.C:
		return nil, fmt.Errorf("no response for %q within %s", line, timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.Err()
	}
}

// Close ends the connection.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.conn.Close()
	})
	<-c.done
	return nil
}

func (c *Client) readLoop() {
	defer close(c.done)
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
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			continue // a broken line must not kill the link
		}
		if isEvent(message) {
			select {
			case c.events <- message:
			case <-c.done:
				return
			}
			continue
		}
		select {
		case c.replies <- message:
		case <-c.done:
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
}

// isEvent mirrors the Python bridge: the explicit marker wins, a known reply
// type or a *_ack suffix is a reply, everything else is an event.
func isEvent(message map[string]any) bool {
	if marker, ok := message["event"].(bool); ok && marker {
		return true
	}
	kind, _ := message["type"].(string)
	if kind == "" {
		return false
	}
	switch kind {
	case "hello", "state", "entities", "screen", "capabilities", "error", "pong",
		"snapshot_ack", "snapshots", "player", "player_context", "context", "context_bundle":
		return false
	}
	return !strings.HasSuffix(kind, "_ack")
}

func readLine(reader *bufio.Reader, max int) (string, error) {
	var builder strings.Builder
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			return "", err
		}
		if builder.Len()+len(chunk) > max {
			return "", fmt.Errorf("line longer than %d bytes", max)
		}
		builder.Write(chunk)
		if !isPrefix {
			return builder.String(), nil
		}
	}
}
