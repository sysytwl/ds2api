package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"elm2api/internal/elmcore"
)

const maxRequestBody = 8 << 20

// Server exposes the ELM web chat as an OpenAI-compatible service.
type Server struct {
	pool *Pool
	cfg  Config
	keys map[string]struct{}
	up   Upstream
	tmpl *template.Template
}

// NewServer builds the HTTP layer.
func NewServer(cfg Config, pool *Pool, up Upstream) *Server {
	keys := make(map[string]struct{}, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		keys[key] = struct{}{}
	}
	return &Server{pool: pool, cfg: cfg, keys: keys, up: up, tmpl: template.Must(template.New("page").Parse(indexHTML))}
}

// Handler returns the full router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	// OpenAI-compatible surface. The same endpoints are served with and
	// without the /v1 prefix so any client base_url works, and a trailing
	// slash is tolerated (some clients append one).
	for _, prefix := range []string{"", "/v1"} {
		mux.HandleFunc("GET "+prefix+"/models", s.requireKey(s.handleModels))
		mux.HandleFunc("GET "+prefix+"/models/{id}", s.requireKey(s.handleModelByID))
		mux.HandleFunc("POST "+prefix+"/chat/completions", s.requireKey(s.handleChat))
		mux.HandleFunc("POST "+prefix+"/responses", s.requireKey(s.handleResponses))
	}
	mux.HandleFunc("GET /api/status", s.loopback(s.requireKey(s.handleStatus)))
	mux.HandleFunc("GET /api/sessions", s.loopback(s.requireKey(s.handleSessions)))
	mux.HandleFunc("POST /api/sessions", s.loopback(s.requireKey(s.handleAddSession)))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.loopback(s.requireKey(s.handleRemoveSession)))
	mux.HandleFunc("PUT /api/sessions/{id}/model", s.loopback(s.requireKey(s.handleSwitchModel)))
	mux.HandleFunc("POST /api/models/refresh", s.loopback(s.requireKey(s.handleRefreshModels)))
	// The catch-all has no method so an unknown path answers with JSON (and the
	// list of real endpoints) instead of net/http's bare 405/404.
	mux.HandleFunc("/", s.loopback(s.requireKey(s.handleIndex)))
	mux.HandleFunc("POST /ui/login", s.loopback(s.requireKey(s.handleUILogin)))
	mux.HandleFunc("POST /ui/switch", s.loopback(s.requireKey(s.handleUISwitch)))
	mux.HandleFunc("POST /ui/remove", s.loopback(s.requireKey(s.handleUIRemove)))
	return logRequests(withCORS(trimTrailingSlash(mux)))
}

func (s *Server) requireKey(next http.HandlerFunc) http.HandlerFunc {
	if len(s.keys) == 0 {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			key = strings.TrimSpace(r.URL.Query().Get("key"))
		}
		if _, ok := s.keys[key]; !ok {
			writeJSON(w, http.StatusUnauthorized, errorPayload("invalid API key", "invalid_api_key"))
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

func (s *Server) loopback(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !isLoopback(host) {
			writeJSON(w, http.StatusForbidden, errorPayload("management endpoints are localhost only", "forbidden"))
			return
		}
		next(w, r)
	}
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// trimTrailingSlash rewrites "/v1/models/" to "/v1/models" so clients that
// append a slash keep working without an extra redirect round-trip.
func trimTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if len(path) > 1 && strings.HasSuffix(path, "/") {
			trimmed := strings.TrimRight(path, "/")
			if trimmed == "" {
				trimmed = "/"
			}
			clone := r.Clone(r.Context())
			u := *r.URL
			u.Path = trimmed
			clone.URL = &u
			next.ServeHTTP(w, clone)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withCORS lets browser-based OpenAI clients call the local service directly.
// Only the chat surface gets CORS, never the loopback management endpoints.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1") || strings.HasPrefix(r.URL.Path, "/models") || strings.HasPrefix(r.URL.Path, "/chat/") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response code so the request log shows it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func isAPIPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/api/") ||
		strings.HasPrefix(path, "/models") || strings.HasPrefix(path, "/chat/")
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		if isAPIPath(r.URL.Path) {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

// writeUpstreamError renders an actionable error for the client, including the
// cookie-renewal hint when the ELM session is gone.
func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	code := "upstream_error"
	payload := errorPayload(err.Error(), code)
	switch {
	case errors.Is(err, elmcore.ErrSessionExpired):
		status = http.StatusUnauthorized
		code = "elm_session_expired"
		payload = errorPayload(err.Error(), code)
		payload["hint"] = cookieHelp(s.cfg.Listen)
		s.logExpiredSessions()
	case errors.Is(err, ErrNoSession):
		status = http.StatusServiceUnavailable
		code = "elm_session_missing"
		payload = errorPayload(err.Error(), code)
		payload["hint"] = cookieHelp(s.cfg.Listen)
	case errors.Is(err, ErrBusy):
		status = http.StatusTooManyRequests
		payload = errorPayload(err.Error(), "busy")
	}
	writeJSON(w, status, payload)
}

func (s *Server) logExpiredSessions() {
	for _, view := range s.pool.Views() {
		if view.Expired {
			logExpired(view.Name, view.Email, s.cfg.Listen)
		}
	}
}

func (s *Server) warnings() []string {
	views := s.pool.Views()
	out := []string{}
	expired := 0
	for _, view := range views {
		if view.Expired {
			expired++
			label := view.Name
			if strings.TrimSpace(view.Email) != "" {
				label += "（" + view.Email + "）"
			}
			out = append(out, fmt.Sprintf("会话 %q 的 ELM SESSION cookie 已过期/失效，请更新 cookie", label))
		}
	}
	if len(views) == 0 {
		out = append(out, "当前没有已登录的 ELM 会话，请用 SESSION cookie 登录")
	} else if expired == len(views) {
		out = append(out, "所有 ELM 会话都已失效，服务会在 cookie 更新前拒绝请求")
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func timeNowUnix() int64 { return time.Now().Unix() }

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.pool.ModelsForOpenAI(r.Context())
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleModelByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	models, err := s.pool.ModelsForOpenAI(r.Context())
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	for _, model := range models {
		if model["id"] == id {
			writeJSON(w, http.StatusOK, model)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, errorPayload("model not found", "model_not_found"))
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("read body: "+err.Error(), "invalid_request"))
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid JSON: "+err.Error(), "invalid_request"))
		return
	}
	resolved, err := s.pool.ResolveModel(r.Context(), req.Model)
	if err != nil {
		if errors.Is(err, ErrModelNotFound) {
			writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "model_not_found"))
		} else {
			s.writeUpstreamError(w, err)
		}
		return
	}
	tools := parseTools(req.Tools)
	choice, forced := toolChoice(req.ToolChoice)
	opt := promptOptions{Tools: tools, Choice: choice, Forced: forced}
	prompt, err := buildPromptWithOptions(req.Messages, opt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "invalid_request"))
		return
	}
	toolsEnabled := len(tools) > 0 && choice != "none"
	displayModel := strings.TrimSpace(req.Model)
	if displayModel == "" {
		displayModel = resolved
	}
	id := newCompletionID()
	created := timeNowUnix()
	tail := continuationTail(opt)
	if req.Stream {
		if toolsEnabled {
			s.streamToolCompletion(w, r, id, displayModel, resolved, prompt, tail, created, tools)
			return
		}
		s.streamCompletion(w, r, id, displayModel, resolved, prompt, tail, created)
		return
	}
	var content, thinking strings.Builder
	err = s.pool.ChatTail(r.Context(), resolved, prompt, tail, func(kind elmcore.DeltaKind, text string) error {
		if kind == elmcore.DeltaThinking {
			thinking.WriteString(text)
		} else {
			content.WriteString(text)
		}
		return nil
	})
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	if content.Len() == 0 && thinking.Len() == 0 {
		s.writeEmptyReplyError(w, len(prompt))
		return
	}
	if toolsEnabled {
		calls, rest := parseToolCalls(content.String(), tools)
		log.Printf("elm2api: tools=%d prompt=%d reply=%d tool_calls=%d", len(tools), len(prompt), content.Len(), len(calls))
		if len(calls) > 0 {
			writeJSON(w, http.StatusOK, toolCompletionPayload(id, displayModel, calls, rest))
			return
		}
	}
	writeJSON(w, http.StatusOK, completionPayload(id, displayModel, content.String(), thinking.String()))
}

// streamToolCompletion buffers the reply, because a text tool call can only be
// recognised once the whole JSON block arrived. The buffered text is then
// replayed as one delta, or converted into OpenAI tool_calls.
func (s *Server) streamToolCompletion(w http.ResponseWriter, r *http.Request, id, displayModel, resolved, prompt, tail string, created int64, tools []toolSpec) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorPayload("streaming unsupported", "internal_error"))
		return
	}
	var content, thinking strings.Builder
	err := s.pool.ChatTail(r.Context(), resolved, prompt, tail, func(kind elmcore.DeltaKind, text string) error {
		if kind == elmcore.DeltaThinking {
			thinking.WriteString(text)
			return nil
		}
		content.WriteString(text)
		return nil
	})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.writeUpstreamError(w, err)
		return
	}
	if content.Len() == 0 && thinking.Len() == 0 {
		s.writeEmptyReplyError(w, len(prompt))
		return
	}
	emit := func(payload map[string]any) {
		if writeErr := ssePayload(w, payload); writeErr != nil {
			log.Printf("stream write failed: %v", writeErr)
		}
		flusher.Flush()
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if thinking.Len() > 0 {
		emit(chunkPayload(id, displayModel, created, map[string]any{"role": "assistant", "reasoning_content": thinking.String()}))
	}
	calls, rest := parseToolCalls(content.String(), tools)
	log.Printf("elm2api: tools=%d prompt=%d reply=%d tool_calls=%d", len(tools), len(prompt), content.Len(), len(calls))
	if len(calls) > 0 {
		delta := map[string]any{"role": "assistant", "content": nil, "tool_calls": streamingToolCalls(calls)}
		if strings.TrimSpace(rest) != "" {
			delta["content"] = rest
		}
		emit(chunkPayload(id, displayModel, created, delta))
		emit(finishChunkPayload(id, displayModel, created, "tool_calls"))
	} else {
		emit(chunkPayload(id, displayModel, created, map[string]any{"role": "assistant", "content": content.String()}))
		emit(finishChunkPayload(id, displayModel, created, "stop"))
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, id, displayModel, resolved, prompt, tail string, created int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorPayload("streaming unsupported", "internal_error"))
		return
	}
	var contentLen, thinkingLen int
	started := false
	emit := func(payload map[string]any) {
		if err := ssePayload(w, payload); err != nil {
			log.Printf("stream write failed: %v", err)
		}
		flusher.Flush()
	}
	// Headers are only sent once the upstream actually produced something, so
	// an upstream failure can still be reported as a real HTTP error instead of
	// an empty 200 stream that clients read as "no content".
	start := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		emit(chunkPayload(id, displayModel, created, map[string]any{"role": "assistant", "content": ""}))
	}

	err := s.pool.ChatTail(r.Context(), resolved, prompt, tail, func(kind elmcore.DeltaKind, text string) error {
		start()
		if kind == elmcore.DeltaThinking {
			thinkingLen += len(text)
			emit(chunkPayload(id, displayModel, created, map[string]any{"reasoning_content": text}))
			return nil
		}
		contentLen += len(text)
		emit(chunkPayload(id, displayModel, created, map[string]any{"content": text}))
		return nil
	})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		log.Printf("stream chat failed: %v", err)
		if !started {
			s.writeUpstreamError(w, err)
			return
		}
		payload := errorPayload(err.Error(), "upstream_error")
		if errors.Is(err, elmcore.ErrSessionExpired) {
			payload = errorPayload(err.Error(), "elm_session_expired")
			payload["hint"] = cookieHelp(s.cfg.Listen)
			s.logExpiredSessions()
		}
		emit(payload)
		return
	}
	if contentLen == 0 && thinkingLen == 0 {
		if !started {
			s.writeEmptyReplyError(w, len(prompt))
			return
		}
		emit(emptyReplyPayload(len(prompt)))
		return
	}
	start()
	emit(finishChunkPayload(id, displayModel, created, "stop"))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// elmMaxPromptChars is the practical size limit of one ELM web-chat message.
// Bigger prompts are rejected upstream and come back as an empty reply.
const elmMaxPromptChars = 16000

// emptyReplyPayload is returned when ELM finishes a chat without any text.
func emptyReplyPayload(promptLen int) map[string]any {
	if promptLen > elmMaxPromptChars {
		payload := errorPayload(fmt.Sprintf("prompt is too long for the ELM web chat (%d chars, limit about %d)", promptLen, elmMaxPromptChars), "prompt_too_long")
		payload["hint"] = fmt.Sprintf("ELM 网页端单条消息上限约 %d 字符，当前 prompt 有 %d 字符；请精简 system prompt 或历史消息后重试", elmMaxPromptChars, promptLen)
		return payload
	}
	payload := errorPayload("ELM returned an empty reply", "empty_upstream_response")
	payload["hint"] = "ELM 网页端偶发返回空回复（通常是上游 websocket 被临时断开），直接重试即可；若持续出现请检查会话是否被网页端占用"
	return payload
}

func (s *Server) writeEmptyReplyError(w http.ResponseWriter, promptLen int) {
	status := http.StatusBadGateway
	if promptLen > elmMaxPromptChars {
		status = http.StatusRequestEntityTooLarge
	}
	writeJSON(w, status, emptyReplyPayload(promptLen))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	models, _ := s.pool.Models(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"default_model": s.cfg.DefaultModel,
		"listen":        s.cfg.Listen,
		"concurrency": map[string]any{
			"max":         cap(s.pool.sem),
			"in_use":      len(s.pool.sem),
			"per_account": 1,
		},
		"sessions": s.pool.Views(),
		"models":   len(models),
		"warnings": s.warnings(),
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.pool.Views()})
}

func (s *Server) handleAddSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid JSON: "+err.Error(), "invalid_request"))
		return
	}
	view, err := s.pool.Add(r.Context(), req.Name, req.Cookie)
	if err != nil {
		if errors.Is(err, elmcore.ErrSessionExpired) {
			logExpired(strings.TrimSpace(req.Name), "", s.cfg.Listen)
			payload := errorPayload(err.Error(), "elm_session_expired")
			payload["hint"] = cookieHelp(s.cfg.Listen)
			writeJSON(w, http.StatusUnauthorized, payload)
			return
		}
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "login_failed"))
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) handleRemoveSession(w http.ResponseWriter, r *http.Request) {
	if !s.pool.Remove(r.PathValue("id")) {
		writeJSON(w, http.StatusNotFound, errorPayload("session not found", "not_found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSwitchModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid JSON: "+err.Error(), "invalid_request"))
		return
	}
	model, err := s.pool.SetSessionModel(r.Context(), r.PathValue("id"), req.Model)
	if err != nil {
		if errors.Is(err, elmcore.ErrSessionExpired) {
			s.writeUpstreamError(w, err)
			return
		}
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "switch_failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model})
}

func (s *Server) handleRefreshModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.pool.RefreshModels(r.Context())
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": len(models)})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{
				"code":    "not_found",
				"message": "no such endpoint: " + r.Method + " " + r.URL.Path,
				"type":    "invalid_request_error",
			},
			"hint": "OpenAI base_url = http://" + displayListen(s.cfg.Listen) + "/v1 （带不带 /v1 都可以）；" +
				"可用接口：GET /v1/models、POST /v1/chat/completions、POST /v1/responses、GET /api/status",
		})
		return
	}
	models, _ := s.pool.Models(r.Context())
	data := map[string]any{
		"Sessions":   s.pool.Views(),
		"Models":     models,
		"MaxConc":    cap(s.pool.sem),
		"InUse":      len(s.pool.sem),
		"Default":    s.cfg.DefaultModel,
		"BaseURL":    s.cfg.BaseURL,
		"KeyQuery":   keyQuery(r),
		"HasAPIKeys": len(s.keys) > 0,
		"Warnings":   s.warnings(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("render index: %v", err)
	}
}

func keyQuery(r *http.Request) string {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		return ""
	}
	return "?key=" + key
}

func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err := s.pool.Add(r.Context(), r.PostFormValue("name"), r.PostFormValue("cookie"))
	if err != nil {
		http.Error(w, "login failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/"+keyQuery(r), http.StatusSeeOther)
}

func (s *Server) handleUISwitch(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.pool.SetSessionModel(r.Context(), r.PostFormValue("id"), r.PostFormValue("model")); err != nil {
		http.Error(w, "switch failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/"+keyQuery(r), http.StatusSeeOther)
}

func (s *Server) handleUIRemove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.pool.Remove(r.PostFormValue("id"))
	http.Redirect(w, r, "/"+keyQuery(r), http.StatusSeeOther)
}

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>ELM Web → API</title>
<style>
:root { color-scheme: dark; }
body { font-family: system-ui, -apple-system, "Segoe UI", sans-serif; margin: 24px; background: #12161c; color: #e6e6e6; }
h1 { font-size: 20px; margin: 0 0 16px; }
h2 { font-size: 15px; margin: 24px 0 8px; color: #9fd3ff; }
table { border-collapse: collapse; width: 100%; font-size: 13px; }
th, td { border-bottom: 1px solid #263040; padding: 6px 8px; text-align: left; vertical-align: top; }
th { color: #8aa0b8; font-weight: 600; }
code { background: #1b2430; padding: 1px 5px; border-radius: 4px; }
form { display: inline; }
input, select { background: #1b2430; color: #e6e6e6; border: 1px solid #33465e; border-radius: 6px; padding: 5px 8px; font-size: 13px; }
button { background: #2b6cb0; color: #fff; border: 0; border-radius: 6px; padding: 6px 10px; font-size: 13px; cursor: pointer; }
button.secondary { background: #33465e; }
.muted { color: #8aa0b8; font-size: 12px; }
.card { background: #18202b; border: 1px solid #263040; border-radius: 10px; padding: 14px; margin: 10px 0; }
.warn { background: #3a1d1d; border-color: #a33; }
.expired { color: #ff8a8a; font-weight: 600; }
</style>
</head>
<body>
<h1>ELM 网页版 → OpenAI API</h1>
{{range .Warnings}}<div class="card warn">⚠ {{.}}</div>{{end}}
<div class="card">
  <div class="muted">监听 <code>{{.BaseURL}}</code> 的上游 · 并发上限 <b>{{.MaxConc}}</b>（当前占用 {{.InUse}}）· 默认模型 <code>{{.Default}}</code></div>
  <div class="muted">OpenAI 客户端：<code>base_url = http://127.0.0.1:8787/v1</code>{{if .HasAPIKeys}}，<code>api_key</code> = 配置里的 key{{end}}</div>
</div>

<h2>已登录会话（{{len .Sessions}}）</h2>
{{if .Sessions}}
<table>
<tr><th>名称</th><th>账号</th><th>当前模型</th><th>ELM 聊天的</th><th>并发</th><th>预算</th><th>操作</th></tr>
{{range .Sessions}}
{{$sess := .}}
<tr>
  <td>{{.Name}}{{if .Expired}} <span class="expired">（cookie 已过期）</span>{{end}}<div class="muted">{{.ID}}</div></td>
  <td>{{.Email}}</td>
  <td><code>{{.Model}}</code></td>
  <td>{{if .Chats}}{{len .Chats}} 个聊天<div class="muted"><code>{{.ChatID}}</code></div><div class="muted">{{(index .Chats 0).Opening}}</div>{{else}}—{{end}}</td>
  <td>{{.Active}}</td>
  <td>已用 £{{printf "%.2f" .Quota.UsageGBP}} / 剩余 £{{printf "%.2f" .Quota.RemainingGBP}}</td>
  <td>
    <form method="post" action="/ui/switch{{$.KeyQuery}}">
      <input type="hidden" name="id" value="{{.ID}}">
      <select name="model">
        {{range $.Models}}<option value="{{.ID}}"{{if eq .ID $sess.Model}} selected{{end}}>{{.Name}} ({{.ID}})</option>{{end}}
      </select>
      <button type="submit">切换模型</button>
    </form>
    <form method="post" action="/ui/remove{{$.KeyQuery}}">
      <input type="hidden" name="id" value="{{.ID}}">
      <button type="submit" class="secondary">移除</button>
    </form>
  </td>
</tr>
{{end}}
</table>
{{else}}<p class="muted">还没有会话，请先粘贴 ELM SESSION cookie 登录。</p>{{end}}

<div class="card">
<h2 style="margin-top:0">用 SESSION cookie 登录</h2>
<form method="post" action="/ui/login{{.KeyQuery}}">
  <input name="name" placeholder="备注名（可选）" size="18">
  <input name="cookie" placeholder="SESSION=... 或只贴 cookie 值" size="60">
  <button type="submit">登录</button>
</form>
<div class="muted">cookie 获取：浏览器登录 ELM → DevTools → Application → Cookies → elm.edina.ac.uk → SESSION</div>
</div>

<h2>网页端全部模型（{{len .Models}}）</h2>
<table>
<tr><th>模型</th><th>ID</th><th>来源</th><th>上下文</th><th>免费</th></tr>
{{range .Models}}
<tr><td>{{.Name}}</td><td><code>{{.ID}}</code></td><td>{{.Source}}</td><td>{{.MaxTokens}}</td><td>{{if .IsEco}}是{{else}}否{{end}}</td></tr>
{{end}}
</table>
</body>
</html>`
