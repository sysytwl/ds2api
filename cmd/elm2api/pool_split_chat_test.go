package main

import (
	"context"
	"strings"
	"testing"

	"elm2api/internal/elmcore"
)

func TestPoolChatSplitsLongPrompt(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	long := strings.Repeat("context line\n", 2000) // 26000 bytes, over the ELM limit
	var content strings.Builder
	var thinking strings.Builder
	err := pool.Chat(context.Background(), "Qwen/Qwen3.5-397B-A17B-FP8", long, func(kind elmcore.DeltaKind, text string) error {
		if kind == elmcore.DeltaThinking {
			thinking.WriteString(text)
			return nil
		}
		content.WriteString(text)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	f.mu.Lock()
	calls := f.chatCalls
	f.mu.Unlock()
	if calls < 2 {
		t.Fatalf("expected the prompt to be split, got %d upstream call(s)", calls)
	}
	// Only the last chunk's deltas may reach the caller.
	if content.String() != "hello" {
		t.Fatalf("content = %q, want hello", content.String())
	}
	if thinking.String() != "thinking" {
		t.Fatalf("thinking = %q, want thinking", thinking.String())
	}
}

func TestPoolChatSinglePromptNoSplit(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	if err := pool.Chat(context.Background(), "Qwen/Qwen3.5-397B-A17B-FP8", "short question", func(elmcore.DeltaKind, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	calls := f.chatCalls
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected one upstream call, got %d", calls)
	}
}
