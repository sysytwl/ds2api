package main

import (
	"strings"
	"testing"
)

// A split prompt must carry the tool protocol again in the chunk the model
// answers, otherwise a long conversation reaches the model without it.
func TestSplitPromptWithTailRepeatsTail(t *testing.T) {
	long := strings.Repeat("context line\n", 2000) // 26000 bytes
	limit := elmMaxPromptChars - len(splitContextPrefix)
	tail := "# TOOL TAIL\n{\"tool_calls\":[]}"
	chunks := splitPromptWithTail(long, limit, tail)
	if len(chunks) < 2 {
		t.Fatalf("expected a split prompt, got %d chunk(s)", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) > limit {
			t.Fatalf("chunk %d is %d bytes, limit %d", i, len(chunk), limit)
		}
	}
	if last := chunks[len(chunks)-1]; !strings.HasPrefix(last, tail) {
		t.Fatalf("last chunk does not start with the tail: %q", last[:60])
	}
	for i, chunk := range chunks[:len(chunks)-1] {
		if strings.Contains(chunk, "TOOL TAIL") {
			t.Fatalf("chunk %d carries the tail as well", i)
		}
	}
	if chunks[0] != splitPrompt(long, limit)[0] {
		t.Fatal("the first chunk changed, chat prefix matching would break")
	}
}

func TestSplitPromptWithTailIgnoredWhenPromptFits(t *testing.T) {
	chunks := splitPromptWithTail("short question", elmMaxPromptChars, "TAIL")
	if len(chunks) != 1 || chunks[0] != "short question" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

// End to end: an oversized prompt with tools still comes back as a tool call.
func TestChatToolsSurviveLongPrompt(t *testing.T) {
	reply := "```json\n{\"tool_calls\":[{\"name\":\"pwsh\",\"arguments\":{\"command\":\"git status\"}}]}\n```"
	f := &fakeUpstream{loggedIn: true, models: testModels, reply: reply}
	pool, _ := newTestPool(t, f, 2)
	ts := httptestServer(t, pool, f)
	defer ts.Close()

	long := strings.Repeat("filler ", 3000) // over the ELM message limit
	body := `{"model":"Qwen/Qwen3.5-397B-A17B-FP8","stream":true,"messages":[{"role":"system","content":"` + long + `"},{"role":"user","content":"check git status"}],"tools":[` + pwshTool + `]}`
	status, raw := postChat(t, ts, body)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, raw)
	}

	f.mu.Lock()
	prompts := append([]string(nil), f.streamPrompts...)
	f.mu.Unlock()
	if len(prompts) < 2 {
		t.Fatalf("expected the prompt to be split, got %d upstream call(s)", len(prompts))
	}
	if last := prompts[len(prompts)-1]; !strings.Contains(last, "工具调用") {
		t.Fatal("the last chunk does not repeat the tool protocol")
	}
	out := string(raw)
	for _, want := range []string{"tool_calls", "pwsh", "git status"} {
		if !strings.Contains(out, want) {
			t.Fatalf("response %q missing %q", out, want)
		}
	}
}
