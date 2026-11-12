package elmcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// stompFrame is one STOMP 1.2 frame carried inside a WebSocket text message.
type stompFrame struct {
	Command string
	Headers []stompHeader
	Body    string
}

type stompHeader struct {
	Key   string
	Value string
}

// ErrStompProtocol reports a frame the client cannot interpret.
var ErrStompProtocol = errors.New("elm web: invalid stomp frame")

func (f stompFrame) Header(key string) string {
	for _, h := range f.Headers {
		if strings.EqualFold(h.Key, key) {
			return h.Value
		}
	}
	return ""
}

func encodeStompFrame(command string, headers []stompHeader, body string) string {
	var b strings.Builder
	b.WriteString(command)
	b.WriteByte('\n')
	for _, h := range headers {
		b.WriteString(h.Key)
		b.WriteByte(':')
		b.WriteString(h.Value)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(body)
	b.WriteByte(0)
	return b.String()
}

// parseStompFrame decodes one WebSocket text message into a STOMP frame.
// ok is false for heartbeat messages (an empty payload or a single newline).
func parseStompFrame(raw string) (frame stompFrame, ok bool, err error) {
	raw = strings.TrimSuffix(raw, "\x00")
	if strings.TrimSpace(raw) == "" {
		return stompFrame{}, false, nil
	}
	head := raw
	body := ""
	if idx := strings.Index(raw, "\n\n"); idx >= 0 {
		head = raw[:idx]
		body = raw[idx+2:]
	}
	lines := strings.Split(head, "\n")
	command := strings.TrimSpace(lines[0])
	if command == "" {
		return stompFrame{}, false, ErrStompProtocol
	}
	frame = stompFrame{Command: command, Body: body}
	for _, line := range lines[1:] {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		frame.Headers = append(frame.Headers, stompHeader{Key: strings.TrimSpace(key), Value: strings.TrimSpace(value)})
	}
	return frame, true, nil
}

// chatEvent is one JSON object from an ELM chat reply topic. ELM streams a
// USER echo, AI delta chunks and a final CHAT_FINISHED frame.
type chatEvent struct {
	ID         string `json:"id"`
	Reply      string `json:"reply"`
	ReplyType  string `json:"replyType"`
	ModelID    string `json:"modelId"`
	TokensUsed *int   `json:"tokensUsed"`
}

func parseChatEvent(body string) (chatEvent, error) {
	var ev chatEvent
	body = strings.TrimSpace(body)
	if body == "" {
		return ev, nil
	}
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		return ev, fmt.Errorf("elm web: decode chat event: %w", err)
	}
	return ev, nil
}

func isFinishedEvent(ev chatEvent) bool {
	kind := strings.ToUpper(strings.TrimSpace(ev.ReplyType))
	return strings.Contains(kind, "FINISHED") || strings.Contains(kind, "CHAT_END")
}

func isThinkingEvent(ev chatEvent) bool {
	kind := strings.ToUpper(strings.TrimSpace(ev.ReplyType))
	return strings.Contains(kind, "THINK") || strings.Contains(kind, "REASON")
}

func isUserEvent(ev chatEvent) bool {
	return strings.EqualFold(strings.TrimSpace(ev.ReplyType), "USER")
}

func isErrorEvent(ev chatEvent) bool {
	kind := strings.ToUpper(strings.TrimSpace(ev.ReplyType))
	return strings.Contains(kind, "ERROR") || strings.Contains(kind, "TIMEOUT") || strings.Contains(kind, "STOPPED")
}
