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
	Model      string          `json:"model"`
	Messages   []chatMessage   `json:"messages"`
	Tools      json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Stream     bool            `json:"stream"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

// chatToolCall is an assistant tool call echoed back by the client.
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
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

// promptOptions carries the pieces that are rendered ahead of the conversation.
type promptOptions struct {
	Instructions string
	Tools        []toolSpec
	Choice       string
	Forced       string
}

// buildPrompt turns an OpenAI message list into the single prompt the ELM web
// chat accepts.
func buildPrompt(messages []chatMessage) (string, error) {
	return buildPromptWithOptions(messages, promptOptions{})
}

func buildPromptWithOptions(messages []chatMessage, opt promptOptions) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("messages is empty")
	}
	simple := len(messages) == 1 && strings.EqualFold(messages[0].Role, "user") &&
		len(messages[0].ToolCalls) == 0 && strings.TrimSpace(opt.Instructions) == "" && len(opt.Tools) == 0
	if simple {
		text := strings.TrimSpace(messages[0].text())
		if text == "" {
			return "", fmt.Errorf("message content is empty")
		}
		return text, nil
	}
	var b strings.Builder
	if instructions := strings.TrimSpace(opt.Instructions); instructions != "" {
		b.WriteString("System: ")
		b.WriteString(instructions)
		b.WriteString("\n\n")
	}
	if len(opt.Tools) > 0 && opt.Choice != "none" {
		b.WriteString(toolInstructions(opt.Tools, opt.Choice, opt.Forced))
		b.WriteString("\n")
	}
	for _, m := range messages {
		line := m.render()
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n\n")
	}
	prompt := strings.TrimSpace(b.String())
	if prompt == "" {
		return "", fmt.Errorf("messages contain no text")
	}
	return prompt, nil
}

// render flattens one message into the transcript the web chat model sees.
func (m chatMessage) render() string {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	text := strings.TrimSpace(m.text())
	switch role {
	case "system", "developer":
		if text == "" {
			return ""
		}
		return "System: " + text
	case "assistant":
		parts := make([]string, 0, 2)
		if text != "" {
			parts = append(parts, text)
		}
		if calls := m.callsText(); calls != "" {
			parts = append(parts, calls)
		}
		if len(parts) == 0 {
			return ""
		}
		return "Assistant: " + strings.Join(parts, " ")
	case "tool":
		if text == "" {
			return ""
		}
		label := strings.TrimSpace(m.Name)
		if label == "" {
			label = strings.TrimSpace(m.ToolCallID)
		}
		return "Tool result (" + label + "): " + text
	default:
		if text == "" {
			return ""
		}
		return "User: " + text
	}
}

func (m chatMessage) callsText() string {
	if len(m.ToolCalls) == 0 {
		return ""
	}
	out := make([]string, 0, len(m.ToolCalls))
	for _, call := range m.ToolCalls {
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		out = append(out, fmt.Sprintf("[tool call %s(%s)]", call.Function.Name, args))
	}
	return strings.Join(out, " ")
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

// toolCompletionPayload is a non-streaming reply that asks the client to run
// one or more tools instead of answering with text.
func toolCompletionPayload(id, model string, calls []invokedTool, content string) map[string]any {
	message := map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCallPayload(nil, calls)}
	if strings.TrimSpace(content) != "" {
		message["content"] = content
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": timeNowUnix(),
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": "tool_calls"}},
		"usage":   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}

// streamingToolCalls renders the streamed delta shape (with indexes).
func streamingToolCalls(calls []invokedTool) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for i, call := range calls {
		out = append(out, map[string]any{
			"index": i,
			"id":    newPrefixedID("call"),
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return out
}

func errorPayload(message, code string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error", "code": code}}
}
