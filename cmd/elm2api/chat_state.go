package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// maxChatsPerSession bounds how many ELM chats a session remembers. The web
	// history is the real context store, so a handful is enough for the
	// conversations an agent (or its subagents) keeps alive at the same time.
	maxChatsPerSession = 8
	// maxRememberedResponses bounds the previous_response_id -> ELM chat map.
	maxRememberedResponses = 512
)

// chatRecord is one ELM chat this session has written to. It stores only a
// length plus a hash of the last prompt (never the prompt itself), which is
// enough to tell whether a new prompt continues that conversation.
type chatRecord struct {
	ID       string
	Model    string
	PrevLen  int
	PrevHash string
	Opening  string
	Turns    int
	Updated  time.Time
}

// SessionChat is the JSON shape of one remembered chat.
type SessionChat struct {
	ChatID     string    `json:"chat_id"`
	Model      string    `json:"model,omitempty"`
	Opening    string    `json:"opening,omitempty"`
	Turns      int       `json:"turns"`
	Continuing bool      `json:"continuing"`
	Updated    time.Time `json:"updated"`
}

func promptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// openingPreview keeps the first line of a conversation for the dashboard.
func openingPreview(prompt string) string {
	line := strings.TrimSpace(prompt)
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	runes := []rune(line)
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return line
}

// matchChat finds the remembered chat whose last prompt is a prefix of prompt,
// i.e. the conversation the new request continues. The longest match wins, so
// several conversations can share one session without stealing each other's
// context. A prompt that matches nothing (an edit, a rollback, a new topic)
// must start a new chat: the ELM web chat cannot be rewound.
func (s *Session) matchChat(prompt string) *chatRecord {
	if prompt == "" {
		return nil
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	var best *chatRecord
	for _, rec := range s.chats {
		if rec.PrevLen == 0 || len(prompt) < rec.PrevLen {
			continue
		}
		if promptHash(prompt[:rec.PrevLen]) != rec.PrevHash {
			continue
		}
		if best == nil || rec.PrevLen > best.PrevLen {
			best = rec
		}
	}
	if best != nil {
		s.touchLocked(best)
	}
	return best
}

// noteChat lists a chat this session just used without claiming a prompt
// prefix (the Responses API continues a chat by id, not by prompt).
func (s *Session) noteChat(id, opening string) *chatRecord {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	for _, rec := range s.chats {
		if rec.ID == id {
			s.touchLocked(rec)
			if rec.Opening == "" && opening != "" {
				rec.Opening = openingPreview(opening)
			}
			return rec
		}
	}
	rec := &chatRecord{ID: id, Opening: openingPreview(opening), Updated: time.Now()}
	s.chats = append([]*chatRecord{rec}, s.chats...)
	if len(s.chats) > maxChatsPerSession {
		s.chats = s.chats[:maxChatsPerSession]
	}
	return rec
}

// rememberChat records that prompt was the last message sent into rec.
func (s *Session) rememberChat(rec *chatRecord, model, prompt string) {
	if rec == nil {
		return
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	rec.PrevLen = len(prompt)
	rec.PrevHash = promptHash(prompt)
	rec.Turns++
	if model != "" {
		rec.Model = model
	}
	if rec.Opening == "" {
		rec.Opening = openingPreview(prompt)
	}
	s.touchLocked(rec)
}

func (s *Session) touchLocked(rec *chatRecord) {
	rec.Updated = time.Now()
	for i, existing := range s.chats {
		if existing != rec {
			continue
		}
		copy(s.chats[1:i+1], s.chats[:i])
		s.chats[0] = rec
		return
	}
}

func (s *Session) primaryChatID() string {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if len(s.chats) == 0 {
		return ""
	}
	return s.chats[0].ID
}

func (s *Session) chatList() []SessionChat {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	out := make([]SessionChat, 0, len(s.chats))
	for _, rec := range s.chats {
		out = append(out, SessionChat{
			ChatID:     rec.ID,
			Model:      rec.Model,
			Opening:    rec.Opening,
			Turns:      rec.Turns,
			Continuing: rec.PrevLen > 0,
			Updated:    rec.Updated,
		})
	}
	return out
}

type persistedChat struct {
	ChatID   string `json:"chat_id"`
	Model    string `json:"model,omitempty"`
	PrevLen  int    `json:"prev_len,omitempty"`
	PrevHash string `json:"prev_hash,omitempty"`
	Opening  string `json:"opening,omitempty"`
	Turns    int    `json:"turns,omitempty"`
	Updated  string `json:"updated,omitempty"`
}

// persistedSession holds the chats of one session. The legacy single-chat
// fields are still read so an existing state file keeps working (and gets
// rewritten in the compact shape on the next save).
type persistedSession struct {
	Chats      []persistedChat `json:"chats,omitempty"`
	ChatID     string          `json:"chat_id,omitempty"`
	Model      string          `json:"model,omitempty"`
	LastPrompt string          `json:"last_prompt,omitempty"`
}

type persistedResponse struct {
	Session   string    `json:"session"`
	ChatID    string    `json:"chat_id"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type persistedState struct {
	Sessions  map[string]persistedSession  `json:"sessions"`
	Responses map[string]persistedResponse `json:"responses,omitempty"`
}

func (c persistedChat) record() *chatRecord {
	rec := &chatRecord{
		ID:       strings.TrimSpace(c.ChatID),
		Model:    c.Model,
		PrevLen:  c.PrevLen,
		PrevHash: c.PrevHash,
		Opening:  c.Opening,
		Turns:    c.Turns,
	}
	if rec.Updated, _ = time.Parse(time.RFC3339, c.Updated); rec.Updated.IsZero() {
		rec.Updated = time.Now()
	}
	if rec.PrevLen > 0 && rec.PrevHash == "" {
		// A hash is mandatory for matching; drop the prefix claim otherwise.
		rec.PrevLen = 0
	}
	return rec
}

func (ps persistedSession) records() []*chatRecord {
	var out []*chatRecord
	for _, c := range ps.Chats {
		if rec := c.record(); rec.ID != "" {
			out = append(out, rec)
		}
	}
	if len(out) > 0 {
		return out
	}
	if strings.TrimSpace(ps.ChatID) == "" {
		return nil
	}
	rec := &chatRecord{ID: strings.TrimSpace(ps.ChatID), Model: ps.Model, Updated: time.Now()}
	if ps.LastPrompt != "" {
		rec.PrevLen = len(ps.LastPrompt)
		rec.PrevHash = promptHash(ps.LastPrompt)
		rec.Opening = openingPreview(ps.LastPrompt)
	}
	return []*chatRecord{rec}
}

func (s *Session) persisted() persistedSession {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	out := persistedSession{Chats: make([]persistedChat, 0, len(s.chats))}
	for _, rec := range s.chats {
		out.Chats = append(out.Chats, persistedChat{
			ChatID:   rec.ID,
			Model:    rec.Model,
			PrevLen:  rec.PrevLen,
			PrevHash: rec.PrevHash,
			Opening:  rec.Opening,
			Turns:    rec.Turns,
			Updated:  rec.Updated.Format(time.RFC3339),
		})
	}
	return out
}

// SetStatePath enables persisting chat ids, so a restart (or a session that is
// logged in again later) resumes the same ELM conversation.
func (p *Pool) SetStatePath(path string) {
	p.stateMu.Lock()
	p.statePath = strings.TrimSpace(path)
	p.stateMu.Unlock()
}

func (p *Pool) stateFile() string {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.statePath
}

// LoadState restores remembered chats and response ids from an earlier run.
func (p *Pool) LoadState() {
	path := p.stateFile()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("elm2api: read chat state %s: %v", path, err)
		}
		return
	}
	var st persistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Printf("elm2api: parse chat state %s: %v", path, err)
		return
	}
	restored := 0
	p.mu.RLock()
	for _, s := range p.sessions {
		chats := st.Sessions[s.ID].records()
		if len(chats) == 0 {
			continue
		}
		s.chatMu.Lock()
		if len(s.chats) == 0 {
			s.chats = chats
			restored++
		}
		s.chatMu.Unlock()
	}
	p.mu.RUnlock()
	p.respMu.Lock()
	if p.responses == nil {
		p.responses = make(map[string]persistedResponse, len(st.Responses))
	}
	for id, ref := range st.Responses {
		if _, exists := p.responses[id]; !exists {
			p.responses[id] = ref
		}
	}
	p.respMu.Unlock()
	if restored > 0 {
		log.Printf("elm2api: restored %d ELM chat(s) from %s", restored, path)
	}
}

// SaveState writes every session's chats and the response id map.
func (p *Pool) SaveState() {
	path := p.stateFile()
	if path == "" {
		return
	}
	st := persistedState{Sessions: map[string]persistedSession{}}
	p.mu.RLock()
	for _, s := range p.sessions {
		if state := s.persisted(); len(state.Chats) > 0 {
			st.Sessions[s.ID] = state
		}
	}
	p.mu.RUnlock()
	p.respMu.Lock()
	if len(p.responses) > 0 {
		st.Responses = make(map[string]persistedResponse, len(p.responses))
		for id, ref := range p.responses {
			st.Responses[id] = ref
		}
	}
	p.respMu.Unlock()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		log.Printf("elm2api: encode chat state: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("elm2api: write chat state %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("elm2api: replace chat state %s: %v", path, err)
	}
}

// Response looks up which ELM chat a previous response id continues.
func (p *Pool) Response(id string) (persistedResponse, bool) {
	p.respMu.Lock()
	defer p.respMu.Unlock()
	ref, ok := p.responses[id]
	return ref, ok
}

// RememberResponse maps a response id to the ELM chat that answered it.
func (p *Pool) RememberResponse(id string, ref persistedResponse) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(ref.ChatID) == "" {
		return
	}
	p.respMu.Lock()
	if p.responses == nil {
		p.responses = map[string]persistedResponse{}
	}
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now()
	}
	p.responses[id] = ref
	p.pruneResponsesLocked()
	p.respMu.Unlock()
	p.SaveState()
}

func (p *Pool) pruneResponsesLocked() {
	if len(p.responses) <= maxRememberedResponses {
		return
	}
	list := make([]string, 0, len(p.responses))
	for id := range p.responses {
		list = append(list, id)
	}
	sort.Slice(list, func(i, j int) bool {
		return p.responses[list[i]].CreatedAt.Before(p.responses[list[j]].CreatedAt)
	})
	for _, id := range list[:len(list)-maxRememberedResponses] {
		delete(p.responses, id)
	}
}
