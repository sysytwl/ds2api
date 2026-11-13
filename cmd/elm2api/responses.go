package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"ds2api/internal/elmcore"
)

// responsesRequest is the subset of the OpenAI Responses API the bridge
// supports. The ELM chat is continued by response id, so the client never has
// to resend the whole history: exactly what the web chat already stores.
type responsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	PreviousResponseID string          `json:"previous_response_id"`
	Store              *bool           `json:"store"`
	Stream             bool            `json:"stream"`
}

// storable reports whether this response may be continued later. store:false is
// not really supported (the ELM web chat always keeps the history) but it is
// honoured as "do not reuse anything": every call starts a new chat.
func (r responsesRequest) storable() bool { return r.Store == nil || *r.Store }

type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Output  json.RawMessage `json:"output"`
	Name    string          `json:"name"`
}

func newResponseID() string { return newPrefixedID("resp") }

func newItemID(prefix string) string { return newPrefixedID(prefix) }

func newPrefixedID(prefix string) string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return prefix + "_fallback"
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

// responsesMessages normalises the Responses input into chat messages.
func responsesMessages(req responsesRequest) ([]chatMessage, error) {
	var msgs []chatMessage
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: mustJSON(instructions)})
	}
	items, err := parseResponseInput(req.Input)
	if err != nil {
		return nil, err
	}
	return append(msgs, items...), nil
}

func parseResponseInput(raw json.RawMessage) ([]chatMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("input is required")
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("input is empty")
		}
		return []chatMessage{{Role: "user", Content: mustJSON(text)}}, nil
	}
	if raw[0] != '[' {
		return nil, fmt.Errorf("input must be a string or an array of items")
	}
	var items []responsesItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]chatMessage, 0, len(items))
	for _, item := range items {
		if msg, ok := item.chatMessage(); ok {
			out = append(out, msg)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("input contains no text")
	}
	return out, nil
}

// chatMessage converts one Responses input item; tool plumbing is not supported
// yet, so unknown item types are skipped instead of failing the request.
func (i responsesItem) chatMessage() (chatMessage, bool) {
	switch strings.TrimSpace(i.Type) {
	case "", "message":
		if strings.TrimSpace(i.Role) == "" {
			return chatMessage{}, false
		}
		content := bytes.TrimSpace(i.Content)
		if len(content) == 0 || string(content) == "null" {
			return chatMessage{}, false
		}
		return chatMessage{Role: i.Role, Content: content}, true
	case "function_call_output":
		text := flattenText(i.Output)
		if text == "" {
			return chatMessage{}, false
		}
		return chatMessage{Role: "tool", Content: mustJSON(text)}, true
	default:
		return chatMessage{}, false
	}
}

// flattenText accepts a JSON string, an object with "text"/"output" or an array
// of text parts.
func flattenText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return strings.TrimSpace(text)
		}
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Text)
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return raw
}

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("read body: "+err.Error(), "invalid_request"))
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid JSON: "+err.Error(), "invalid_request"))
		return
	}
	resolved, err := s.pool.ResolveModel(r.Context(), req.Model)
	if err != nil {
		if errors.Is(err, ErrModelNotFound) {
			writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "model_not_found"))
		} else {
			s.writeUpstreamError(w, err)
		}
		return
	}
	messages, err := responsesMessages(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "invalid_request"))
		return
	}
	prompt, err := buildPrompt(messages)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "invalid_request"))
		return
	}
	displayModel := strings.TrimSpace(req.Model)
	if displayModel == "" {
		displayModel = resolved
	}
	store := req.storable()
	chatID := ""
	if prev := strings.TrimSpace(req.PreviousResponseID); prev != "" && store {
		if ref, ok := s.pool.Response(prev); ok {
			chatID = ref.ChatID
		} else {
			log.Printf("elm2api: previous_response_id %q is unknown (chat deleted or state reset), starting a new ELM chat", prev)
		}
	}
	id := newResponseID()
	created := time.Now().Unix()
	if req.Stream {
		s.streamResponse(w, r, id, displayModel, resolved, prompt, chatID, store, req.PreviousResponseID)
		return
	}
	var content, thinking strings.Builder
	usedChat, sessionID, err := s.pool.ChatIn(r.Context(), resolved, prompt, chatID, func(kind elmcore.DeltaKind, text string) error {
		if kind == elmcore.DeltaThinking {
			thinking.WriteString(text)
			return nil
		}
		content.WriteString(text)
		return nil
	})
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	if content.Len() == 0 && thinking.Len() == 0 {
		s.writeEmptyReplyError(w, len(prompt))
		return
	}
	if store {
		s.rememberResponse(id, usedChat, sessionID, resolved)
	}
	output := []map[string]any{}
	if reasoning := thinking.String(); reasoning != "" {
		output = append(output, reasoningItem(newItemID("rs"), reasoning))
	}
	output = append(output, messageItem(newItemID("msg"), content.String()))
	payload := responseObject(id, displayModel, created, "completed", store, output, prompt, content.String()+thinking.String())
	payload["previous_response_id"] = nil
	if prev := strings.TrimSpace(req.PreviousResponseID); prev != "" {
		payload["previous_response_id"] = prev
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) rememberResponse(id, chatID, sessionID, model string) {
	s.pool.RememberResponse(id, persistedResponse{
		Session:   sessionID,
		ChatID:    chatID,
		Model:     model,
		CreatedAt: time.Now(),
	})
}

// streamResponse emits the Responses API event stream.
func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, id, displayModel, resolved, prompt, chatID string, store bool, previousID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorPayload("streaming unsupported", "internal_error"))
		return
	}
	created := time.Now().Unix()
	messageID := newItemID("msg")
	reasoningID := newItemID("rs")
	sequence := 0
	started := false
	nextIndex, messageIndex, reasoningIndex := 0, -1, -1
	var content, thinking strings.Builder

	emit := func(event string, payload map[string]any) {
		payload["type"] = event
		payload["sequence_number"] = sequence
		sequence++
		if err := sseEvent(w, event, payload); err != nil {
			log.Printf("responses stream write failed: %v", err)
		}
		flusher.Flush()
	}
	// Headers are only committed once the upstream produced text, so a failing
	// ELM chat still becomes a real HTTP error instead of an empty stream.
	start := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		emit("response.created", map[string]any{`response`: responseObject(id, displayModel, created, "in_progress", store, []map[string]any{}, "", "")})
	}
	startReasoning := func() {
		reasoningIndex = nextIndex
		nextIndex++
		emit("response.output_item.added", map[string]any{
			"output_index": reasoningIndex,
			"item":         map[string]any{"id": reasoningID, "type": "reasoning", "summary": []any{}},
		})
	}
	startMessage := func() {
		messageIndex = nextIndex
		nextIndex++
		emit("response.output_item.added", map[string]any{
			"output_index": messageIndex,
			"item":         map[string]any{"id": messageID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		})
		emit("response.content_part.added", map[string]any{
			"item_id": messageID, "output_index": messageIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}

	usedChat, sessionID, err := s.pool.ChatIn(r.Context(), resolved, prompt, chatID, func(kind elmcore.DeltaKind, text string) error {
		start()
		if kind == elmcore.DeltaThinking {
			if reasoningIndex < 0 {
				startReasoning()
			}
			thinking.WriteString(text)
			emit("response.reasoning_summary_text.delta", map[string]any{
				"item_id": reasoningID, "output_index": reasoningIndex, "summary_index": 0, "delta": text,
			})
			return nil
		}
		if messageIndex < 0 {
			startMessage()
		}
		content.WriteString(text)
		emit("response.output_text.delta", map[string]any{
			"item_id": messageID, "output_index": messageIndex, "content_index": 0, "delta": text,
		})
		return nil
	})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		log.Printf("responses stream failed: %v", err)
		if !started {
			s.writeUpstreamError(w, err)
			return
		}
		payload := errorPayload(err.Error(), "upstream_error")
		if errors.Is(err, elmcore.ErrSessionExpired) {
			payload = errorPayload(err.Error(), "elm_session_expired")
			payload["hint"] = cookieHelp(s.cfg.Listen)
			s.logExpiredSessions()
		}
		emit("response.failed", map[string]any{"response": payload})
		return
	}
	if content.Len() == 0 && thinking.Len() == 0 {
		if !started {
			s.writeEmptyReplyError(w, len(prompt))
			return
		}
		failed := responseObject(id, displayModel, created, "failed", store, []map[string]any{}, "", "")
		failed["error"] = emptyReplyPayload(len(prompt))["error"]
		emit("response.failed", map[string]any{"response": failed})
		return
	}
	if store {
		s.rememberResponse(id, usedChat, sessionID, resolved)
	}
	output := make([]map[string]any, nextIndex)
	if reasoningIndex >= 0 {
		output[reasoningIndex] = reasoningItem(reasoningID, thinking.String())
		emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": reasoningID, "output_index": reasoningIndex, "summary_index": 0, "text": thinking.String(),
		})
		emit("response.output_item.done", map[string]any{"output_index": reasoningIndex, "item": output[reasoningIndex]})
	}
	if messageIndex >= 0 {
		output[messageIndex] = messageItem(messageID, content.String())
		emit("response.output_text.done", map[string]any{
			"item_id": messageID, "output_index": messageIndex, "content_index": 0, "text": content.String(),
		})
		emit("response.content_part.done", map[string]any{
			"item_id": messageID, "output_index": messageIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": content.String(), "annotations": []any{}},
		})
		emit("response.output_item.done", map[string]any{"output_index": messageIndex, "item": output[messageIndex]})
	}
	final := responseObject(id, displayModel, created, "completed", store, output, prompt, content.String()+thinking.String())
	final["previous_response_id"] = nil
	if previousID = strings.TrimSpace(previousID); previousID != "" {
		final["previous_response_id"] = previousID
	}
	emit("response.completed", map[string]any{"response": final})
}

func reasoningItem(id, text string) map[string]any {
	return map[string]any{
		"id":      id,
		"type":    "reasoning",
		"summary": []map[string]any{{"type": "summary_text", "text": text}},
	}
}

func messageItem(id, text string) map[string]any {
	return map[string]any{
		"id":      id,
		"type":    "message",
		"status":  "completed",
		"role":    "assistant",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

func responseObject(id, model string, created int64, status string, store bool, output []map[string]any, prompt, answer string) map[string]any {
	payload := map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          created,
		"status":              status,
		"model":               model,
		"output":              output,
		"store":               store,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
	}
	if status == "completed" {
		payload["usage"] = estimateUsage(prompt, answer)
	}
	return payload
}

// estimateUsage reports approximate token counts (characters / 4). The ELM web
// chat does not return usage, so these are estimates rather than billed values.
func estimateUsage(prompt, answer string) map[string]any {
	in := len(prompt) / 4
	out := len(answer) / 4
	return map[string]any{
		"input_tokens":         in,
		"output_tokens":        out,
		"total_tokens":         in + out,
		"input_tokens_details": map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
	}
}

// sseEvent writes one named Server-Sent Event.
func sseEvent(w io.Writer, event string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	return nil
}
