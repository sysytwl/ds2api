package main

import (
	"encoding/json"
	"strings"
	"testing"

	"elm2api/internal/elmcore"
)

func TestPickEmptyPrompt(t *testing.T) {
	cases := []struct {
		name    string
		prompts []elmcore.Prompt
		want    int64
	}{
		{"prefers empty name", []elmcore.Prompt{{ID: 5, Name: "Default Prompt", Prompt: "x"}, {ID: 25, Name: "Empty Prompt"}}, 25},
		{"any blank template", []elmcore.Prompt{{ID: 5, Prompt: "x"}, {ID: 7, Name: "Blank"}}, 7},
		{"none", []elmcore.Prompt{{ID: 5, Prompt: "x"}}, 0},
		{"empty list", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := elmcore.PickEmptyPrompt(tc.prompts); got != tc.want {
				t.Fatalf("PickEmptyPrompt = %d, want %d", got, tc.want)
			}
		})
	}
}

// ELM applies its default institution prompt when promptId is null, which makes
// the model answer as the ELM assistant and fight the tool protocol. The service
// must discover the blank template, select it once and send its id every turn.
func TestEmptyPromptSelectedAndSent(t *testing.T) {
	f := &fakeUpstream{
		loggedIn: true,
		models:   testModels,
		prompts: []elmcore.Prompt{
			{ID: 5, Name: "Default Prompt", Prompt: "You are ELM..."},
			{ID: 25, Name: "Empty Prompt"},
		},
	}
	pool, _ := newTestPool(t, f, 2)
	ts := httptestServer(t, pool, f)
	defer ts.Close()

	body := `{"model":"qwen-3.5","messages":[{"role":"user","content":"hi"}]}`
	if status, raw := postChat(t, ts, body); status != 200 {
		t.Fatalf("status = %d: %s", status, raw)
	}
	again := `{"model":"qwen-3.5","messages":[{"role":"user","content":"again"}]}`
	if status, raw := postChat(t, ts, again); status != 200 {
		t.Fatalf("status = %d: %s", status, raw)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.selectedPromptIDs) != 1 || f.selectedPromptIDs[0] != 25 {
		t.Fatalf("selected prompts = %v, want [25] once", f.selectedPromptIDs)
	}
	if len(f.streamPromptIDs) != 2 {
		t.Fatalf("chat calls = %v", f.streamPromptIDs)
	}
	for i, id := range f.streamPromptIDs {
		if id != 25 {
			t.Fatalf("chat call %d promptId = %d, want 25", i, id)
		}
	}
}

func TestEmptyPromptDisabled(t *testing.T) {
	f := &fakeUpstream{
		loggedIn: true,
		models:   testModels,
		prompts:  []elmcore.Prompt{{ID: 25, Name: "Empty Prompt"}},
	}
	pool, _ := newTestPool(t, f, 2)
	pool.SetEmptyPrompt(false)
	ts := httptestServer(t, pool, f)
	defer ts.Close()

	if status, raw := postChat(t, ts, `{"model":"qwen-3.5","messages":[{"role":"user","content":"hi"}]}`); status != 200 {
		t.Fatalf("status = %d: %s", status, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.selectedPromptIDs) != 0 {
		t.Fatalf("selected prompts = %v, want none", f.selectedPromptIDs)
	}
	if len(f.streamPromptIDs) != 1 || f.streamPromptIDs[0] != 0 {
		t.Fatalf("prompt ids = %v, want [0]", f.streamPromptIDs)
	}
}

// Not every model wraps the call in a fence; a bare object after one sentence
// still has to become a tool call.
func TestParseToolCallsEmbeddedJSON(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	reply := "我先看一下目录。\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"ls\"}}]}"
	calls, rest := parseToolCalls(reply, tools)
	if len(calls) != 1 || calls[0].Name != "pwsh" {
		t.Fatalf("calls = %#v", calls)
	}
	if !strings.Contains(rest, "我先看一下目录") {
		t.Fatalf("rest lost the preamble: %q", rest)
	}
	if strings.Contains(rest, "tool_calls") {
		t.Fatalf("rest still holds the call: %q", rest)
	}
}

// A bare JSON array of calls is a common variant too.
func TestParseToolCallsArrayEnvelope(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	reply := "```json\n[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"ls\"}}]\n```"
	calls, _ := parseToolCalls(reply, tools)
	if len(calls) != 1 || calls[0].Name != "pwsh" || !strings.Contains(calls[0].Arguments, "ls") {
		t.Fatalf("calls = %#v", calls)
	}
}

// Ordinary JSON in a normal answer must not turn into a tool call.
func TestParseToolCallsIgnoresOrdinaryJSON(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	reply := "```json\n{\"name\":\"config\",\"value\":1}\n```"
	calls, rest := parseToolCalls(reply, tools)
	if len(calls) != 0 {
		t.Fatalf("calls = %#v", calls)
	}
	if !strings.Contains(rest, "config") {
		t.Fatalf("content lost: %q", rest)
	}
}

// The transcript must show the model its own call in the protocol's own shape,
// so a replayed tool result is recognised instead of re-issued.
func TestAssistantToolCallRenderedAsProtocolJSON(t *testing.T) {
	msg := chatMessage{Role: "assistant"}
	msg.ToolCalls = []chatToolCall{{ID: "call_1"}}
	msg.ToolCalls[0].Function.Name = "pwsh"
	msg.ToolCalls[0].Function.Arguments = `{"command":"ls"}`
	rendered := msg.render()
	if !strings.Contains(rendered, "Assistant: ") || !strings.Contains(rendered, `"tool_calls"`) {
		t.Fatalf("rendered = %q", rendered)
	}
	call := strings.TrimPrefix(rendered, "Assistant: ")
	if !json.Valid([]byte(call)) {
		t.Fatalf("rendered call is not valid JSON: %q", rendered)
	}
}

func TestToolResultCarriesProtocolReminder(t *testing.T) {
	msg := chatMessage{Role: "tool", ToolCallID: "call_1", Content: mustJSON("clean")}
	rendered := msg.render()
	if !strings.Contains(rendered, "Tool result (call_1): clean") {
		t.Fatalf("rendered = %q", rendered)
	}
	if !strings.Contains(rendered, toolResultReminder) {
		t.Fatalf("reminder missing: %q", rendered)
	}
}
