package main

import (
	"context"
	"path/filepath"
	"testing"

	"ds2api/internal/elmcore"
)

func chatOnce(t *testing.T, pool *Pool, model, prompt string) {
	t.Helper()
	if err := pool.Chat(context.Background(), model, prompt, func(elmcore.DeltaKind, string) error { return nil }); err != nil {
		t.Fatalf("chat %q: %v", prompt, err)
	}
}

func TestPoolChatReusesChatForContinuingConversation(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: hello")
	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: hello\n\nUser: again")

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 1 {
		t.Fatalf("expected one ELM chat, got %d", f.newChatCalls)
	}
	if len(f.streamPrompts) != 2 {
		t.Fatalf("expected two upstream calls, got %d", len(f.streamPrompts))
	}
	if f.streamPrompts[1] != "User: again" {
		t.Fatalf("second call sent %q, want only the new turn", f.streamPrompts[1])
	}
	if f.streamChats[0] != f.streamChats[1] {
		t.Fatalf("chats differ: %q vs %q", f.streamChats[0], f.streamChats[1])
	}
}

func TestPoolChatNewConversationStartsNewChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: hello")
	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: something else")

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 2 {
		t.Fatalf("expected a new chat for the new conversation, got %d", f.newChatCalls)
	}
	if f.streamPrompts[1] != "User: something else" {
		t.Fatalf("second call sent %q", f.streamPrompts[1])
	}
}

func TestPoolChatModelSwitchStartsNewChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: hello")
	chatOnce(t, pool, "meta-llama/Llama-3.3-70B-Instruct", "User: hello")

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 2 {
		t.Fatalf("expected a new chat on model switch, got %d", f.newChatCalls)
	}
}

func TestPoolChatStatePersistsChatID(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, view := newTestPool(t, f, 2)
	pool.SetStatePath(statePath)
	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: hello")

	restored := &fakeUpstream{loggedIn: true, models: testModels}
	pool2 := NewPool(restored, "", 2, 1, 60)
	pool2.SetStatePath(statePath)
	if _, err := pool2.Add(context.Background(), "test", "SESSION=abc"); err != nil {
		t.Fatal(err)
	}
	pool2.LoadState()
	got := pool2.Views()
	if len(got) != 1 || got[0].ID != view.ID {
		t.Fatalf("unexpected views %+v", got)
	}
	if got[0].ChatID != "new-chat-1" {
		t.Fatalf("chat id not restored: %q", got[0].ChatID)
	}
}
