package elmweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"ds2api/internal/sse"
)

func TestStompFrameRoundTrip(t *testing.T) {
	raw := encodeStompFrame("SEND", []stompHeader{
		{Key: "destination", Value: "/app/queue/query-abc"},
		{Key: "content-type", Value: "application/json"},
	}, `{"query":"hi"}`)
	frame, ok, err := parseStompFrame(raw)
	if err != nil || !ok {
		t.Fatalf("parse frame: ok=%v err=%v", ok, err)
	}
	if frame.Command != "SEND" {
		t.Fatalf("command = %q", frame.Command)
	}
	if got := frame.Header("destination"); got != "/app/queue/query-abc" {
		t.Fatalf("destination = %q", got)
	}
	if frame.Body != `{"query":"hi"}` {
		t.Fatalf("body = %q", frame.Body)
	}
	if !strings.HasSuffix(raw, "\x00") {
		t.Fatal("frame must be NUL terminated")
	}
}

func TestStompHeartbeat(t *testing.T) {
	for _, raw := range []string{"", "\n", "\x00"} {
		if _, ok, err := parseStompFrame(raw); ok || err != nil {
			t.Fatalf("heartbeat %q: ok=%v err=%v", raw, ok, err)
		}
	}
}

func TestChatEventClassification(t *testing.T) {
	cases := []struct {
		body     string
		finished bool
		thinking bool
		user     bool
		errEvent bool
		reply    string
	}{
		{body: `{"reply":"hi","replyType":"USER"}`, user: true, reply: "hi"},
		{body: `{"reply":"hello","replyType":"AI"}`, reply: "hello"},
		{body: `{"reply":"","replyType":"CHAT_FINISHED"}`, finished: true},
		{body: `{"reply":"thinking","replyType":"THINKING"}`, thinking: true, reply: "thinking"},
		{body: `{"reply":"","replyType":"TIMEOUT"}`, errEvent: true},
	}
	for _, tc := range cases {
		ev, err := parseChatEvent(tc.body)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.body, err)
		}
		if got := isFinishedEvent(ev); got != tc.finished {
			t.Fatalf("%q finished = %v", tc.body, got)
		}
		if got := isThinkingEvent(ev); got != tc.thinking {
			t.Fatalf("%q thinking = %v", tc.body, got)
		}
		if got := isUserEvent(ev); got != tc.user {
			t.Fatalf("%q user = %v", tc.body, got)
		}
		if got := isErrorEvent(ev); got != tc.errEvent {
			t.Fatalf("%q error = %v", tc.body, got)
		}
		if ev.Reply != tc.reply {
			t.Fatalf("%q reply = %q", tc.body, ev.Reply)
		}
	}
}

func TestWriteDeepSeekSSEIsReadableByPipeline(t *testing.T) {
	var buf bytes.Buffer
	if err := writeDeepSeekSSE(&buf, "response/content", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := writeDeepSeekSSE(&buf, "response/status", "FINISHED"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(&buf, "data: [DONE]\n\n")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	content := ""
	finished := false
	done := false
	for _, line := range lines {
		if line == "" {
			continue
		}
		chunk, isDone, ok := sse.ParseDeepSeekSSELine([]byte(line))
		if !ok {
			t.Fatalf("unparseable line %q", line)
		}
		if isDone {
			done = true
			continue
		}
		parts, isFinished, _ := sse.ParseSSEChunkForContent(chunk, false, "text")
		finished = finished || isFinished
		for _, p := range parts {
			content += p.Text
		}
	}
	if content != "hello" {
		t.Fatalf("content = %q", content)
	}
	if !finished {
		t.Fatal("expected FINISHED status to stop the stream")
	}
	if !done {
		t.Fatal("expected [DONE] terminator")
	}
}

func TestWebSocketFrameRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	writer := &wsConn{conn: client}
	reader := &wsConn{conn: server, br: bufio.NewReader(server)}

	done := make(chan error, 1)
	go func() {
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		done <- writer.WriteMessage(`data: {"p":"response/content","v":"ok"}`)
	}()
	_ = server.SetDeadline(time.Now().Add(2 * time.Second))
	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if msg != `data: {"p":"response/content","v":"ok"}` {
		t.Fatalf("message = %q", msg)
	}
	if err := <-done; err != nil {
		t.Fatalf("write message: %v", err)
	}
}

func TestParseChatEventJSONNumbers(t *testing.T) {
	ev, err := parseChatEvent(`{"reply":"done","replyType":"AI","tokensUsed":906,"modelId":"Qwen/Qwen3.5-397B-A17B-FP8"}`)
	if err != nil {
		t.Fatal(err)
	}
	if ev.TokensUsed == nil || *ev.TokensUsed != 906 {
		t.Fatalf("tokensUsed = %v", ev.TokensUsed)
	}
	if ev.ModelID != "Qwen/Qwen3.5-397B-A17B-FP8" {
		t.Fatalf("modelId = %q", ev.ModelID)
	}
	raw, _ := json.Marshal(ev)
	if !bytes.Contains(raw, []byte("CHAT")) && ev.ReplyType != "AI" {
		t.Fatalf("unexpected marshal %s", raw)
	}
}
