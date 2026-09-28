package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignatureMatchesPythonFormula(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"eventId":"abc:1"}`)
	timestamp := int64(1700000000)
	expected := hmac.New(sha256.New, []byte(secret))
	expected.Write([]byte("1700000000."))
	expected.Write(body)
	want := "sha256=" + hex.EncodeToString(expected.Sum(nil))
	if got := Sign(secret, timestamp, body); got != want {
		t.Fatalf("Sign() = %s, want %s", got, want)
	}
}

func TestEventBodyShape(t *testing.T) {
	event := map[string]any{
		"eventId":    "stream:7",
		"seq":        int64(7),
		"streamId":   "stream",
		"type":       "chat",
		"category":   "chat",
		"receivedAt": int64(1700000000000),
		"tick":       int64(42),
		"sender":     "Alice",
		"contextId":  "ctx-1",
	}
	body := BuildEventBody(event)
	if body["eventId"] != "stream:7" || body["category"] != "chat" {
		t.Fatalf("body = %v", body)
	}
	if body["sender"] != "Alice" || body["context_id"] != "ctx-1" {
		t.Fatalf("sender/context = %v", body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok || data["tick"] != int64(42) {
		t.Fatalf("raw event was not preserved: %v", body["data"])
	}
}

func TestForwarderDeliversAndRetries(t *testing.T) {
	var attempts atomic.Int32
	received := make(chan *http.Request, 4)
	bodies := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempt := attempts.Add(1)
		payload, _ := io.ReadAll(request.Body)
		if attempt == 1 {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		received <- request
		bodies <- payload
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := &Config{URL: server.URL, Secret: "secret", Events: []string{"chat"},
		QueueSize: 4, MaxAttempts: 3, Backoff: 10 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond, Timeout: time.Second}
	forwarder, err := NewForwarder(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go forwarder.Run(ctx)
	forwarder.Enqueue(map[string]any{"eventId": "s:1", "type": "chat", "category": "chat",
		"receivedAt": time.Now().UnixMilli()})

	select {
	case request := <-received:
		if request.Header.Get(SignatureHeader) == "" || request.Header.Get(EventIDHeader) != "s:1" {
			t.Fatalf("headers = %v", request.Header)
		}
		if request.Header.Get(AttemptHeader) == "" {
			t.Fatal("attempt header is missing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the retried delivery never arrived")
	}
	select {
	case <-time.After(200 * time.Millisecond):
	case <-received:
		t.Fatal("a third delivery should not happen")
	}
	stats := forwarder.Status()
	if stats.Delivered != 1 || stats.Retries != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	forwarder.Stop()
}

func TestForwarderDoesNotRetryPermanentFailures(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	config := &Config{URL: server.URL, Secret: "secret", Events: []string{"*"},
		QueueSize: 4, MaxAttempts: 5, Backoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond, Timeout: time.Second}
	forwarder, _ := NewForwarder(config, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go forwarder.Run(ctx)
	forwarder.Enqueue(map[string]any{"eventId": "s:2", "type": "game", "category": "game"})
	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
	forwarder.Stop()
}

func TestLoadConfigRejectsUnsafeSettings(t *testing.T) {
	_, err := LoadConfig("", nil, 0, 0, 0, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "url") {
		t.Fatalf("missing URL should fail: %v", err)
	}
	t.Setenv(EnvURL, "http://example.test/hook")
	if _, err := LoadConfig("", nil, 0, 0, 0, 0, 0); err == nil ||
		!strings.Contains(err.Error(), "secret") {
		t.Fatalf("missing secret should fail: %v", err)
	}
	t.Setenv(EnvSecret, "hunter2")
	config, err := LoadConfig("", []string{"chat"}, 0, 0, 100*time.Millisecond, time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if config.Events[0] != "chat" || config.QueueSize != DefaultQueueSize {
		t.Fatalf("config = %+v", config)
	}
	if RedactURL("https://user:pass@example.test/hook?token=abc") != "https://example.test" {
		t.Fatal("redaction leaked URL parts")
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestConfigFileRoundTrip(t *testing.T) {
	path := t.TempDir() + "/webhook.json"
	document := fmt.Sprintf(`{"url":"http://example.test/x","secret":"s","events":["game"],"queueSize":7,"maxAttempts":2,"backoff":0.5,"maxBackoff":3,"timeout":4}`)
	if err := writeTestFile(path, document); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path, nil, 0, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if config.QueueSize != 7 || config.MaxAttempts != 2 || config.Timeout != 4*time.Second {
		raw, _ := json.Marshal(config)
		t.Fatalf("config = %s", raw)
	}
}
