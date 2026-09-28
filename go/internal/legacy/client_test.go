package legacy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/guajun/mc-agent-bridge/internal/legacy"
)

// startLegacyServer emulates the released mod's JSON-lines endpoint.
func startLegacyServer(t *testing.T, capabilities string) (string, chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		writer := bufio.NewWriter(connection)
		writer.WriteString(`{"type":"hello","mod":"mc-agent-interface","version":"0.7.0","protocol":1,` +
			`"minecraft":"26.2","instance":"server","capabilities":` + capabilities + "}\n")
		writer.Flush()
		reader := bufio.NewReader(connection)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			lines <- line
			switch {
			case line == "STATE":
				writer.WriteString(`{"type":"state","instance":"server","levelName":"legacy-world","worldDir":"C:\\legacy","tick":9}` + "\n")
			case line == "CMD say hi":
				// Push an unrelated event first: reply routing must ignore it.
				writer.WriteString(`{"type":"game","event":true,"text":"chat-noise"}` + "\n")
				writer.WriteString(`{"type":"cmd_ack","detail":"say hi","output":["from-ack"]}` + "\n")
			case strings.HasPrefix(line, "CMD "):
				writer.WriteString(`{"type":"cmd_ack","detail":"x"}` + "\n")
			case line == "ENTITIES":
				writer.WriteString(`{"type":"entities","entities":[]}` + "\n")
			default:
				writer.WriteString(`{"type":"error","message":"unknown command: ` + line + `"}` + "\n")
			}
			writer.Flush()
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().String(), lines
}

func TestRequestSkipsEventsAndKeepsReplies(t *testing.T) {
	address, _ := startLegacyServer(t, `["state","command","events:game"]`)
	client, err := legacy.Dial(context.Background(), address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.Instance() != "server" {
		t.Fatalf("instance = %s", client.Instance())
	}
	if len(client.Capabilities()) != 3 {
		t.Fatalf("capabilities = %v", client.Capabilities())
	}
	reply, err := client.Request(context.Background(), "STATE", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reply["type"] != "state" || reply["levelName"] != "legacy-world" {
		t.Fatalf("reply = %v", reply)
	}

	// The command reply comes after an event; the event must not be mistaken
	// for the reply.
	reply, err = client.Request(context.Background(), "CMD say hi", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reply["type"] != "cmd_ack" {
		t.Fatalf("reply = %v", reply)
	}
	select {
	case event := <-client.Events():
		if event["type"] != "game" || event["text"] != "chat-noise" {
			t.Fatalf("event = %v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pushed event was lost")
	}

	// An error reply is still a reply.
	if _, err := client.Request(context.Background(), "NOPE", 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionCloseIsReported(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, _ := listener.Accept()
		if connection != nil {
			connection.Close()
		}
	}()
	_, err = legacy.Dial(context.Background(), listener.Addr().String(), 2*time.Second)
	if err == nil {
		t.Fatal("a closed endpoint must fail the dial")
	}
	payload, _ := json.Marshal(map[string]any{"error": err.Error()})
	if !strings.Contains(string(payload), "hello") {
		t.Fatalf("error = %s", payload)
	}
}
