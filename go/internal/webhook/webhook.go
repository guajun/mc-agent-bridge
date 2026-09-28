// Package webhook forwards daemon events to a generic HTTP receiver.
//
// This is the Go port of the Python bridge's webhook forwarder (bridge #2):
// the same HMAC-SHA256 signature over "<timestamp>.<body>", the same
// X-MC-Agent-* headers, the same eventId stability across retries, and the
// same bounded in-memory queue and retry policy. Bridge #7 owns the remaining
// Hermes-specific interoperability follow-up.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults mirror the Python bridge.
const (
	DefaultQueueSize   = 256
	DefaultMaxAttempts = 5
	DefaultBackoff     = time.Second
	DefaultMaxBackoff  = 30 * time.Second
	DefaultTimeout     = 10 * time.Second
)

// Header names, identical to the Python forwarder.
const (
	SignatureHeader = "X-MC-Agent-Signature"
	TimestampHeader = "X-MC-Agent-Timestamp"
	EventIDHeader   = "X-MC-Agent-Event-Id"
	EventTypeHeader = "X-MC-Agent-Event-Type"
	AttemptHeader   = "X-MC-Agent-Attempt"
)

// Environment variable names, identical to the Python forwarder.
const (
	EnvURL         = "MC_AGENT_WEBHOOK_URL"
	EnvSecret      = "MC_AGENT_WEBHOOK_SECRET"
	EnvEvents      = "MC_AGENT_WEBHOOK_EVENTS"
	EnvQueue       = "MC_AGENT_WEBHOOK_QUEUE"
	EnvMaxAttempts = "MC_AGENT_WEBHOOK_MAX_ATTEMPTS"
	EnvBackoff     = "MC_AGENT_WEBHOOK_BACKOFF"
	EnvMaxBackoff  = "MC_AGENT_WEBHOOK_MAX_BACKOFF"
	EnvTimeout     = "MC_AGENT_WEBHOOK_TIMEOUT"
	EnvConfig      = "MC_AGENT_WEBHOOK_CONFIG"
)

// Config is one forwarder configuration. The secret never appears in String().
type Config struct {
	URL         string
	Secret      string
	Events      []string
	QueueSize   int
	MaxAttempts int
	Backoff     time.Duration
	MaxBackoff  time.Duration
	Timeout     time.Duration
}

// DefaultEvents are forwarded unless the receiver asks for something else.
var DefaultEvents = []string{"chat", "game", "mark", "error"}

// Validate checks the configuration.
func (c *Config) Validate() error {
	parsed, err := url.Parse(c.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("webhook url must be an absolute http(s) url, got %s", RedactURL(c.URL))
	}
	if c.Secret == "" {
		return errors.New("webhook forwarding requires a shared secret for HMAC-SHA256 signing")
	}
	if c.QueueSize < 1 {
		return errors.New("webhook queue size must be at least 1")
	}
	if c.MaxAttempts < 1 {
		return errors.New("webhook max attempts must be at least 1")
	}
	if c.Backoff < 0 {
		return errors.New("webhook backoff cannot be negative")
	}
	if c.MaxBackoff < c.Backoff {
		return errors.New("webhook max backoff cannot be smaller than the base backoff")
	}
	if c.Timeout <= 0 {
		return errors.New("webhook timeout must be positive")
	}
	return nil
}

// RedactedURL keeps only scheme and host so receiver tokens in paths and query
// strings never reach a log or a status payload.
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// LoadConfig merges a config file, the environment and explicit overrides.
// Later sources win; the secret is never echoed.
func LoadConfig(configPath string, events []string, queueSize, maxAttempts int,
	backoff, maxBackoff, timeout time.Duration) (*Config, error) {
	config := &Config{Events: DefaultEvents, QueueSize: DefaultQueueSize,
		MaxAttempts: DefaultMaxAttempts, Backoff: DefaultBackoff,
		MaxBackoff: DefaultMaxBackoff, Timeout: DefaultTimeout}
	if configPath == "" {
		configPath = os.Getenv(EnvConfig)
	}
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("webhook config file not found: %s", configPath)
		}
		var file struct {
			URL         string   `json:"url"`
			Secret      string   `json:"secret"`
			Events      []string `json:"events"`
			QueueSize   int      `json:"queueSize"`
			MaxAttempts int      `json:"maxAttempts"`
			Backoff     float64  `json:"backoff"`
			MaxBackoff  float64  `json:"maxBackoff"`
			Timeout     float64  `json:"timeout"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, errors.New("webhook config file is not valid JSON")
		}
		if file.URL != "" {
			config.URL = file.URL
		}
		if file.Secret != "" {
			config.Secret = file.Secret
		}
		if len(file.Events) > 0 {
			config.Events = file.Events
		}
		if file.QueueSize > 0 {
			config.QueueSize = file.QueueSize
		}
		if file.MaxAttempts > 0 {
			config.MaxAttempts = file.MaxAttempts
		}
		if file.Backoff > 0 {
			config.Backoff = time.Duration(file.Backoff * float64(time.Second))
		}
		if file.MaxBackoff > 0 {
			config.MaxBackoff = time.Duration(file.MaxBackoff * float64(time.Second))
		}
		if file.Timeout > 0 {
			config.Timeout = time.Duration(file.Timeout * float64(time.Second))
		}
	}
	if value := os.Getenv(EnvURL); value != "" {
		config.URL = value
	}
	if value := os.Getenv(EnvSecret); value != "" {
		config.Secret = value
	}
	if value := os.Getenv(EnvEvents); value != "" {
		config.Events = splitList(value)
	}
	if value := os.Getenv(EnvQueue); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			config.QueueSize = parsed
		}
	}
	if value := os.Getenv(EnvMaxAttempts); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			config.MaxAttempts = parsed
		}
	}
	if value := os.Getenv(EnvBackoff); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			config.Backoff = time.Duration(parsed * float64(time.Second))
		}
	}
	if value := os.Getenv(EnvMaxBackoff); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			config.MaxBackoff = time.Duration(parsed * float64(time.Second))
		}
	}
	if value := os.Getenv(EnvTimeout); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			config.Timeout = time.Duration(parsed * float64(time.Second))
		}
	}
	if len(events) > 0 {
		config.Events = events
	}
	if queueSize > 0 {
		config.QueueSize = queueSize
	}
	if maxAttempts > 0 {
		config.MaxAttempts = maxAttempts
	}
	if backoff > 0 {
		config.Backoff = backoff
	}
	if maxBackoff > 0 {
		config.MaxBackoff = maxBackoff
	}
	if timeout > 0 {
		config.Timeout = timeout
	}
	if config.URL == "" {
		return nil, errors.New("webhook forwarding needs a url (--webhook-url, MC_AGENT_WEBHOOK_URL or a config file)")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// Sign is the receiver-neutral signature: sha256=hex(HMAC(key=secret, msg=t.body)).
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// BuildEventBody produces the stable receiver-facing view, keeping the raw
// event verbatim under "data" exactly like the Python forwarder.
func BuildEventBody(event map[string]any) map[string]any {
	eventID := stringField(event, "eventId")
	if eventID == "" {
		streamID := stringField(event, "streamId")
		if streamID == "" {
			streamID = "mc-bridge"
		}
		suffix := "unknown"
		if seq, ok := event["seq"]; ok {
			suffix = fmt.Sprint(seq)
		}
		eventID = streamID + ":" + suffix
	}
	category := stringField(event, "category")
	if category == "" {
		category = "other"
	}
	eventType := stringField(event, "type")
	if eventType == "" {
		eventType = category
	}
	timestamp := int64(0)
	switch typed := event["receivedAt"].(type) {
	case float64:
		timestamp = int64(typed)
	case int64:
		timestamp = typed
	case int:
		timestamp = int64(typed)
	}
	if timestamp == 0 {
		timestamp = time.Now().UnixMilli()
	}
	return map[string]any{
		"eventId":    eventID,
		"sequence":   event["seq"],
		"streamId":   event["streamId"],
		"event":      category,
		"type":       eventType,
		"category":   category,
		"timestamp":  timestamp,
		"tick":       event["tick"],
		"sender":     extractSender(event),
		"context_id": extractContextID(event),
		"data":       event,
	}
}

func extractSender(event map[string]any) any {
	for _, key := range []string{"sender", "player", "playerName", "name", "from"} {
		value, ok := event[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		case map[string]any:
			if nested, ok := typed["name"]; ok && nested != "" {
				return nested
			}
			if nested, ok := typed["id"]; ok && nested != "" {
				return nested
			}
		}
	}
	return nil
}

func extractContextID(event map[string]any) any {
	for _, key := range []string{"context_id", "contextId", "context"} {
		value, ok := event[key]
		if !ok {
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			for _, nestedKey := range []string{"id", "context_id", "contextId"} {
				if candidate, ok := nested[nestedKey]; ok && candidate != "" {
					return candidate
				}
			}
			continue
		}
		if value != nil && value != "" {
			return value
		}
	}
	return nil
}

func stringField(event map[string]any, key string) string {
	if value, ok := event[key].(string); ok {
		return value
	}
	return ""
}

// Stats reports forwarder counters for status payloads.
type Stats struct {
	Queued    int    `json:"queued"`
	Delivered int    `json:"delivered"`
	Dropped   int    `json:"dropped"`
	Retries   int    `json:"retries"`
	Failed    int    `json:"failed"`
	LastError string `json:"lastError,omitempty"`
	LastEvent string `json:"lastEvent,omitempty"`
}

// Logger is the minimal logging surface the forwarder needs; the daemon passes
// a stderr printer so diagnostics never mix with machine stdout.
type Logger func(format string, args ...any)

// Forwarder is the bounded, in-memory delivery queue.
type Forwarder struct {
	config *Config
	logger Logger
	client *http.Client
	queue  chan map[string]any

	mu    sync.Mutex
	stats Stats
	stop  chan struct{}
	once  sync.Once
}

// NewForwarder builds a forwarder for a validated config.
func NewForwarder(config *Config, logger Logger) (*Forwarder, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Forwarder{
		config: config,
		logger: logger,
		client: &http.Client{Timeout: config.Timeout},
		queue:  make(chan map[string]any, config.QueueSize),
		stop:   make(chan struct{}),
	}, nil
}

// Enqueue schedules one event for delivery; it never blocks the daemon.
func (f *Forwarder) Enqueue(event map[string]any) {
	category, _ := event["category"].(string)
	if !f.accepts(category) {
		return
	}
	select {
	case f.queue <- event:
		f.mu.Lock()
		f.stats.Queued++
		f.mu.Unlock()
	default:
		f.mu.Lock()
		f.stats.Dropped++
		f.mu.Unlock()
	}
}

func (f *Forwarder) accepts(category string) bool {
	for _, allowed := range f.config.Events {
		if allowed == category || allowed == "*" {
			return true
		}
	}
	return false
}

// Run delivers queued events until the context ends.
func (f *Forwarder) Run(ctx context.Context) {
	for {
		select {
		case event := <-f.queue:
			f.deliver(ctx, event)
		case <-ctx.Done():
			return
		case <-f.stop:
			return
		}
	}
}

// Stop ends the delivery loop.
func (f *Forwarder) Stop() {
	f.once.Do(func() { close(f.stop) })
}

// Status reports a copy of the counters.
func (f *Forwarder) Status() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *Forwarder) deliver(ctx context.Context, event map[string]any) {
	body, err := json.Marshal(BuildEventBody(event))
	if err != nil {
		return
	}
	eventID := "unknown"
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) == nil {
		eventID = stringField(parsed, "eventId")
	}
	eventType := stringField(event, "type")
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		attempt++
		timestamp := time.Now().Unix()
		status, failure := f.post(ctx, body, eventID, eventType, timestamp, attempt)
		if failure == "" {
			f.mu.Lock()
			f.stats.Delivered++
			f.stats.LastEvent = eventID
			f.mu.Unlock()
			return
		}
		permanent := status >= 400 && status < 500 && status != 408 && status != 425 && status != 429
		if permanent || attempt >= f.config.MaxAttempts {
			f.mu.Lock()
			f.stats.Failed++
			f.stats.LastError = failure
			f.mu.Unlock()
			f.logger("webhook delivery of %s failed permanently after %d attempt(s): %s",
				eventID, attempt, failure)
			return
		}
		f.mu.Lock()
		f.stats.Retries++
		f.stats.LastError = failure
		f.mu.Unlock()
		delay := f.backoff(attempt)
		f.logger("webhook delivery of %s failed (%s); retrying in %s (attempt %d/%d)",
			eventID, failure, delay, attempt+1, f.config.MaxAttempts)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		case <-f.stop:
			return
		}
	}
}

func (f *Forwarder) post(ctx context.Context, body []byte, eventID, eventType string,
	timestamp int64, attempt int) (int, string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.config.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(SignatureHeader, Sign(f.config.Secret, timestamp, body))
	request.Header.Set(TimestampHeader, strconv.FormatInt(timestamp, 10))
	request.Header.Set(EventIDHeader, eventID)
	if eventType != "" {
		request.Header.Set(EventTypeHeader, eventType)
	}
	request.Header.Set(AttemptHeader, strconv.Itoa(attempt))
	response, err := f.client.Do(request)
	if err != nil {
		return 0, sanitize(err.Error(), f.config)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response.StatusCode, ""
	}
	return response.StatusCode, fmt.Sprintf("receiver answered HTTP %d", response.StatusCode)
}

func (f *Forwarder) backoff(attempt int) time.Duration {
	delay := f.config.Backoff
	for index := 1; index < attempt; index++ {
		delay *= 2
		if delay >= f.config.MaxBackoff {
			return f.config.MaxBackoff
		}
	}
	if delay > f.config.MaxBackoff {
		return f.config.MaxBackoff
	}
	return delay
}

func sanitize(text string, config *Config) string {
	if config.URL != "" {
		text = strings.ReplaceAll(text, config.URL, RedactURL(config.URL))
	}
	if config.Secret != "" {
		text = strings.ReplaceAll(text, config.Secret, "[redacted]")
	}
	return text
}
