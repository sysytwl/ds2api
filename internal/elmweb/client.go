package elmweb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ds2api/internal/auth"
	"ds2api/internal/config"
	dsclient "ds2api/internal/deepseek/client"
)

const (
	// DefaultBaseURL is the ELM deployment used when the config leaves it empty.
	DefaultBaseURL = "https://elm.edina.ac.uk"

	connectTimeout     = 30 * time.Second
	streamIdleTimeout  = 120 * time.Second
	streamTotalTimeout = 10 * time.Minute
	maxJSONBody        = 4 << 20
)

// ErrNoSessionCookie means the routed account has no ELM browser session.
var ErrNoSessionCookie = errors.New("elm web: account token is not an ELM SESSION cookie")

// ErrFilesUnsupported means the ELM web upstream has no file API for ds2api.
var ErrFilesUnsupported = errors.New("elm web: file upload is not supported by the ELM web upstream")

// ConfigReader is the subset of the config store the ELM client needs.
type ConfigReader interface {
	ElmBaseURL() string
	ElmModel() string
}

// Client drives the ELM web chat as an upstream for ds2api. It implements the
// same caller contract as the DeepSeek web client, so every ds2api surface
// (OpenAI, Claude, Gemini, Responses) can run on an ELM account.
type Client struct {
	cfg  ConfigReader
	http *http.Client

	mu     sync.Mutex
	models map[string]string
}

// New builds an ELM web client.
func New(cfg ConfigReader) *Client {
	return &Client{
		cfg:    cfg,
		http:   &http.Client{Timeout: 60 * time.Second},
		models: map[string]string{},
	}
}

// Login returns the ELM browser session cookie stored on the account token.
func (c *Client) Login(_ context.Context, acc config.Account) (string, error) {
	token := strings.TrimSpace(acc.Token)
	if token == "" {
		return "", fmt.Errorf("elm web: account %q has no ELM session cookie in token", strings.TrimSpace(acc.Identifier()))
	}
	return normalizeCookie(token), nil
}

func normalizeCookie(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if strings.Contains(token, "=") {
		return token
	}
	return "SESSION=" + token
}

func (c *Client) baseURL() string {
	if c != nil && c.cfg != nil {
		if v := strings.TrimSpace(c.cfg.ElmBaseURL()); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return DefaultBaseURL
}

func (c *Client) wsURL() string {
	u, err := url.Parse(c.baseURL())
	if err != nil {
		return "wss://elm.edina.ac.uk/ws"
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/ws"
	u.RawQuery = ""
	return u.String()
}

func (c *Client) modelFor(payload map[string]any) string {
	if payload != nil {
		if v, ok := payload["elm_model"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if c != nil && c.cfg != nil {
		return strings.TrimSpace(c.cfg.ElmModel())
	}
	return ""
}

// CreateSession asks ELM for a fresh chat id.
func (c *Client) CreateSession(ctx context.Context, a *auth.RequestAuth, _ int) (string, error) {
	cookie, err := sessionCookie(a)
	if err != nil {
		return "", err
	}
	return c.chatID(ctx, cookie)
}

// GetPow is a no-op upstream: ELM does not require a proof of work.
func (c *Client) GetPow(_ context.Context, _ *auth.RequestAuth, _ int) (string, error) {
	return "", nil
}

// UploadFile is not supported by the ELM web upstream.
func (c *Client) UploadFile(_ context.Context, _ *auth.RequestAuth, _ dsclient.UploadFileRequest, _ int) (*dsclient.UploadFileResult, error) {
	return nil, ErrFilesUnsupported
}

// DeleteSessionForToken is a best-effort no-op; ELM chats expire with the session.
func (c *Client) DeleteSessionForToken(_ context.Context, _ string, sessionID string) (*dsclient.DeleteSessionResult, error) {
	return &dsclient.DeleteSessionResult{SessionID: sessionID, Success: true}, nil
}

// DeleteAllSessionsForToken is a best-effort no-op.
func (c *Client) DeleteAllSessionsForToken(_ context.Context, _ string) error {
	return nil
}

// GetSessionCountForToken reports no DeepSeek-style session list for ELM.
func (c *Client) GetSessionCountForToken(_ context.Context, _ string) (*dsclient.SessionStats, error) {
	return &dsclient.SessionStats{Success: true}, nil
}

// CallCompletion sends one prompt through the ELM web chat and returns a
// DeepSeek-shaped SSE stream so the shared ds2api pipeline can consume it.
func (c *Client) CallCompletion(ctx context.Context, a *auth.RequestAuth, payload map[string]any, _ string, _ int) (*http.Response, error) {
	cookie, err := sessionCookie(a)
	if err != nil {
		return nil, err
	}
	prompt := asString(payload["prompt"])
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("elm web: empty prompt")
	}
	sessionID := asString(payload["chat_session_id"])
	model := c.modelFor(payload)
	pr, pw := io.Pipe()
	streamCtx, cancel := context.WithTimeout(ctx, streamTotalTimeout)
	go func() {
		defer cancel()
		defer func() { _ = pw.Close() }()
		c.streamChat(streamCtx, cookie, sessionID, prompt, model, pw)
	}()
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Body:       pr,
	}, nil
}

func sessionCookie(a *auth.RequestAuth) (string, error) {
	if a == nil {
		return "", ErrNoSessionCookie
	}
	cookie := normalizeCookie(a.DeepSeekToken)
	if cookie == "" {
		return "", ErrNoSessionCookie
	}
	if strings.ContainsAny(cookie, "\r\n") {
		return "", errors.New("elm web: invalid session cookie")
	}
	return cookie, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func cookieKey(cookie string) string {
	sum := sha1.Sum([]byte(cookie))
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (c *Client) doJSON(ctx context.Context, method, path, cookie string, headers map[string]string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", c.baseURL())
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) ds2api-elm/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("elm web: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody))
	if err != nil {
		return fmt.Errorf("elm web: %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("elm web: %s %s: status %d: %s", method, path, resp.StatusCode, truncate(strings.TrimSpace(string(data)), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("elm web: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

type csrfResponse struct {
	HeaderName string `json:"headerName"`
	Token      string `json:"token"`
}

func (c *Client) csrf(ctx context.Context, cookie string) (csrfResponse, error) {
	var out csrfResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/csrf", cookie, nil, nil, &out); err != nil {
		return csrfResponse{}, err
	}
	return out, nil
}

type chatIDResponse struct {
	ChatID string `json:"chatId"`
}

func (c *Client) chatID(ctx context.Context, cookie string) (string, error) {
	var out chatIDResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/chat-id", cookie, nil, nil, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ChatID) == "" {
		return "", errors.New("elm web: chat-id response was empty")
	}
	return out.ChatID, nil
}

func (c *Client) settings(ctx context.Context, cookie string) (map[string]any, error) {
	out := map[string]any{}
	if err := c.doJSON(ctx, http.MethodGet, "/api/settings", cookie, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) putSettings(ctx context.Context, cookie string, csrf csrfResponse, settings map[string]any) error {
	headers := map[string]string{}
	if strings.TrimSpace(csrf.HeaderName) != "" && strings.TrimSpace(csrf.Token) != "" {
		headers[csrf.HeaderName] = csrf.Token
	}
	return c.doJSON(ctx, http.MethodPut, "/api/settings", cookie, headers, settings, nil)
}

// ensureModel switches the ELM browser session to the configured model. ELM
// keeps the model in the session, not in the chat request, so it is written once
// per cookie and model.
func (c *Client) ensureModel(ctx context.Context, cookie, model string) error {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	key := cookieKey(cookie) + "|" + model
	c.mu.Lock()
	_, cached := c.models[key]
	c.mu.Unlock()
	if cached {
		return nil
	}
	settings, err := c.settings(ctx, cookie)
	if err != nil {
		return err
	}
	current, _ := settings["model"].(string)
	if current != model {
		settings["model"] = model
		csrf, err := c.csrf(ctx, cookie)
		if err != nil {
			return err
		}
		if err := c.putSettings(ctx, cookie, csrf, settings); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.models[key] = model
	c.mu.Unlock()
	return nil
}

func (c *Client) streamChat(ctx context.Context, cookie, sessionID, prompt, model string, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	if err := c.ensureModel(ctx, cookie, model); err != nil {
		config.Logger.Warn("[elm] set model failed", "error", err)
	}
	if strings.TrimSpace(sessionID) == "" {
		id, err := c.chatID(ctx, cookie)
		if err != nil {
			config.Logger.Warn("[elm] create chat failed", "error", err)
			return
		}
		sessionID = id
	}
	csrf, err := c.csrf(ctx, cookie)
	if err != nil {
		config.Logger.Warn("[elm] csrf failed", "error", err)
		return
	}
	ws, err := dialWebSocket(ctx, c.wsURL(), map[string]string{"Cookie": cookie, "Origin": c.baseURL()})
	if err != nil {
		config.Logger.Warn("[elm] websocket dial failed", "error", err)
		return
	}
	defer func() { _ = ws.Close() }()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-done:
		}
	}()

	connectHeaders := []stompHeader{
		{Key: "accept-version", Value: "1.2,1.1,1.0"},
		{Key: "heart-beat", Value: "0,0"},
		{Key: "host", Value: "/"},
	}
	if strings.TrimSpace(csrf.HeaderName) != "" && strings.TrimSpace(csrf.Token) != "" {
		connectHeaders = append(connectHeaders, stompHeader{Key: csrf.HeaderName, Value: csrf.Token})
	}
	if err := ws.WriteMessage(encodeStompFrame("CONNECT", connectHeaders, "")); err != nil {
		config.Logger.Warn("[elm] stomp connect failed", "error", err)
		return
	}
	_ = ws.SetReadDeadline(time.Now().Add(connectTimeout))
	connected := false
	for !connected {
		msg, err := ws.ReadMessage()
		if err != nil {
			config.Logger.Warn("[elm] stomp handshake failed", "error", err)
			return
		}
		frame, ok, err := parseStompFrame(msg)
		if err != nil {
			return
		}
		if !ok {
			continue
		}
		switch frame.Command {
		case "CONNECTED":
			connected = true
		case "ERROR":
			config.Logger.Warn("[elm] stomp connect rejected", "message", frame.Header("message"))
			return
		}
	}

	if err := ws.WriteMessage(encodeStompFrame("SUBSCRIBE", []stompHeader{
		{Key: "id", Value: "sub-0"},
		{Key: "destination", Value: "/topic/query-" + sessionID},
	}, "")); err != nil {
		return
	}
	queryBody, err := json.Marshal(map[string]any{"query": prompt, "promptId": nil})
	if err != nil {
		return
	}
	if err := ws.WriteMessage(encodeStompFrame("SEND", []stompHeader{
		{Key: "destination", Value: "/app/queue/query-" + sessionID},
		{Key: "content-type", Value: "application/json"},
		{Key: "content-length", Value: strconv.Itoa(len(queryBody))},
	}, string(queryBody))); err != nil {
		return
	}

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(streamIdleTimeout))
		msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		frame, ok, err := parseStompFrame(msg)
		if err != nil || !ok {
			continue
		}
		switch frame.Command {
		case "ERROR":
			config.Logger.Warn("[elm] stomp stream error", "message", frame.Header("message"))
			return
		case "MESSAGE":
		default:
			continue
		}
		ev, err := parseChatEvent(frame.Body)
		if err != nil {
			continue
		}
		switch {
		case isFinishedEvent(ev):
			if err := writeDeepSeekSSE(w, "response/status", "FINISHED"); err != nil {
				return
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		case isErrorEvent(ev):
			return
		case isUserEvent(ev):
			continue
		case isThinkingEvent(ev):
			if ev.Reply == "" {
				continue
			}
			if err := writeDeepSeekSSE(w, "response/thinking_content", ev.Reply); err != nil {
				return
			}
		default:
			if ev.Reply == "" {
				continue
			}
			if err := writeDeepSeekSSE(w, "response/content", ev.Reply); err != nil {
				return
			}
		}
	}
}

func writeDeepSeekSSE(w io.Writer, path, value string) error {
	raw, err := json.Marshal(map[string]any{"p": path, "v": value})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	return nil
}
