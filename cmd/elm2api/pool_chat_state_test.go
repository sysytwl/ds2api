package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"elm2api/internal/elmcore"
)

// eol keeps the test prompts readable without embedded escape sequences.
var eol = string(rune(10))

// nextTurn appends one new turn to the conversation history.
func nextTurn(history, turn string) string { return history + eol + eol + turn }

func chatOnce(t *testing.T, pool *Pool, model, prompt string) {
	t.Helper()
	if err := pool.Chat(context.Background(), model, prompt, func(elmcore.DeltaKind, string) error { return nil }); err != nil {
		t.Fatalf("chat %q: %v", prompt, err)
	}
}

func TestPoolChatReusesChatForContinuingConversation(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	const model = "Qwen/Qwen3.5-397B-A17B-FP8"
	chatOnce(t, pool, model, "User: hello")
	chatOnce(t, pool, model, nextTurn("User: hello", "User: again"))

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

	const model = "Qwen/Qwen3.5-397B-A17B-FP8"
	chatOnce(t, pool, model, "User: hello")
	chatOnce(t, pool, model, "User: something else")

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 2 {
		t.Fatalf("expected a new chat for the new conversation, got %d", f.newChatCalls)
	}
	if f.streamPrompts[1] != "User: something else" {
		t.Fatalf("second call sent %q", f.streamPrompts[1])
	}
}

// Switching the model must keep the conversation: the ELM model is a session
// setting, so the same chat continues with the new model.
func TestPoolChatModelSwitchKeepsChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: hello")
	chatOnce(t, pool, "meta-llama/Llama-3.3-70B-Instruct", nextTurn("User: hello", "User: again"))

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 1 {
		t.Fatalf("model switch must not open a new chat, got %d", f.newChatCalls)
	}
	if f.streamChats[0] != f.streamChats[1] {
		t.Fatalf("chats differ: %q vs %q", f.streamChats[0], f.streamChats[1])
	}
	if len(f.streamModels) != 2 || f.streamModels[1] != "meta-llama/Llama-3.3-70B-Instruct" {
		t.Fatalf("model switch was not pushed upstream: %#v", f.streamModels)
	}
}

// An agent that runs several conversations on one session (subagents, a second
// window) must get one chat each; a request that continues an older
// conversation returns to that chat instead of opening yet another one.
func TestPoolChatKeepsSeveralConversations(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	const model = "Qwen/Qwen3.5-397B-A17B-FP8"
	chatOnce(t, pool, model, "User: A")
	chatOnce(t, pool, model, "User: B")
	chatOnce(t, pool, model, nextTurn("User: B", "User: B2"))
	chatOnce(t, pool, model, nextTurn("User: A", "User: A2"))

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 2 {
		t.Fatalf("expected two chats, got %d", f.newChatCalls)
	}
	want := []string{"new-chat-1", "new-chat-2", "new-chat-2", "new-chat-1"}
	for i, id := range want {
		if f.streamChats[i] != id {
			t.Fatalf("call %d went to %q, want %q (all: %#v)", i, f.streamChats[i], id, f.streamChats)
		}
	}
	if f.streamPrompts[3] != "User: A2" {
		t.Fatalf("fourth call sent %q, want the tail only", f.streamPrompts[3])
	}
}

// A rollback in the client (edited or removed messages) cannot be replayed in
// the web chat, so it must start a new chat instead of mixing histories.
func TestPoolChatRollbackStartsNewChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	const model = "Qwen/Qwen3.5-397B-A17B-FP8"
	chatOnce(t, pool, model, nextTurn("User: first", "User: second"))
	chatOnce(t, pool, model, nextTurn("User: first", "User: different"))

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 2 {
		t.Fatalf("edited prompt must open a new chat, got %d", f.newChatCalls)
	}
	if !strings.Contains(f.streamPrompts[1], "User: different") {
		t.Fatalf("second call sent %q", f.streamPrompts[1])
	}
}

// Retrying the identical prompt (an empty upstream reply) reuses the chat so the
// context survives the retry.
func TestPoolChatRetrySamePromptReusesChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)

	const model = "Qwen/Qwen3.5-397B-A17B-FP8"
	chatOnce(t, pool, model, "User: hello")
	chatOnce(t, pool, model, "User: hello")

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 1 {
		t.Fatalf("retry must reuse the chat, got %d new chat(s)", f.newChatCalls)
	}
	if f.streamChats[0] != f.streamChats[1] {
		t.Fatalf("chats differ: %q vs %q", f.streamChats[0], f.streamChats[1])
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

// After a restart the remembered prefix still identifies the conversation, so
// the next turn goes to the same chat and only the new turn is sent.
func TestPoolChatStateResumesConversation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	pool.SetStatePath(statePath)
	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: hello")

	restored := &fakeUpstream{loggedIn: true, models: testModels}
	pool2 := NewPool(restored, "", 2, 1, 60)
	pool2.SetStatePath(statePath)
	if _, err := pool2.Add(context.Background(), "test", "SESSION=abc"); err != nil {
		t.Fatal(err)
	}
	chatOnce(t, pool2, "Qwen/Qwen3.5-397B-A17B-FP8", nextTurn("User: hello", "User: again"))

	restored.mu.Lock()
	defer restored.mu.Unlock()
	if restored.newChatCalls != 0 {
		t.Fatalf("expected the restored chat to be reused, got %d new chat(s)", restored.newChatCalls)
	}
	if len(restored.streamChats) != 1 || restored.streamChats[0] != "new-chat-1" {
		t.Fatalf("unexpected chats: %#v", restored.streamChats)
	}
	if restored.streamPrompts[0] != "User: again" {
		t.Fatalf("sent %q, want only the new turn", restored.streamPrompts[0])
	}
}

// The state file must stay small: only a hash, a length and a short preview of
// the conversation are kept, never the whole prompt.
func TestPoolChatStateKeepsPromptCompact(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	pool.SetStatePath(statePath)
	long := "User: " + strings.Repeat("filler ", 2000) + "TAIL-MARKER"
	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", long)

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 4096 {
		t.Fatalf("state file is %d bytes for a %d byte prompt", len(raw), len(long))
	}
	if strings.Contains(string(raw), "TAIL-MARKER") {
		t.Fatalf("state file keeps the whole prompt: %s", raw)
	}
	if !strings.Contains(string(raw), "new-chat-1") {
		t.Fatalf("state file lost the chat id: %s", raw)
	}
}

// A chat deleted in the web UI must not break the next request.
func TestPoolChatFallsBackWhenChatIsGone(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", "User: hello")

	f.mu.Lock()
	f.failChat = errString("elm: chat stream error: chat not found")
	f.failChatID = "new-chat-1"
	f.mu.Unlock()
	chatOnce(t, pool, "Qwen/Qwen3.5-397B-A17B-FP8", nextTurn("User: hello", "User: again"))

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newChatCalls != 2 {
		t.Fatalf("expected a replacement chat, got %d", f.newChatCalls)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
