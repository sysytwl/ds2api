package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// chatRequest is the OpenAI-compatible request subset the bridge supports.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text flattens string or multimodal content into plain text.
func (m chatMessage) text() string {
	raw := strings.TrimSpace(string(m.Content))
	if raw == "" || raw == "null" {
		return ""
	}
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			return s
		}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Text != "" {
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	return raw
}

// buildPrompt turns an OpenAI message list into the single prompt the ELM web
// chat accepts.
func buildPrompt(messages []chatMessage) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("messages is empty")
	}
	if len(messages) == 1 && strings.EqualFold(messages[0].Role, "user") {
		text := strings.TrimSpace(messages[0].text())
		if text == "" {
			return "", fmt.Errorf("message content is empty")
		}
		return text, nil
	}
	var b strings.Builder
	for _, m := range messages {
		text := strings.TrimSpace(m.text())
		if text == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			b.WriteString("System: ")
		case "assistant":
			b.WriteString("Assistant: ")
		default:
			b.WriteString("User: ")
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	prompt := strings.TrimSpace(b.String())
	if prompt == "" {
		return "", fmt.Errorf("messages contain no text")
	}
	return prompt, nil
}

func newCompletionID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "chatcmpl-fallback"
	}
	return "chatcmpl-" + hex.EncodeToString(buf[:])
}

// ssePayload writes one Server-Sent Event JSON payload.
func ssePayload(w interface{ Write([]byte) (int, error) }, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	return nil
}

func chunkPayload(id, model string, created int64, delta map[string]any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{"index": 0, "delta": delta}},
	}
}

func finishChunkPayload(id, model string, created int64, reason string) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
	}
}

func completionPayload(id, model, content, reasoning string) map[string]any {
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": timeNowUnix(),
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}

func errorPayload(message, code string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error", "code": code}}
}
