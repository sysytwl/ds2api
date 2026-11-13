package main

import (
	"strings"
	"testing"
)

func TestTrimEchoedAssistant(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Assistant: hello\n\nUser: next", "User: next"},
		{"Assistant: a\n\nAssistant: b\n\nTool result (x): ok", "Tool result (x): ok"},
		{"Tool result (x): ok", "Tool result (x): ok"},
		{"Assistant: only", ""},
		{"User: next", "User: next"},
	}
	for _, tc := range cases {
		if got := trimEchoedAssistant(tc.in); got != tc.want {
			t.Fatalf("trimEchoedAssistant(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseToolCallsRepairsFenceVariant(t *testing.T) {
	tools := parseTools([]byte("[" + pwshTool + "]"))
	reply := "我用工具查一下。\n\n``` json\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"git status\"}}]}。\n```"
	calls, rest := parseToolCalls(reply, tools)
	if len(calls) != 1 || calls[0].Name != "pwsh" || !strings.Contains(calls[0].Arguments, "git status") {
		t.Fatalf("calls = %#v (rest %q)", calls, rest)
	}
}

// A model that writes the call as plain text is asked once more in the same
// chat instead of returning the JSON to the client.
func TestServerToolCallRepairTurn(t *testing.T) {
	bad := "我来执行：\n\n```json\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"git status\",}}]}\n```"
	good := "```json\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"git status\"}}]}\n```"
	f := &fakeUpstream{loggedIn: true, models: testModels, replies: []string{bad, good}}
	pool, _ := newTestPool(t, f, 2)
	ts := httptestServer(t, pool, f)
	defer ts.Close()

	body := `{"model":"qwen-3.5","messages":[{"role":"user","content":"check git status"}],"tools":[` + pwshTool + `]}`
	status, raw := postChat(t, ts, body)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if !strings.Contains(string(raw), "\"tool_calls\"") || !strings.Contains(string(raw), "\"finish_reason\":\"tool_calls\"") {
		t.Fatalf("repair did not produce a tool call: %s", raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streamPrompts) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(f.streamPrompts))
	}
	if !strings.Contains(f.streamPrompts[1], "没有按协议输出") {
		t.Fatalf("second prompt is not the repair turn: %q", f.streamPrompts[1])
	}
}

// The continuation delta must not echo the assistant answer back as a user
// message: ELM already has it, and the duplicate makes the model repeat itself.
func TestContinuationDropsEchoedAssistant(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels, reply: "ok"}
	pool, _ := newTestPool(t, f, 2)
	ts := httptestServer(t, pool, f)
	defer ts.Close()

	first := `{"model":"qwen-3.5","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`
	if status, raw := postChat(t, ts, first); status != 200 {
		t.Fatalf("first status = %d: %s", status, raw)
	}
	second := `{"model":"qwen-3.5","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"next"}]}`
	if status, raw := postChat(t, ts, second); status != 200 {
		t.Fatalf("second status = %d: %s", status, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streamPrompts) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(f.streamPrompts))
	}
	if strings.Contains(f.streamPrompts[1], "Assistant: hello") {
		t.Fatalf("delta echoes the assistant answer: %q", f.streamPrompts[1])
	}
	if !strings.Contains(f.streamPrompts[1], "User: next") {
		t.Fatalf("delta lost the new user message: %q", f.streamPrompts[1])
	}
}
