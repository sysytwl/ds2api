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

	"ds2api/internal/elmcore"
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
	ID       string
	Name     string
	Cookie   string
	User     elmcore.User
	Quota    elmcore.Quota
	Created  time.Time
	SwitchMu sync.Mutex // held until a chat's model is fixed (first delta)
	modelMu  sync.RWMutex
	model    string
	active   int64
	expired  atomic.Bool
}

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
	s := &Session{ID: id, Name: name, Cookie: cookie, User: user, Quota: quota, Created: time.Now(), model: model}
	p.mu.Lock()
	for i, existing := range p.sessions {
		if existing.ID == id {
			existing.Name = name
			existing.User = user
			existing.Quota = quota
			existing.setModel(model)
			existing.markExpired(false)
			p.mu.Unlock()
			return existing.view(), nil
		}
		p.sessions[i] = existing
	}
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()
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
	s := &Session{ID: id, Name: name, Cookie: cookie, Created: time.Now()}
	s.expired.Store(true)
	p.sessions = append(p.sessions, s)
}

// Remove drops a session by id.
func (p *Pool) Remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, s := range p.sessions {
		if s.ID == id {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			return true
		}
	}
	return false
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
	s.SwitchMu.Lock()
	defer s.SwitchMu.Unlock()
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

// Chat runs one prompt through the pool with concurrency limits and per-session
// model switching. The session switch lock is held only until the first streamed
// delta, so same-model chats run in parallel and different models serialize just
// the switch window.
func (p *Pool) Chat(ctx context.Context, model, prompt string, onDelta func(elmcore.DeltaKind, string) error) error {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		timer := time.NewTimer(p.queueWait)
		defer timer.Stop()
		select {
		case p.sem <- struct{}{}:
			defer func() { <-p.sem }()
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrBusy
		}
	}
	s := p.pick(model)
	if s == nil {
		return ErrNoSession
	}
	atomic.AddInt64(&s.active, 1)
	defer atomic.AddInt64(&s.active, -1)

	// A fresh chat per request keeps API traffic out of the chat the user has
	// open in the browser and stops concurrent requests from sharing one thread.
	chatID, err := p.up.NewChat(ctx, s.Cookie)
	if err != nil {
		if errors.Is(err, elmcore.ErrSessionExpired) {
			s.markExpired(true)
			return err
		}
		chatID, err = p.up.ChatID(ctx, s.Cookie)
		if err != nil {
			if errors.Is(err, elmcore.ErrSessionExpired) {
				s.markExpired(true)
			}
			return err
		}
	}

	s.SwitchMu.Lock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			s.SwitchMu.Unlock()
		}
	}
	sendModel := model
	if sendModel != "" && s.Model() == sendModel {
		sendModel = ""
	}
	chunks := splitPrompt(prompt, elmMaxPromptChars-len(splitContextPrefix))
	if len(chunks) > 1 {
		log.Printf("elm2api: prompt split into %d messages (%d chars, ELM limit ~%d)", len(chunks), len(prompt), elmMaxPromptChars)
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
		err = p.up.StreamChat(ctx, s.Cookie, chatID, body, sendThis, func(kind elmcore.DeltaKind, text string) error {
			unlock()
			if !last {
				return nil
			}
			return onDelta(kind, text)
		})
		unlock()
		if err != nil {
			break
		}
	}
	if err != nil {
		if errors.Is(err, elmcore.ErrSessionExpired) {
			s.markExpired(true)
		}
		return err
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
