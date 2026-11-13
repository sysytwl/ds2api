package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"elm2api/internal/elmcore"
)

// Upstream is the ELM backend used by the pool (elmcore.Client in production).
type Upstream interface {
	IsLoggedIn(ctx context.Context, cookie string) (bool, error)
	User(ctx context.Context, cookie string) (elmcore.User, error)
	Quota(ctx context.Context, cookie string) (elmcore.Quota, error)
	Models(ctx context.Context, cookie string) ([]elmcore.Model, error)
	ChatID(ctx context.Context, cookie string) (string, error)
	NewChat(ctx context.Context, cookie string) (string, error)
	SetModel(ctx context.Context, cookie, model string) error
	CurrentModel(ctx context.Context, cookie string) (string, error)
	StreamChat(ctx context.Context, cookie, chatID, prompt, model string, onDelta func(elmcore.DeltaKind, string) error) error
}

var (
	// ErrNoSession means no ELM session is registered.
	ErrNoSession = errors.New("elm2api: no ELM session configured")
	// ErrBusy means the concurrency limit stayed saturated for too long.
	ErrBusy = errors.New("elm2api: all ELM chats are busy, try again later")
	// ErrModelNotFound means the requested model is not offered by ELM.
	ErrModelNotFound = errors.New("elm2api: model not found")
)

// Session is one logged-in ELM browser session.
type Session struct {
	ID      string
	Name    string
	Cookie  string
	User    elmcore.User
	Quota   elmcore.Quota
	Created time.Time
	modelMu sync.RWMutex
	model   string
	active  int64
	expired atomic.Bool

	// turn serializes every prompt that goes through this cookie: one ELM
	// account runs one chat at a time, so concurrent requests wait here
	// instead of interleaving inside the web chat.
	turn chan struct{}

	// The ELM chats this session has written to, most recent first. The web
	// history is the real context store; these records only remember which
	// chat a conversation continues.
	chatMu sync.Mutex
	chats  []*chatRecord
}

// newSession builds a session with an empty chat list and a free turn lock.
func newSession(id, name, cookie string) *Session {
	return &Session{
		ID:      id,
		Name:    name,
		Cookie:  cookie,
		Created: time.Now(),
		turn:    make(chan struct{}, 1),
	}
}

// acquireTurn blocks until this session is free. ELM serializes chats per
// account, so queueing here keeps requests ordered instead of failing them.
func (s *Session) acquireTurn(ctx context.Context) error {
	select {
	case s.turn <- struct{}{}:
		return nil
	default:
	}
	log.Printf("elm2api: session %s is busy, queueing request (one ELM account runs one chat at a time)", s.ID)
	select {
	case s.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) releaseTurn() { <-s.turn }

// Model returns the model last observed/selected for the session.
func (s *Session) Model() string {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	return s.model
}

func (s *Session) setModel(model string) {
	s.modelMu.Lock()
	s.model = model
	s.modelMu.Unlock()
}

// Expired reports whether the last upstream call proved the cookie dead.
func (s *Session) Expired() bool { return s.expired.Load() }

func (s *Session) markExpired(expired bool) { s.expired.Store(expired) }

// SessionView is the JSON shape exposed by the management API.
type SessionView struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Email   string        `json:"email"`
	Model   string        `json:"model"`
	Active  int64         `json:"active"`
	Created time.Time     `json:"created"`
	Quota   elmcore.Quota `json:"quota"`
	Expired bool          `json:"expired"`
	ChatID  string        `json:"chat_id,omitempty"`
	Chats   []SessionChat `json:"chats,omitempty"`
}

func sessionID(cookie string) string {
	sum := sha1.Sum([]byte(cookie))
	return hex.EncodeToString(sum[:6])
}

// Pool keeps ELM sessions, the model catalog and the concurrency budget.
type Pool struct {
	up           Upstream
	defaultModel string
	queueWait    time.Duration

	mu       sync.RWMutex
	sessions []*Session
	sem      chan struct{}

	modelsMu  sync.Mutex
	models    []elmcore.Model
	modelsAt  time.Time
	modelsTTL time.Duration

	stateMu   sync.Mutex
	statePath string

	respMu    sync.Mutex
	responses map[string]persistedResponse
}

// NewPool builds a session pool.
func NewPool(up Upstream, defaultModel string, maxConcurrency, queueWaitSeconds, modelTTLSeconds int) *Pool {
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	if queueWaitSeconds <= 0 {
		queueWaitSeconds = 30
	}
	if modelTTLSeconds <= 0 {
		modelTTLSeconds = 300
	}
	return &Pool{
		up:           up,
		defaultModel: strings.TrimSpace(defaultModel),
		queueWait:    time.Duration(queueWaitSeconds) * time.Second,
		sem:          make(chan struct{}, maxConcurrency),
		modelsTTL:    time.Duration(modelTTLSeconds) * time.Second,
	}
}

// Add validates an ELM cookie and registers it as a session.
func (p *Pool) Add(ctx context.Context, name, cookie string) (SessionView, error) {
	cookie = elmcore.NormalizeCookie(cookie)
	if cookie == "" {
		return SessionView{}, ErrNoSession
	}
	ok, err := p.up.IsLoggedIn(ctx, cookie)
	if err != nil {
		return SessionView{}, err
	}
	if !ok {
		return SessionView{}, elmcore.ErrSessionExpired
	}
	user, err := p.up.User(ctx, cookie)
	if err != nil {
		return SessionView{}, err
	}
	quota, _ := p.up.Quota(ctx, cookie)
	model, _ := p.up.CurrentModel(ctx, cookie)
	id := sessionID(cookie)
	if name = strings.TrimSpace(name); name == "" {
		name = user.Email
	}
	s := newSession(id, name, cookie)
	s.User = user
	s.Quota = quota
	s.model = model
	p.mu.Lock()
	for i, existing := range p.sessions {
		if existing.ID == id {
			existing.Name = name
			existing.User = user
			existing.Quota = quota
			existing.setModel(model)
			existing.markExpired(false)
			p.mu.Unlock()
			p.LoadState()
			return existing.view(), nil
		}
		p.sessions[i] = existing
	}
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()
	p.LoadState()
	return s.view(), nil
}

// addExpired keeps a session whose cookie already failed validation so the
// dashboard can show it and the operator can replace the cookie.
func (p *Pool) addExpired(name, cookie string) {
	cookie = elmcore.NormalizeCookie(cookie)
	if cookie == "" {
		return
	}
	id := sessionID(cookie)
	if name = strings.TrimSpace(name); name == "" {
		name = "expired-session"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, existing := range p.sessions {
		if existing.ID == id {
			existing.markExpired(true)
			return
		}
	}
	s := newSession(id, name, cookie)
	s.expired.Store(true)
	p.sessions = append(p.sessions, s)
}

// Remove drops a session by id.
func (p *Pool) Remove(id string) bool {
	p.mu.Lock()
	removed := false
	for i, s := range p.sessions {
		if s.ID == id {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			removed = true
			break
		}
	}
	p.mu.Unlock()
	if removed {
		p.SaveState()
	}
	return removed
}

// Views lists registered sessions.
func (p *Pool) Views() []SessionView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]SessionView, 0, len(p.sessions))
	for _, s := range p.sessions {
		out = append(out, s.view())
	}
	return out
}

func (s *Session) view() SessionView {
	return SessionView{
		ID:      s.ID,
		Name:    s.Name,
		Email:   s.User.Email,
		Model:   s.Model(),
		Active:  atomic.LoadInt64(&s.active),
		Created: s.Created,
		Quota:   s.Quota,
		Expired: s.Expired(),
		ChatID:  s.primaryChatID(),
		Chats:   s.chatList(),
	}
}

// Session returns a session by id.
func (p *Pool) Session(id string) (*Session, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, s := range p.sessions {
		if s.ID == id {
			return s, true
		}
	}
	return nil, false
}

// SetSessionModel switches the model of one session.
func (p *Pool) SetSessionModel(ctx context.Context, id, model string) (string, error) {
	s, ok := p.Session(id)
	if !ok {
		return "", ErrNoSession
	}
	resolved, err := p.ResolveModel(ctx, model)
	if err != nil {
		return "", err
	}
	if err := s.acquireTurn(ctx); err != nil {
		return "", err
	}
	defer s.releaseTurn()
	if err := p.up.SetModel(ctx, s.Cookie, resolved); err != nil {
		if errors.Is(err, elmcore.ErrSessionExpired) {
			s.markExpired(true)
		}
		return "", err
	}
	s.markExpired(false)
	s.setModel(resolved)
	return resolved, nil
}

// Models returns the ELM model catalog, cached for the configured TTL.
func (p *Pool) Models(ctx context.Context) ([]elmcore.Model, error) {
	p.modelsMu.Lock()
	if len(p.models) > 0 && time.Since(p.modelsAt) < p.modelsTTL {
		out := append([]elmcore.Model(nil), p.models...)
		p.modelsMu.Unlock()
		return out, nil
	}
	p.modelsMu.Unlock()

	sessions := p.snapshotSessions()
	if len(sessions) == 0 {
		return nil, ErrNoSession
	}
	var lastErr error
	for _, s := range sessions {
		models, err := p.up.Models(ctx, s.Cookie)
		if err != nil {
			if errors.Is(err, elmcore.ErrSessionExpired) {
				s.markExpired(true)
			}
			lastErr = err
			continue
		}
		s.markExpired(false)
		p.modelsMu.Lock()
		p.models = append([]elmcore.Model(nil), models...)
		p.modelsAt = time.Now()
		p.modelsMu.Unlock()
		return models, nil
	}
	return nil, fmt.Errorf("elm2api: list models: %w", lastErr)
}

// RefreshModels forces a catalog refresh.
func (p *Pool) RefreshModels(ctx context.Context) ([]elmcore.Model, error) {
	p.modelsMu.Lock()
	p.modelsAt = time.Time{}
	p.modelsMu.Unlock()
	return p.Models(ctx)
}

// ResolveModel maps a client model name to an ELM model id.
func (p *Pool) ResolveModel(ctx context.Context, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	models, err := p.Models(ctx)
	if err != nil {
		return "", err
	}
	if requested == "" {
		requested = p.defaultModel
	}
	if requested == "" {
		if len(models) > 0 {
			return models[0].ID, nil
		}
		return "", ErrModelNotFound
	}
	for _, m := range models {
		if m.ID == requested {
			return m.ID, nil
		}
	}
	norm := normalizeModelName(requested)
	for _, m := range models {
		if normalizeModelName(m.ID) == norm || normalizeModelName(m.Name) == norm {
			return m.ID, nil
		}
	}
	for _, m := range models {
		if strings.EqualFold(m.ID, requested) || strings.EqualFold(m.Name, requested) {
			return m.ID, nil
		}
	}
	// Convenience aliases: accept a normalized prefix such as "qwen-3.5".
	var prefixMatches []elmcore.Model
	for _, m := range models {
		idNorm := normalizeModelName(m.ID)
		nameNorm := normalizeModelName(m.Name)
		if len(norm) >= 4 && (strings.HasPrefix(idNorm, norm) || strings.HasPrefix(nameNorm, norm)) {
			prefixMatches = append(prefixMatches, m)
		}
	}
	if len(prefixMatches) > 0 {
		best := prefixMatches[0]
		for _, m := range prefixMatches[1:] {
			if len(normalizeModelName(m.ID)) < len(normalizeModelName(best.ID)) {
				best = m
			}
		}
		return best.ID, nil
	}
	return "", fmt.Errorf("%w: %q (see GET /v1/models)", ErrModelNotFound, requested)
}

func normalizeModelName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	replacer := strings.NewReplacer(" ", "", "-", "", "_", "", ".", "")
	return replacer.Replace(name)
}

// ModelsForOpenAI lists models in OpenAI shape.
func (p *Pool) ModelsForOpenAI(ctx context.Context) ([]map[string]any, error) {
	models, err := p.Models(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if !m.Active {
			continue
		}
		out = append(out, map[string]any{
			"id":                m.ID,
			"object":            "model",
			"created":           0,
			"owned_by":          strings.ToLower(m.Source),
			"name":              m.Name,
			"context_length":    m.MaxTokens,
			"max_tokens":        m.MaxTokens,
			"eco":               m.IsEco,
			"source":            m.Source,
			"reasoning":         m.ReasoningEfforts,
			"reasoning_default": m.ReasoningDefault,
		})
	}
	return out, nil
}

func (p *Pool) snapshotSessions() []*Session {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*Session(nil), p.sessions...)
}

// pick chooses the least busy session, preferring one already on the model.
func (p *Pool) pick(model string) *Session {
	sessions := p.snapshotSessions()
	if len(sessions) == 0 {
		return nil
	}
	var best *Session
	bestActive := int64(1 << 62)
	bestMatches := false
	for _, s := range sessions {
		active := atomic.LoadInt64(&s.active)
		matches := model != "" && s.Model() == model
		if best == nil || (matches && !bestMatches) || (matches == bestMatches && active < bestActive) {
			best = s
			bestActive = active
			bestMatches = matches
		}
	}
	return best
}

// Chat runs one prompt for the chat-completions surface. The prompt usually
// repeats the whole conversation, so it is matched against the chats this
// session already has: an extending prompt only sends the new turn, an edited
// or unrelated prompt starts a new ELM chat (the web chat cannot be rewound).
func (p *Pool) Chat(ctx context.Context, model, prompt string, onDelta func(elmcore.DeltaKind, string) error) error {
	return p.ChatTail(ctx, model, prompt, "", onDelta)
}

// ChatTail is Chat with a tail block: when the prompt is too long for one ELM
// message it is split, and tail is repeated in the final chunk. The tool
// protocol only exists at the top of the prompt, so without the repeat a long
// conversation reaches the model with no tool definitions in front of it.
func (p *Pool) ChatTail(ctx context.Context, model, prompt, tail string, onDelta func(elmcore.DeltaKind, string) error) error {
	return p.withSession(ctx, model, func(s *Session) error {
		rec, sendPrompt, err := p.prepareChat(ctx, s, prompt)
		if err != nil {
			return err
		}
		emitted := false
		wrap := func(kind elmcore.DeltaKind, text string) error {
			emitted = true
			return onDelta(kind, text)
		}
		err = p.sendPrompt(ctx, s, rec.ID, sendPrompt, tail, model, wrap)
		if err != nil && !emitted && rec.PrevLen > 0 {
			// The remembered chat is gone (deleted in the web UI) or unusable;
			// start a fresh one instead of failing the client.
			id, chatErr := p.openChat(ctx, s)
			if chatErr != nil {
				return err
			}
			log.Printf("elm2api: chat %s unusable (%v), opened %s instead", rec.ID, err, id)
			rec = s.noteChat(id, prompt)
			err = p.sendPrompt(ctx, s, id, prompt, tail, model, wrap)
		}
		if err != nil {
			return err
		}
		s.rememberChat(rec, model, prompt)
		return nil
	})
}

// ChatIn runs one prompt in a specific ELM chat (empty chatID opens a new one)
// and returns the chat id plus the session id that served it. It backs the
// Responses API, where the conversation is continued by id instead of by
// resending the prompt.
func (p *Pool) ChatIn(ctx context.Context, model, prompt, chatID string, onDelta func(elmcore.DeltaKind, string) error) (string, string, error) {
	return p.ChatInTail(ctx, model, prompt, chatID, "", onDelta)
}

// ChatInTail is ChatIn with the same tail repeat as ChatTail.
func (p *Pool) ChatInTail(ctx context.Context, model, prompt, chatID, tail string, onDelta func(elmcore.DeltaKind, string) error) (string, string, error) {
	used := strings.TrimSpace(chatID)
	sessionID := ""
	err := p.withSession(ctx, model, func(s *Session) error {
		sessionID = s.ID
		emitted := false
		wrap := func(kind elmcore.DeltaKind, text string) error {
			emitted = true
			return onDelta(kind, text)
		}
		if used == "" {
			id, err := p.openChat(ctx, s)
			if err != nil {
				return err
			}
			used = id
			s.noteChat(id, prompt)
		} else {
			s.noteChat(used, "")
		}
		err := p.sendPrompt(ctx, s, used, prompt, tail, model, wrap)
		if err != nil && !emitted {
			id, chatErr := p.openChat(ctx, s)
			if chatErr != nil {
				return err
			}
			log.Printf("elm2api: chat %s unusable (%v), opened %s instead", used, err, id)
			used = id
			s.noteChat(id, prompt)
			err = p.sendPrompt(ctx, s, id, prompt, tail, model, wrap)
		}
		return err
	})
	return used, sessionID, err
}

// withSession runs fn with a concurrency slot, a chosen session and that
// session's turn lock held.
func (p *Pool) withSession(ctx context.Context, model string, fn func(*Session) error) error {
	if err := p.acquireSlot(ctx); err != nil {
		return err
	}
	defer p.releaseSlot()
	s := p.pick(model)
	if s == nil {
		return ErrNoSession
	}
	atomic.AddInt64(&s.active, 1)
	defer atomic.AddInt64(&s.active, -1)
	if err := s.acquireTurn(ctx); err != nil {
		return err
	}
	defer s.releaseTurn()
	err := fn(s)
	p.SaveState()
	return err
}

func (p *Pool) acquireSlot(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(p.queueWait)
	defer timer.Stop()
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrBusy
	}
}

func (p *Pool) releaseSlot() { <-p.sem }

// prepareChat picks the chat a chat-completions prompt continues.
func (p *Pool) prepareChat(ctx context.Context, s *Session, prompt string) (*chatRecord, string, error) {
	if rec := s.matchChat(prompt); rec != nil {
		send := prompt
		if len(prompt) > rec.PrevLen {
			if tail := strings.TrimLeft(prompt[rec.PrevLen:], " \t\r\n"); strings.TrimSpace(tail) != "" {
				send = tail
			}
		}
		return rec, send, nil
	}
	id, err := p.openChat(ctx, s)
	if err != nil {
		return nil, "", err
	}
	log.Printf("elm2api: prompt does not continue a known chat, opening ELM chat %s for session %s", id, s.ID)
	return s.noteChat(id, prompt), prompt, nil
}

// openChat asks ELM for a fresh chat id, falling back to the session's current
// chat when the endpoint refuses.
func (p *Pool) openChat(ctx context.Context, s *Session) (string, error) {
	id, err := p.up.NewChat(ctx, s.Cookie)
	if err == nil {
		return id, nil
	}
	if errors.Is(err, elmcore.ErrSessionExpired) {
		s.markExpired(true)
		return "", err
	}
	fallback, fallbackErr := p.up.ChatID(ctx, s.Cookie)
	if fallbackErr != nil {
		if errors.Is(fallbackErr, elmcore.ErrSessionExpired) {
			s.markExpired(true)
		}
		return "", fallbackErr
	}
	return fallback, nil
}

// sendPrompt writes one prompt into chatID, splitting it when it exceeds the
// ELM message limit. Only the final chunk's deltas reach onDelta.
func (p *Pool) sendPrompt(ctx context.Context, s *Session, chatID, prompt, tail, model string, onDelta func(elmcore.DeltaKind, string) error) error {
	sendModel := ""
	if model != "" && s.Model() != model {
		sendModel = model
	}
	chunks := splitPromptWithTail(prompt, elmMaxPromptChars-len(splitContextPrefix), tail)
	if len(chunks) > 1 {
		log.Printf("elm2api: prompt split into %d messages (%d chars, tail %d chars, ELM limit ~%d)", len(chunks), len(prompt), len(tail), elmMaxPromptChars)
	}
	for i, chunk := range chunks {
		last := i == len(chunks)-1
		body := chunk
		if !last {
			body = splitContextPrefix + chunk
		}
		sendThis := sendModel
		if i > 0 {
			sendThis = ""
		}
		err := p.up.StreamChat(ctx, s.Cookie, chatID, body, sendThis, func(kind elmcore.DeltaKind, text string) error {
			if !last {
				return nil
			}
			return onDelta(kind, text)
		})
		if err != nil {
			if errors.Is(err, elmcore.ErrSessionExpired) {
				s.markExpired(true)
			}
			return err
		}
	}
	s.markExpired(false)
	if strings.TrimSpace(model) != "" && s.Model() != model {
		s.setModel(model)
	}
	return nil
}

// splitContextPrefix marks intermediate context messages so the model answers
// them briefly instead of producing a full answer.
const splitContextPrefix = "（以下是对话上下文的一部分，请只回复 OK，不要展开）\n"

// splitPrompt breaks an oversized prompt into ELM-sized messages, preferring
// line boundaries and never splitting a UTF-8 rune.
func splitPrompt(prompt string, limit int) []string {
	if limit <= 0 || len(prompt) <= limit {
		return []string{prompt}
	}
	var chunks []string
	remaining := prompt
	for len(remaining) > limit {
		cut := strings.LastIndex(remaining[:limit], "\n")
		if cut <= 0 {
			cut = safeCut(remaining, limit)
		} else {
			cut++
		}
		chunks = append(chunks, remaining[:cut])
		remaining = remaining[cut:]
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

// splitPromptWithTail splits prompt into ELM-sized chunks and repeats tail at
// the start of the final chunk, which is the only one the model answers. The
// prompt is re-cut with room reserved so the repeated block still fits.
func splitPromptWithTail(prompt string, limit int, tail string) []string {
	tail = strings.TrimSpace(tail)
	chunks := splitPrompt(prompt, limit)
	if tail == "" || len(chunks) < 2 || len(tail)+2 >= limit {
		return chunks
	}
	if last := chunks[len(chunks)-1]; len(last)+len(tail)+2 > limit {
		chunks = splitPrompt(prompt, limit-len(tail)-2)
	}
	last := chunks[len(chunks)-1]
	chunks[len(chunks)-1] = tail + "\n\n" + last
	return chunks
}

// safeCut returns the largest offset <= n that does not split a rune.
func safeCut(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if n == 0 {
		return len(s)
	}
	return n
}
