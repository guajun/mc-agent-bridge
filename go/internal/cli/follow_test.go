package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/config"
	"github.com/guajun/mc-agent-bridge/internal/ipc"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// scriptedDaemon serves the subset of the local API that events --follow
// needs: a replay ring with paging/filtering plus paced live broadcasts.
type scriptedDaemon struct {
	mu         sync.Mutex
	events     []map[string]any
	stream     string
	delayFirst bool
	firstCall  chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (s *scriptedDaemon) append(category, target, text string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	event := map[string]any{
		"seq":        int64(len(s.events) + 1),
		"streamId":   s.stream,
		"eventId":    fmt.Sprintf("%s:%d", s.stream, len(s.events)+1),
		"category":   category,
		"target":     target,
		"type":       map[string]string{"mark": "mark", "game": "game"}[category],
		"text":       text,
		"receivedAt": time.Now().UnixMilli(),
	}
	s.events = append(s.events, event)
	return event
}

func (s *scriptedDaemon) handler(_ context.Context, method string, params map[string]any) (any, *protocol.Error) {
	if method != "events" {
		return map[string]any{"ok": true}, nil
	}
	s.mu.Lock()
	delay := s.delayFirst
	events := append([]map[string]any(nil), s.events...)
	s.mu.Unlock()
	if delay {
		s.once.Do(func() { close(s.firstCall) })
		<-s.release
	}
	since := jsonInt(params["since"])
	limit := int(jsonInt(params["limit"]))
	if limit <= 0 {
		limit = 200
	}
	category := stringValue(params["category"])
	target := stringValue(params["target"])
	var filtered []map[string]any
	for _, event := range events {
		if jsonInt(event["seq"]) <= since {
			continue
		}
		if category != "" && event["category"] != category {
			continue
		}
		if target != "" && event["target"] != target {
			continue
		}
		filtered = append(filtered, event)
	}
	truncated := len(filtered) > limit
	if truncated {
		filtered = filtered[:limit]
	}
	next := since
	if len(filtered) > 0 {
		next = jsonInt(filtered[len(filtered)-1]["seq"])
	}
	older := int64(0)
	if len(events) > 0 {
		older = jsonInt(events[0]["seq"])
	}
	dropped := since > 0 && older > 0 && older > since+1
	return map[string]any{
		"events": filtered, "next": next, "truncated": truncated, "dropped": dropped,
		"streamId": s.stream, "reset": false, "lastSeq": len(events),
	}, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func jsonInt(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	}
	return 0
}

func startScriptedCLI(t *testing.T, scripted *scriptedDaemon) (*app, *ipc.Server) {
	t.Helper()
	server, err := ipc.Start("127.0.0.1:0", "cli-token", scripted.handler)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := config.SaveDaemonState(home, &config.DaemonState{
		Address: server.Address(), Token: "cli-token", Version: "test",
	}); err != nil {
		t.Fatal(err)
	}
	return &app{home: home, stdout: &syncBuffer{}, stderr: &syncBuffer{}}, server
}

// The real CLI follow path must page the whole replay, recover events that
// overflowed the bounded live queue while the replay reply was pending, and
// avoid false gap markers for a filtered stream.
func TestFollowRecoversOverflowThroughReplay(t *testing.T) {
	scripted := &scriptedDaemon{stream: "cli-stream-1", delayFirst: true,
		firstCall: make(chan struct{}), release: make(chan struct{})}
	application, server := startScriptedCLI(t, scripted)
	defer server.Stop()
	for index := 0; index < 25; index++ {
		scripted.append("mark", "scripted", fmt.Sprintf("m-%d", index))
	}
	for index := 0; index < 3; index++ {
		scripted.append("game", "scripted", fmt.Sprintf("g-%d", index))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = application.cmdEvents(ctx, []string{"--follow", "--since", "0", "--limit", "10",
			"--category", "mark"})
		close(done)
	}()
	select {
	case <-scripted.firstCall:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow replay call never reached the daemon")
	}
	// More live events than the IPC client queue while the replay reply is
	// still blocked: the old blocking read loop deadlocked here.
	for index := 0; index < 300; index++ {
		scripted.append("mark", "scripted", fmt.Sprintf("live-%d", index))
		server.Broadcast("mark", scripted.events[len(scripted.events)-1])
		if index%25 == 24 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	close(scripted.release)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(application.stdout.(*syncBuffer).String(), `"event":"following"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow command did not stop after cancellation")
	}
	output := application.stdout.(*syncBuffer).String()
	if !strings.Contains(output, `"event":"following"`) {
		t.Fatalf("the follow stream never reached live mode:\n%s", output)
	}
	if !strings.Contains(output, "clientDropped") {
		t.Fatalf("the live-queue overflow was not reported as a gap:\n%s", output)
	}
	if strings.Contains(output, "g-0") || strings.Contains(output, "g-1") {
		t.Fatalf("a game event reached a mark-only follow:\n%s", output)
	}
	texts := parsedTexts(output)
	for index := 0; index < 25; index++ {
		needle := fmt.Sprintf("m-%d", index)
		if texts[needle] != 1 {
			t.Fatalf("replay event %s appeared %d times", needle, texts[needle])
		}
	}
	for index := 0; index < 300; index++ {
		needle := fmt.Sprintf("live-%d", index)
		if texts[needle] != 1 {
			t.Fatalf("overflowed live event %s appeared %d times", needle, texts[needle])
		}
	}
}

// parsedTexts counts the exact "text" field of every JSON event printed by the
// follow stream.
func parsedTexts(output string) map[string]int {
	counts := map[string]int{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var value map[string]any
		if json.Unmarshal([]byte(line), &value) != nil {
			continue
		}
		if text, ok := value["text"].(string); ok {
			counts[text]++
		}
	}
	return counts
}

// Without overflow the follow path pages everything exactly once and emits no
// gap marker.
func TestFollowPagesWholeReplayWithoutFalseGaps(t *testing.T) {
	scripted := &scriptedDaemon{stream: "cli-stream-2", firstCall: make(chan struct{})}
	application, server := startScriptedCLI(t, scripted)
	defer server.Stop()
	for index := 0; index < 25; index++ {
		scripted.append("mark", "scripted", fmt.Sprintf("page-%d", index))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = application.cmdEvents(ctx, []string{"--follow", "--since", "0", "--limit", "10"})
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(application.stdout.(*syncBuffer).String(), `"event":"following"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow command did not stop after cancellation")
	}
	output := application.stdout.(*syncBuffer).String()
	texts := parsedTexts(output)
	for index := 0; index < 25; index++ {
		needle := fmt.Sprintf("page-%d", index)
		if texts[needle] != 1 {
			t.Fatalf("replay event %s appeared %d times", needle, texts[needle])
		}
	}
	if strings.Contains(output, `"event":"gap"`) {
		t.Fatalf("a clean filtered replay produced a false gap marker:\n%s", output)
	}
}
