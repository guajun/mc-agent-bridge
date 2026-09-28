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
	mu   sync.Mutex
	buf  bytes.Buffer
	gate chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	if gate != nil {
		<-gate
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stall blocks the next Write until release, so a test can overflow the live
// queue while the consumer is stuck on stdout.
func (b *syncBuffer) stall() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gate == nil {
		b.gate = make(chan struct{})
	}
}

func (b *syncBuffer) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gate != nil {
		close(b.gate)
		b.gate = nil
	}
}

// scriptedDaemon serves the subset of the local API that events --follow
// needs: a replay ring with paging/filtering plus paced live broadcasts.
type scriptedDaemon struct {
	mu         sync.Mutex
	events     []map[string]any
	stream     string
	ringLimit  int
	next       int64
	delayFirst bool
	firstCall  chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (s *scriptedDaemon) append(category, target, text string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	event := map[string]any{
		"seq":        s.next,
		"streamId":   s.stream,
		"eventId":    fmt.Sprintf("%s:%d", s.stream, s.next),
		"category":   category,
		"target":     target,
		"type":       map[string]string{"mark": "mark", "game": "game"}[category],
		"text":       text,
		"receivedAt": time.Now().UnixMilli(),
	}
	s.events = append(s.events, event)
	if s.ringLimit > 0 && len(s.events) > s.ringLimit {
		s.events = s.events[len(s.events)-s.ringLimit:]
	}
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

func waitForOutput(t *testing.T, output *syncBuffer, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("output never contained %q:\n%s", needle, output.String())
}

// After the replay handoff, a consumer stuck on stdout must not lose live
// events silently: the overflow is reported and the recoverable range is
// replayed from the last actually delivered sequence, exactly once.
func TestFollowRecoversLiveOverflowAfterHandoff(t *testing.T) {
	scripted := &scriptedDaemon{stream: "cli-stream-live"}
	application, server := startScriptedCLI(t, scripted)
	defer server.Stop()
	output := application.stdout.(*syncBuffer)
	for index := 0; index < 30; index++ {
		scripted.append("mark", "scripted", fmt.Sprintf("pre-%d", index))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = application.cmdEvents(ctx, []string{"--follow", "--since", "0", "--limit", "50",
			"--category", "mark"})
		close(done)
	}()
	waitForOutput(t, output, `"event":"following"`, 10*time.Second)
	output.stall()
	for index := 0; index < 400; index++ {
		event := scripted.append("mark", "scripted", fmt.Sprintf("flood-%d", index))
		server.Broadcast("mark", event)
		if index%25 == 24 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	for index := 0; index < 20; index++ {
		server.Broadcast("game", scripted.append("game", "scripted", fmt.Sprintf("g-%d", index)))
	}
	time.Sleep(300 * time.Millisecond)
	output.release()
	waitForOutput(t, output, "flood-399", 20*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow command did not stop after cancellation")
	}
	text := output.String()
	if !strings.Contains(text, "clientDropped") {
		t.Fatalf("live overflow was not reported as a gap:\n%s", text)
	}
	texts := parsedTexts(text)
	for index := 0; index < 30; index++ {
		needle := fmt.Sprintf("pre-%d", index)
		if texts[needle] != 1 {
			t.Fatalf("replay event %s appeared %d times", needle, texts[needle])
		}
	}
	for index := 0; index < 400; index++ {
		needle := fmt.Sprintf("flood-%d", index)
		if texts[needle] != 1 {
			t.Fatalf("live event %s appeared %d times", needle, texts[needle])
		}
	}
	for index := 0; index < 20; index++ {
		if needle := fmt.Sprintf("g-%d", index); texts[needle] != 0 {
			t.Fatalf("game event %s leaked into a mark-only follow", needle)
		}
	}
}

// When the daemon ring expired past the overflow, the follow stream must say
// so explicitly (a daemon gap marker, not only the client drop marker) and
// print exactly the still-recoverable events.
func TestFollowReportsLiveOverflowWhenRingExpired(t *testing.T) {
	scripted := &scriptedDaemon{stream: "cli-stream-expired", ringLimit: 40}
	application, server := startScriptedCLI(t, scripted)
	defer server.Stop()
	output := application.stdout.(*syncBuffer)
	for index := 0; index < 20; index++ {
		scripted.append("mark", "scripted", fmt.Sprintf("pre-%d", index))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = application.cmdEvents(ctx, []string{"--follow", "--since", "0", "--limit", "50",
			"--category", "mark"})
		close(done)
	}()
	waitForOutput(t, output, `"event":"following"`, 10*time.Second)
	output.stall()
	for index := 0; index < 300; index++ {
		event := scripted.append("mark", "scripted", fmt.Sprintf("flood-%d", index))
		server.Broadcast("mark", event)
		if index%25 == 24 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	time.Sleep(300 * time.Millisecond)
	output.release()
	// The ring keeps seq 281..320, that is flood-260..flood-299.
	waitForOutput(t, output, "flood-299", 20*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow command did not stop after cancellation")
	}
	text := output.String()
	if !strings.Contains(text, "clientDropped") {
		t.Fatalf("live overflow was not reported as a gap:\n%s", text)
	}
	daemonGap := false
	for _, line := range strings.Split(text, "\n") {
		var value map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &value) != nil {
			continue
		}
		if value["event"] != "gap" {
			continue
		}
		data, _ := value["data"].(map[string]any)
		if data["dropped"] == true && data["clientDropped"] == nil {
			daemonGap = true
		}
	}
	if !daemonGap {
		t.Fatalf("ring eviction was not reported as an explicit daemon gap:\n%s", text)
	}
	texts := parsedTexts(text)
	for index := 0; index < 20; index++ {
		needle := fmt.Sprintf("pre-%d", index)
		if texts[needle] != 1 {
			t.Fatalf("replay event %s appeared %d times", needle, texts[needle])
		}
	}
	for index := 260; index <= 299; index++ {
		needle := fmt.Sprintf("flood-%d", index)
		if texts[needle] != 1 {
			t.Fatalf("recoverable event %s appeared %d times", needle, texts[needle])
		}
	}
	// flood-0 was delivered live before the queue filled (it was the event
	// the stalled writer was holding); everything between it and the ring was
	// evicted and must not be printed as if recovered.
	if texts["flood-0"] != 1 {
		t.Fatalf("the pre-gap live event appeared %d times", texts["flood-0"])
	}
	for index := 1; index < 260; index++ {
		if needle := fmt.Sprintf("flood-%d", index); texts[needle] != 0 {
			t.Fatalf("evicted event %s was printed as if recovered", needle)
		}
	}
}
