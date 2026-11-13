package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const pwshTool = `{"type":"function","function":{"name":"pwsh","description":"Run a PowerShell command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}`

func TestParseToolsBothShapes(t *testing.T) {
	chat := parseTools([]byte("[" + pwshTool + "]"))
	if len(chat) != 1 || chat[0].Name != "pwsh" {
		t.Fatalf("chat tools = %#v", chat)
	}
	responses := parseTools([]byte(`[{"type":"function","name":"read","description":"Read a file","parameters":{"type":"object","properties":{}}}]`))
	if len(responses) != 1 || responses[0].Name != "read" {
		t.Fatalf("responses tools = %#v", responses)
	}
}

func TestToolChoiceModes(t *testing.T) {
	if mode, forced := toolChoice([]byte(`"none"`)); mode != "none" || forced != "" {
		t.Fatalf("none = %q %q", mode, forced)
	}
	if mode, forced := toolChoice([]byte(`{"type":"function","function":{"name":"pwsh"}}`)); mode != "required" || forced != "pwsh" {
		t.Fatalf("forced = %q %q", mode, forced)
	}
	if mode, _ := toolChoice(nil); mode != "auto" {
		t.Fatalf("default = %q", mode)
	}
}

func TestParseToolCallsJSONBlock(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	reply := "I will check.\n\n```json\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"git status\"}}]}\n```\n"
	calls, rest := parseToolCalls(reply, tools)
	if len(calls) != 1 || calls[0].Name != "pwsh" {
		t.Fatalf("calls = %#v", calls)
	}
	if !strings.Contains(calls[0].Arguments, "git status") {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
	if strings.Contains(rest, "tool_calls") {
		t.Fatalf("rest still has the call: %q", rest)
	}
}

// Some models answer with the bare YAML-ish block instead of JSON.
func TestParseToolCallsBareBlock(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	reply := "```\npwsh\ndescription: Show git working tree status\ncommand: git status\nworkdir: C:/tmp\n```"
	calls, _ := parseToolCalls(reply, tools)
	if len(calls) != 1 || calls[0].Name != "pwsh" {
		t.Fatalf("calls = %#v", calls)
	}
	for _, want := range []string{"git status", "workdir", "C:/tmp"} {
		if !strings.Contains(calls[0].Arguments, want) {
			t.Fatalf("arguments %q missing %q", calls[0].Arguments, want)
		}
	}
}

func TestParseToolCallsXMLBlock(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	reply := "<tool_call>{\"name\":\"pwsh\",\"arguments\":{\"command\":\"ls\"}}</tool_call>"
	calls, _ := parseToolCalls(reply, tools)
	if len(calls) != 1 || calls[0].Name != "pwsh" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestParseToolCallsPlainText(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	calls, rest := parseToolCalls("git 仓库是干净的，没有未提交的改动。", tools)
	if len(calls) != 0 {
		t.Fatalf("unexpected calls %#v", calls)
	}
	if !strings.Contains(rest, "干净的") {
		t.Fatalf("content lost: %q", rest)
	}
}

func TestBuildPromptWithToolsAndResults(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	messages := []chatMessage{
		{Role: "user", Content: mustJSON("status?")},
		{Role: "assistant", ToolCalls: []chatToolCall{{ID: "call_1"}}},
		{Role: "tool", ToolCallID: "call_1", Content: mustJSON("clean")},
	}
	messages[1].ToolCalls[0].Function.Name = "pwsh"
	messages[1].ToolCalls[0].Function.Arguments = `{"command":"git status"}`
	prompt, err := buildPromptWithOptions(messages, promptOptions{Tools: tools, Choice: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# 工具调用协议", "### pwsh", "User: status?", "[tool call pwsh(", "Tool result (call_1): clean"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q\n%s", want, prompt)
		}
	}
}

func TestServerToolCallingNonStream(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels, reply: "```json\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"git status\"}}]}\n```"}
	pool, _ := newTestPool(t, f, 2)
	ts := httptestServer(t, pool, f)
	defer ts.Close()

	body := `{"model":"qwen-3.5","messages":[{"role":"user","content":"check git status"}],"tools":[` + pwshTool + `]}`
	status, raw := postChat(t, ts, body)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if !strings.Contains(string(raw), "\"tool_calls\"") || !strings.Contains(string(raw), "\"finish_reason\":\"tool_calls\"") {
		t.Fatalf("no tool_calls in %s", raw)
	}
	if !strings.Contains(string(raw), "git status") {
		t.Fatalf("arguments missing: %s", raw)
	}
}

func TestServerToolCallingStream(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels, reply: "```json\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"git status\"}}]}\n```"}
	pool, _ := newTestPool(t, f, 2)
	ts := httptestServer(t, pool, f)
	defer ts.Close()

	body := `{"model":"qwen-3.5","messages":[{"role":"user","content":"check git status"}],"tools":[` + pwshTool + `],"stream":true}`
	status, raw := postChat(t, ts, body)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, raw)
	}
	text := string(raw)
	for _, want := range []string{"\"tool_calls\"", "\"finish_reason\":\"tool_calls\"", "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Logf("stream: %s", text)
			t.Fatalf("missing %q", want)
		}
	}
}

func TestServerToolCallingDisabledByChoice(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels, reply: "```json\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"git status\"}}]}\n```"}
	pool, _ := newTestPool(t, f, 2)
	ts := httptestServer(t, pool, f)
	defer ts.Close()

	body := `{"model":"qwen-3.5","messages":[{"role":"user","content":"hi"}],"tools":[` + pwshTool + `],"tool_choice":"none"}`
	status, raw := postChat(t, ts, body)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if strings.Contains(string(raw), "\"tool_calls\"") {
		t.Fatalf("tool_choice none still emitted calls: %s", raw)
	}
}

// httptestServer starts the elm2api router against a fake upstream.
func httptestServer(t *testing.T, pool *Pool, up *fakeUpstream) *httptest.Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.APIKeys = nil
	return httptest.NewServer(NewServer(cfg, pool, up).Handler())
}

// postChat posts a Chat Completions request and returns status and body.
func postChat(t *testing.T, ts *httptest.Server, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, buf.Bytes()
}
