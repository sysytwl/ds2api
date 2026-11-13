package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type responsesBody struct {
	ID                 string `json:"id"`
	Object             string `json:"object"`
	Status             string `json:"status"`
	PreviousResponseID any    `json:"previous_response_id"`
	Output             []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	} `json:"output"`
	Usage map[string]any `json:"usage"`
}

func postResponses(t *testing.T, ts *httptest.Server, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, buf.Bytes()
}

func messageText(t *testing.T, body responsesBody) string {
	t.Helper()
	for _, item := range body.Output {
		if item.Type == "message" && len(item.Content) > 0 {
			return item.Content[0].Text
		}
	}
	t.Fatalf("no message item in %#v", body.Output)
	return ""
}

func TestResponsesNewChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(DefaultConfig(), pool, f).Handler())
	defer ts.Close()

	status, raw := postResponses(t, ts, `{"model":"qwen-3.5","input":"hi"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	var body responsesBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "response" || body.Status != "completed" {
		t.Fatalf("unexpected envelope: %#v", body)
	}
	if !strings.HasPrefix(body.ID, "resp_") {
		t.Fatalf("id = %q", body.ID)
	}
	if got := messageText(t, body); got != "hello" {
		t.Fatalf("text = %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 1 || f.streamPrompts[0] != "hi" {
		t.Fatalf("newChatCalls=%d prompts=%#v", f.newChatCalls, f.streamPrompts)
	}
}

// previous_response_id keeps the ELM chat, so the next call sends only the new
// input instead of the whole history.
func TestResponsesContinuesPreviousResponse(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(DefaultConfig(), pool, f).Handler())
	defer ts.Close()

	status, raw := postResponses(t, ts, `{"model":"qwen-3.5","input":"first"}`)
	if status != http.StatusOK {
		t.Fatalf("first status = %d: %s", status, raw)
	}
	var first responsesBody
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	second := `{"model":"qwen-3.5","input":"second","previous_response_id":"` + first.ID + `"}`
	status, raw = postResponses(t, ts, second)
	if status != http.StatusOK {
		t.Fatalf("second status = %d: %s", status, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 1 {
		t.Fatalf("expected one chat, got %d", f.newChatCalls)
	}
	if len(f.streamChats) != 2 || f.streamChats[0] != f.streamChats[1] {
		t.Fatalf("chats = %#v", f.streamChats)
	}
	if f.streamPrompts[1] != "second" {
		t.Fatalf("second prompt = %q", f.streamPrompts[1])
	}
}

// An unknown (deleted/expired) response id starts a new chat instead of failing.
func TestResponsesUnknownPreviousStartsNewChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(DefaultConfig(), pool, f).Handler())
	defer ts.Close()

	status, raw := postResponses(t, ts, `{"model":"qwen-3.5","input":"hi","previous_response_id":"resp_missing"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 1 {
		t.Fatalf("expected a new chat, got %d", f.newChatCalls)
	}
	if f.streamPrompts[0] != "hi" {
		t.Fatalf("prompt = %q", f.streamPrompts[0])
	}
}

// store:false is not supported (the web chat always keeps history), so it is
// treated as "start over": every call opens a new chat and nothing is mapped.
func TestResponsesStoreFalseStartsNewChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(DefaultConfig(), pool, f).Handler())
	defer ts.Close()

	status, raw := postResponses(t, ts, `{"model":"qwen-3.5","input":"one","store":false}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	var first responsesBody
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	second := `{"model":"qwen-3.5","input":"two","previous_response_id":"` + first.ID + `"}`
	status, raw = postResponses(t, ts, second)
	if status != http.StatusOK {
		t.Fatalf("second status = %d: %s", status, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 2 {
		t.Fatalf("store:false must not be resumable, got %d chats", f.newChatCalls)
	}
}

func TestResponsesStreaming(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(DefaultConfig(), pool, f).Handler())
	defer ts.Close()

	status, raw := postResponses(t, ts, `{"model":"qwen-3.5","input":"hi","stream":true}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	text := string(raw)
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		`"delta":"he"`,
		`"delta":"llo"`,
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(text, want) {
			t.Logf("stream body: %s", text)
			t.Fatalf("stream missing %q", want)
		}
	}
}

// The Responses input may be a message array with content parts, plus
// instructions that become the system prompt.
func TestResponsesInputArray(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(DefaultConfig(), pool, f).Handler())
	defer ts.Close()

	body := `{"model":"qwen-3.5","instructions":"be brief","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello there"}]}]}`
	status, raw := postResponses(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.streamPrompts[0], "System: be brief") || !strings.Contains(f.streamPrompts[0], "User: hello there") {
		t.Fatalf("prompt = %q", f.streamPrompts[0])
	}
}
