package main

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"ds2api/internal/elmcore"
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
	mux.HandleFunc("GET /v1/models", s.requireKey(s.handleModels))
	mux.HandleFunc("GET /v1/models/{id}", s.requireKey(s.handleModelByID))
	mux.HandleFunc("POST /v1/chat/completions", s.requireKey(s.handleChat))
	mux.HandleFunc("GET /api/status", s.loopback(s.requireKey(s.handleStatus)))
	mux.HandleFunc("GET /api/sessions", s.loopback(s.requireKey(s.handleSessions)))
	mux.HandleFunc("POST /api/sessions", s.loopback(s.requireKey(s.handleAddSession)))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.loopback(s.requireKey(s.handleRemoveSession)))
	mux.HandleFunc("PUT /api/sessions/{id}/model", s.loopback(s.requireKey(s.handleSwitchModel)))
	mux.HandleFunc("POST /api/models/refresh", s.loopback(s.requireKey(s.handleRefreshModels)))
	mux.HandleFunc("GET /", s.loopback(s.requireKey(s.handleIndex)))
	mux.HandleFunc("POST /ui/login", s.loopback(s.requireKey(s.handleUILogin)))
	mux.HandleFunc("POST /ui/switch", s.loopback(s.requireKey(s.handleUISwitch)))
	mux.HandleFunc("POST /ui/remove", s.loopback(s.requireKey(s.handleUIRemove)))
	return logRequests(mux)
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

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
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
		writeJSON(w, http.StatusBadGateway, errorPayload(err.Error(), "upstream_error"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleModelByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	models, err := s.pool.ModelsForOpenAI(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorPayload(err.Error(), "upstream_error"))
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
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "model_not_found"))
		return
	}
	prompt, err := buildPrompt(req.Messages)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "invalid_request"))
		return
	}
	displayModel := strings.TrimSpace(req.Model)
	if displayModel == "" {
		displayModel = resolved
	}
	id := newCompletionID()
	created := timeNowUnix()
	if req.Stream {
		s.streamCompletion(w, r, id, displayModel, resolved, prompt, created)
		return
	}
	var content, thinking strings.Builder
	err = s.pool.Chat(r.Context(), resolved, prompt, func(kind elmcore.DeltaKind, text string) error {
		if kind == elmcore.DeltaThinking {
			thinking.WriteString(text)
		} else {
			content.WriteString(text)
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorPayload(err.Error(), "upstream_error"))
		return
	}
	writeJSON(w, http.StatusOK, completionPayload(id, displayModel, content.String(), thinking.String()))
}

func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, id, displayModel, resolved, prompt string, created int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorPayload("streaming unsupported", "internal_error"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(payload map[string]any) error {
		if err := ssePayload(w, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	_ = emit(chunkPayload(id, displayModel, created, map[string]any{"role": "assistant", "content": ""}))

	err := s.pool.Chat(r.Context(), resolved, prompt, func(kind elmcore.DeltaKind, text string) error {
		delta := map[string]any{"content": text}
		if kind == elmcore.DeltaThinking {
			delta = map[string]any{"reasoning_content": text}
		}
		return emit(chunkPayload(id, displayModel, created, delta))
	})
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("stream chat failed: %v", err)
			_ = emit(errorPayload(err.Error(), "upstream_error"))
		}
		return
	}
	_ = emit(finishChunkPayload(id, displayModel, created, "stop"))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	models, _ := s.pool.Models(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"default_model": s.cfg.DefaultModel,
		"listen":        s.cfg.Listen,
		"concurrency": map[string]any{
			"max":    cap(s.pool.sem),
			"in_use": len(s.pool.sem),
		},
		"sessions": s.pool.Views(),
		"models":   len(models),
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
		status := http.StatusBadRequest
		if errors.Is(err, elmcore.ErrNotLoggedIn) {
			status = http.StatusUnauthorized
		}
		writeJSON(w, status, errorPayload(err.Error(), "login_failed"))
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
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "switch_failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model})
}

func (s *Server) handleRefreshModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.pool.RefreshModels(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorPayload(err.Error(), "upstream_error"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": len(models)})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
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
</style>
</head>
<body>
<h1>ELM 网页版 → OpenAI API</h1>
<div class="card">
  <div class="muted">监听 <code>{{.BaseURL}}</code> 的上游 · 并发上限 <b>{{.MaxConc}}</b>（当前占用 {{.InUse}}）· 默认模型 <code>{{.Default}}</code></div>
  <div class="muted">OpenAI 客户端：<code>base_url = http://127.0.0.1:8787/v1</code>{{if .HasAPIKeys}}，<code>api_key</code> = 配置里的 key{{end}}</div>
</div>

<h2>已登录会话（{{len .Sessions}}）</h2>
{{if .Sessions}}
<table>
<tr><th>名称</th><th>账号</th><th>当前模型</th><th>并发</th><th>预算</th><th>操作</th></tr>
{{range .Sessions}}
{{$sess := .}}
<tr>
  <td>{{.Name}}<div class="muted">{{.ID}}</div></td>
  <td>{{.Email}}</td>
  <td><code>{{.Model}}</code></td>
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
