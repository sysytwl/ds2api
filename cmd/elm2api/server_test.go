package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ds2api/internal/elmcore"
)

type fakeUpstream struct {
	mu            sync.Mutex
	loggedIn      bool
	expired       bool
	models        []elmcore.Model
	chatCalls     int
	active        int
	maxActive     int
	delay         time.Duration
	setModels     []string
	streamModels  []string
	user          elmcore.User
	streamPrompts []string
	streamChats   []string
	newChatCalls  int
	quota         elmcore.Quota
	failChat      error
	// failChatID, when set, restricts failChat to one chat id (a chat that was
	// deleted in the web UI still answers, the other chats do not).
	failChatID string
}

func (f *fakeUpstream) IsLoggedIn(context.Context, string) (bool, error) {
	if f.expired {
		return false, elmcore.ErrSessionExpired
	}
	return f.loggedIn, nil
}
func (f *fakeUpstream) User(context.Context, string) (elmcore.User, error) {
	if f.user.Email == "" {
		return elmcore.User{Email: "user@example.com", ResolvedID: "s1@ed.ac.uk"}, nil
	}
	return f.user, nil
}
func (f *fakeUpstream) Quota(context.Context, string) (elmcore.Quota, error) {
	return f.quota, nil
}
func (f *fakeUpstream) Models(context.Context, string) ([]elmcore.Model, error) {
	return f.models, nil
}
func (f *fakeUpstream) ChatID(context.Context, string) (string, error) { return "chat-1", nil }
func (f *fakeUpstream) NewChat(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newChatCalls++
	return fmt.Sprintf("new-chat-%d", f.newChatCalls), nil
}

func (f *fakeUpstream) SetModel(_ context.Context, _ string, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setModels = append(f.setModels, model)
	return nil
}
func (f *fakeUpstream) CurrentModel(context.Context, string) (string, error) {
	return "Qwen/Qwen3.5-397B-A17B-FP8", nil
}
func (f *fakeUpstream) StreamChat(ctx context.Context, _ string, chatID string, prompt, model string, onDelta func(elmcore.DeltaKind, string) error) error {
	f.mu.Lock()
	f.chatCalls++
	f.streamModels = append(f.streamModels, model)
	f.streamPrompts = append(f.streamPrompts, prompt)
	f.streamChats = append(f.streamChats, chatID)
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	fail := f.failChat
	if fail != nil && f.failChatID != "" && f.failChatID != chatID {
		fail = nil
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if fail != nil {
		return fail
	}
	if f.expired {
		return elmcore.ErrSessionExpired
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := onDelta(elmcore.DeltaThinking, "thinking"); err != nil {
		return err
	}
	if err := onDelta(elmcore.DeltaContent, "he"); err != nil {
		return err
	}
	if err := onDelta(elmcore.DeltaContent, "llo"); err != nil {
		return err
	}
	return nil
}

var testModels = []elmcore.Model{
	{ID: "Qwen/Qwen3.5-397B-A17B-FP8", Name: "Qwen 3.5 397B", Source: "VLLM", MaxTokens: 262144, IsEco: true, Active: true},
	{ID: "meta-llama/Llama-3.3-70B-Instruct", Name: "Llama 3.3", Source: "VLLM", MaxTokens: 131072, IsEco: true, Active: true},
	{ID: "claude-opus-5", Name: "Claude Opus 5", Source: "ANTHROPIC", MaxTokens: 200000, Active: true},
}

func newTestPool(t *testing.T, f *fakeUpstream, maxConc int) (*Pool, SessionView) {
	t.Helper()
	pool := NewPool(f, "", maxConc, 1, 60)
	view, err := pool.Add(context.Background(), "test", "SESSION=abc")
	if err != nil {
		t.Fatalf("add session: %v", err)
	}
	return pool, view
}

func TestBuildPrompt(t *testing.T) {
	prompt, err := buildPrompt([]chatMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}})
	if err != nil || prompt != "hello" {
		t.Fatalf("single user prompt = %q, %v", prompt, err)
	}
	prompt, err = buildPrompt([]chatMessage{
		{Role: "system", Content: json.RawMessage(`"be nice"`)},
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`"hey"`)},
		{Role: "user", Content: json.RawMessage(`"again"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"System: be nice", "User: hi", "Assistant: hey", "User: again"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q missing %q", prompt, want)
		}
	}
	if _, err := buildPrompt(nil); err == nil {
		t.Fatal("expected empty messages error")
	}
}

func TestResolveModel(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	ctx := context.Background()
	cases := map[string]string{
		"Qwen/Qwen3.5-397B-A17B-FP8": "Qwen/Qwen3.5-397B-A17B-FP8",
		"Qwen 3.5 397B":              "Qwen/Qwen3.5-397B-A17B-FP8",
		"llama-3.3-70b-instruct":     "meta-llama/Llama-3.3-70B-Instruct",
		"claude-opus-5":              "claude-opus-5",
	}
	for in, want := range cases {
		got, err := pool.ResolveModel(ctx, in)
		if err != nil || got != want {
			t.Fatalf("resolve %q = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := pool.ResolveModel(ctx, "nope"); err == nil {
		t.Fatal("expected unknown model error")
	}
}

func TestModelsForOpenAIFiltersInactive(t *testing.T) {
	models := append([]elmcore.Model(nil), testModels...)
	models = append(models, elmcore.Model{ID: "old-model", Name: "Old", Active: false})
	f := &fakeUpstream{loggedIn: true, models: models}
	pool, _ := newTestPool(t, f, 2)
	out, err := pool.ModelsForOpenAI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 active models, got %d", len(out))
	}
	for _, model := range out {
		if model["id"] == "old-model" {
			t.Fatal("inactive model leaked")
		}
	}
}

func TestPoolAddValidatesLogin(t *testing.T) {
	f := &fakeUpstream{loggedIn: false}
	pool := NewPool(f, "", 2, 1, 60)
	if _, err := pool.Add(context.Background(), "", "SESSION=x"); err == nil {
		t.Fatal("expected login failure")
	}
	f.loggedIn = true
	view, err := pool.Add(context.Background(), "", "SESSION=x")
	if err != nil {
		t.Fatal(err)
	}
	if view.Email == "" {
		t.Fatal("expected email")
	}
}

// One ELM account runs one chat at a time (a second conversation in the web UI
// waits for the first), so requests on the same cookie must queue instead of
// interleaving.
func TestPoolChatSerializesPerSession(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels, delay: 40 * time.Millisecond}
	pool, _ := newTestPool(t, f, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.Chat(context.Background(), "", "hi", func(elmcore.DeltaKind, string) error { return nil })
		}()
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxActive != 1 {
		t.Fatalf("max concurrent chats = %d, want 1 (ELM serializes per account)", f.maxActive)
	}
}

// Different cookies are different accounts, so they still run in parallel.
func TestPoolChatParallelAcrossSessions(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels, delay: 80 * time.Millisecond}
	pool := NewPool(f, "", 4, 5, 60)
	for _, cookie := range []string{"SESSION=a", "SESSION=b"} {
		if _, err := pool.Add(context.Background(), cookie, cookie); err != nil {
			t.Fatalf("add %s: %v", cookie, err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.Chat(context.Background(), "", "hi", func(elmcore.DeltaKind, string) error { return nil })
		}()
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxActive != 2 {
		t.Fatalf("max concurrent chats = %d, want 2", f.maxActive)
	}
}

func TestPoolChatSerializesModel(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels, delay: 10 * time.Millisecond}
	pool, _ := newTestPool(t, f, 2)
	_ = pool.Chat(context.Background(), "claude-opus-5", "hi", func(elmcore.DeltaKind, string) error { return nil })
	_ = pool.Chat(context.Background(), "Qwen/Qwen3.5-397B-A17B-FP8", "hi", func(elmcore.DeltaKind, string) error { return nil })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streamModels) < 2 {
		t.Fatalf("expected model switches recorded, got %#v", f.streamModels)
	}
	if f.streamModels[0] != "claude-opus-5" || f.streamModels[1] != "Qwen/Qwen3.5-397B-A17B-FP8" {
		t.Fatalf("unexpected model switches: %#v", f.streamModels)
	}
}

func TestServerModelsAndChat(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	cfg := DefaultConfig()
	cfg.APIKeys = nil
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(cfg, pool, f).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var models struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != 3 {
		t.Fatalf("models count = %d", len(models.Data))
	}

	body := `{"model":"qwen-3.5","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, err = http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var completion struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		t.Fatal(err)
	}
	if len(completion.Choices) == 0 || completion.Choices[0].Message.Content != "hello" {
		t.Fatalf("unexpected completion: %#v", completion)
	}
	if completion.Choices[0].Message.ReasoningContent != "thinking" {
		t.Fatalf("unexpected reasoning: %#v", completion)
	}
}

func TestServerStreaming(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	cfg := DefaultConfig()
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(cfg, pool, f).Handler())
	defer ts.Close()

	body := `{"model":"qwen-3.5","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	text := buf.String()
	for _, want := range []string{"chat.completion.chunk", `"content":"he"`, `"content":"llo"`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}
}

func TestServerAPIKeyRequired(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	cfg := DefaultConfig()
	cfg.APIKeys = []string{"sk-test"}
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(cfg, pool, f).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without key = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status with key = %d", resp.StatusCode)
	}
}

func TestServerLoginAndStatus(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	cfg := DefaultConfig()
	pool, _ := newTestPool(t, f, 2)
	ts := httptest.NewServer(NewServer(cfg, pool, f).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status["concurrency"] == nil {
		t.Fatalf("unexpected status: %#v", status)
	}
}
