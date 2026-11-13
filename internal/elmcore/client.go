package elmcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// debugEnabled turns on low-level frame logging (ELM_DEBUG=1).
var debugEnabled atomicBool

// atomicBool is a tiny dependency-free switch.
type atomicBool struct{ v int32 }

func (b *atomicBool) Store(v bool) {
	if v {
		atomic.StoreInt32(&b.v, 1)
		return
	}
	atomic.StoreInt32(&b.v, 0)
}

func (b *atomicBool) Load() bool { return atomic.LoadInt32(&b.v) == 1 }

// SetDebug enables or disables verbose protocol logging.
func SetDebug(v bool) { debugEnabled.Store(v) }

// DebugEnabled reports whether verbose protocol logging is on.
func DebugEnabled() bool { return debugEnabled.Load() }

func debugf(format string, args ...any) {
	if debugEnabled.Load() {
		log.Printf("elmcore: "+format, args...)
	}
}

const (
	// DefaultBaseURL is the ELM deployment.
	DefaultBaseURL = "https://elm.edina.ac.uk"

	connectTimeout    = 30 * time.Second
	streamIdleTimeout = 120 * time.Second
	maxJSONBody       = 4 << 20
	requestTimeout    = 60 * time.Second

	// The chat websocket gateway can drop a new socket while the previous chat
	// connection is still being reaped server side; retry opening the stream.
	chatStartAttempts = 3
	chatStartBackoff  = 400 * time.Millisecond
)

var (
	// ErrSessionRequired means no ELM SESSION cookie was supplied.
	ErrSessionRequired = errors.New("elm: SESSION cookie is required")
	// ErrSessionExpired means the ELM SESSION cookie is no longer logged in
	// (expired, signed out, or rejected by the server). Callers should ask the
	// operator for a fresh cookie instead of retrying.
	ErrSessionExpired = errors.New("elm: SESSION cookie expired or logged out")
)

// DeltaKind tells a streaming consumer what kind of text arrived.
type DeltaKind string

const (
	// DeltaContent is answer text.
	DeltaContent DeltaKind = "content"
	// DeltaThinking is reasoning/thinking text.
	DeltaThinking DeltaKind = "thinking"
)

// User is the ELM account behind a session cookie.
type User struct {
	Email       string `json:"email"`
	ResolvedID  string `json:"resolvedId"`
	IsStaff     bool   `json:"isStaff"`
	AgreedTerms bool   `json:"agreedTerms"`
}

// Quota is the personal AI budget summary.
type Quota struct {
	BaseLimitGBP      float64 `json:"baseLimitGbp"`
	UsageGBP          float64 `json:"usageGbp"`
	EffectiveLimitGBP float64 `json:"effectiveLimitGbp"`
	RemainingGBP      float64 `json:"remainingGbp"`
	PercentageUsed    float64 `json:"percentageUsed"`
	PeriodType        string  `json:"periodType"`
	ResetDate         string  `json:"resetDate"`
	HardCap           bool    `json:"hardCap"`
	Exhausted         bool    `json:"exhausted"`
}

// Model is one model shown by the ELM web app.
type Model struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Source           string `json:"source"`
	MaxTokens        int    `json:"maxTokens"`
	IsEco            bool   `json:"isEco"`
	Active           bool   `json:"active"`
	ReasoningEfforts any    `json:"reasoningEfforts"`
	ReasoningDefault any    `json:"reasoningDefault"`
	Capabilities     any    `json:"capabilities"`
	Traits           any    `json:"traits"`
}

// Prompt is one saved prompt template from GET /api/prompts. ELM prepends the
// selected template's text to every chat message, so the web app always sends
// its id as "promptId" in the STOMP query body. A blank template ("Empty
// Prompt") is how the web chat stays free of the institution-wide default
// prompt, which otherwise competes with the tool protocol this service adds.
type Prompt struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Prompt   string `json:"prompt"`
	IsUser   bool   `json:"isUser"`
}

// ChatRequest is one turn sent to the ELM chat.
type ChatRequest struct {
	ChatID string
	Prompt string
	Model  string
	// PromptID selects the ELM prompt template; 0 sends "promptId": null, which
	// makes ELM fall back to its default institution prompt.
	PromptID int64
}

// ErrEmptyPromptMissing means the account has no prompt template with empty
// text, so the service cannot suppress the default prompt.
var ErrEmptyPromptMissing = errors.New("elm: no empty prompt template is available")

// Client talks to the ELM web backend with a browser session cookie.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds an ELM core client. An empty baseURL uses DefaultBaseURL.
func New(baseURL string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{baseURL: baseURL, http: &http.Client{
		Timeout: requestTimeout,
		// ELM answers an expired session with a redirect to the login page;
		// surface that instead of silently following it into HTML.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// BaseURL is the configured ELM origin.
func (c *Client) BaseURL() string { return c.baseURL }

func (c *Client) wsURL() string {
	u, err := url.Parse(c.baseURL)
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

// NormalizeCookie accepts "SESSION=..." or a bare cookie value.
func NormalizeCookie(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "=") {
		return raw
	}
	return "SESSION=" + raw
}

func validateCookie(cookie string) (string, error) {
	cookie = NormalizeCookie(cookie)
	if cookie == "" {
		return "", ErrSessionRequired
	}
	if strings.ContainsAny(cookie, "\r\n") {
		return "", errors.New("elm: invalid SESSION cookie")
	}
	return cookie, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type csrfResponse struct {
	HeaderName string `json:"headerName"`
	Token      string `json:"token"`
}

type chatIDResponse struct {
	ChatID string `json:"chatId"`
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
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", c.baseURL)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) elm2api/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("elm: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody))
	if err != nil {
		return fmt.Errorf("elm: %s %s: read body: %w", method, path, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s %s returned %d", ErrSessionExpired, method, path, resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location == "" {
			location = truncate(strings.TrimSpace(string(data)), 160)
		}
		return fmt.Errorf("%w: %s %s redirected to %s", ErrSessionExpired, method, path, location)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("elm: %s %s: status %d: %s", method, path, resp.StatusCode, truncate(strings.TrimSpace(string(data)), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("elm: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// IsLoggedIn checks whether the cookie is a live ELM session.
func (c *Client) IsLoggedIn(ctx context.Context, cookie string) (bool, error) {
	cookie, err := validateCookie(cookie)
	if err != nil {
		return false, err
	}
	var out struct {
		IsLoggedIn bool `json:"isLoggedIn"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/open/is-logged-in", cookie, nil, nil, &out); err != nil {
		return false, err
	}
	if !out.IsLoggedIn {
		return false, fmt.Errorf("%w: /api/open/is-logged-in returned false", ErrSessionExpired)
	}
	return true, nil
}

// User returns the account bound to the session.
func (c *Client) User(ctx context.Context, cookie string) (User, error) {
	var out User
	cookie, err := validateCookie(cookie)
	if err != nil {
		return out, err
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/user", cookie, nil, nil, &out); err != nil {
		return out, err
	}
	return out, nil
}

// Quota returns the personal AI budget summary.
func (c *Client) Quota(ctx context.Context, cookie string) (Quota, error) {
	var out Quota
	cookie, err := validateCookie(cookie)
	if err != nil {
		return out, err
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/quota/status/summary", cookie, nil, nil, &out); err != nil {
		return out, err
	}
	return out, nil
}

// Models lists every model offered by the ELM web app.
func (c *Client) Models(ctx context.Context, cookie string) ([]Model, error) {
	cookie, err := validateCookie(cookie)
	if err != nil {
		return nil, err
	}
	var out []Model
	if err := c.doJSON(ctx, http.MethodGet, "/api/settings/models", cookie, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Prompts lists the prompt templates the web app offers.
func (c *Client) Prompts(ctx context.Context, cookie string) ([]Prompt, error) {
	cookie, err := validateCookie(cookie)
	if err != nil {
		return nil, err
	}
	var out []Prompt
	if err := c.doJSON(ctx, http.MethodGet, "/api/prompts", cookie, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EmptyPromptID returns the id of the blank prompt template ("Empty Prompt").
// Sending that id keeps the default institution prompt out of the chat.
func (c *Client) EmptyPromptID(ctx context.Context, cookie string) (int64, error) {
	prompts, err := c.Prompts(ctx, cookie)
	if err != nil {
		return 0, err
	}
	id := PickEmptyPrompt(prompts)
	if id == 0 {
		return 0, ErrEmptyPromptMissing
	}
	return id, nil
}

// PickEmptyPrompt prefers a blank template whose name mentions "empty", then
// any blank template.
func PickEmptyPrompt(prompts []Prompt) int64 {
	var fallback int64
	for _, prompt := range prompts {
		if strings.TrimSpace(prompt.Prompt) != "" {
			continue
		}
		if strings.Contains(strings.ToLower(prompt.Name), "empty") {
			return prompt.ID
		}
		if fallback == 0 {
			fallback = prompt.ID
		}
	}
	return fallback
}

// SelectPrompt makes a prompt template the session's default. The web app does
// this when the user picks a template; the per-query promptId is still sent
// explicitly with every chat message.
func (c *Client) SelectPrompt(ctx context.Context, cookie string, id int64) error {
	cookie, err := validateCookie(cookie)
	if err != nil {
		return err
	}
	if id <= 0 {
		return nil
	}
	headerName, token, err := c.CSRF(ctx, cookie)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if headerName != "" && token != "" {
		headers[headerName] = token
	}
	return c.doJSON(ctx, http.MethodPut, "/api/settings/prompt", cookie, headers, id, nil)
}

// CSRF fetches the token required for mutating requests and STOMP CONNECT.
func (c *Client) CSRF(ctx context.Context, cookie string) (headerName, token string, err error) {
	cookie, err = validateCookie(cookie)
	if err != nil {
		return "", "", err
	}
	var out csrfResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/csrf", cookie, nil, nil, &out); err != nil {
		return "", "", err
	}
	return out.HeaderName, out.Token, nil
}

// ChatID creates a fresh ELM chat id.
func (c *Client) ChatID(ctx context.Context, cookie string) (string, error) {
	cookie, err := validateCookie(cookie)
	if err != nil {
		return "", err
	}
	var out chatIDResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/chat-id", cookie, nil, nil, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ChatID) == "" {
		return "", errors.New("elm: chat-id response was empty")
	}
	return out.ChatID, nil
}

// NewChat asks ELM for a fresh chat id. Each API request gets its own chat so
// requests never mix histories and never disturb the chat open in a browser.
func (c *Client) NewChat(ctx context.Context, cookie string) (string, error) {
	cookie, err := validateCookie(cookie)
	if err != nil {
		return "", err
	}
	var raw map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/new-chat", cookie, nil, nil, &raw); err != nil {
		return "", err
	}
	if id := chatIDFromMap(raw); id != "" {
		return id, nil
	}
	if nested, ok := raw["chat"].(map[string]any); ok {
		if id := chatIDFromMap(nested); id != "" {
			return id, nil
		}
	}
	// Fall back to the session's current chat when the endpoint returns no id.
	return c.ChatID(ctx, cookie)
}

func chatIDFromMap(raw map[string]any) string {
	for _, key := range []string{"chatId", "chatID", "id"} {
		if v, ok := raw[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Settings reads the session settings map.
func (c *Client) Settings(ctx context.Context, cookie string) (map[string]any, error) {
	cookie, err := validateCookie(cookie)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := c.doJSON(ctx, http.MethodGet, "/api/settings", cookie, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CurrentModel reports the model selected in the ELM session.
func (c *Client) CurrentModel(ctx context.Context, cookie string) (string, error) {
	settings, err := c.Settings(ctx, cookie)
	if err != nil {
		return "", err
	}
	model, _ := settings["model"].(string)
	return strings.TrimSpace(model), nil
}

// SetModel switches the ELM session model (a session-wide setting).
func (c *Client) SetModel(ctx context.Context, cookie, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	settings, err := c.Settings(ctx, cookie)
	if err != nil {
		return err
	}
	if current, _ := settings["model"].(string); current == model {
		return nil
	}
	settings["model"] = model
	headerName, token, err := c.CSRF(ctx, cookie)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if headerName != "" && token != "" {
		headers[headerName] = token
	}
	if err := c.doJSON(ctx, http.MethodPut, "/api/settings", cookie, headers, settings, nil); err != nil {
		return err
	}
	return nil
}

// errChatRetry is returned when the broker asks this client to resend the query
// on a fresh connection (a drain notification). The ELM web app does the same:
// it forces a reconnect and republishes the stored query body once.
var errChatRetry = errors.New("elm: broker asked to retry the query")

// drainNotice is the payload the broker publishes on
// /user/exchange/amq.direct/drain. RECONNECT (chatId null) means "reconnect
// when idle"; RETRY_QUERY names a chat whose query must be resent.
type drainNotice struct {
	Type   string  `json:"type"`
	ChatID *string `json:"chatId"`
}

func isDrainNotice(body string) bool {
	body = strings.TrimSpace(body)
	return strings.HasPrefix(body, "{") && strings.Contains(body, `"type"`)
}

// wantsRetry reports whether a drain notice asks for this chat to be resent.
func wantsRetry(body, chatID string) bool {
	var notice drainNotice
	if err := json.Unmarshal([]byte(body), &notice); err != nil {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(notice.Type)) {
	case "RECONNECT":
		return notice.ChatID == nil
	case "RETRY_QUERY":
		return notice.ChatID != nil && strings.TrimSpace(*notice.ChatID) == chatID
	default:
		return false
	}
}

// StreamChat sends one prompt to the ELM web chat and calls onDelta for every
// streamed chunk until the chat finishes. A non-empty model switches the
// session model first, and a non-zero PromptID selects the prompt template
// (0 sends "promptId": null, which lets ELM fall back to its default prompt).
// onDelta returning an error aborts the stream.
//
// The ELM websocket gateway occasionally drops a brand-new chat socket when the
// previous chat connection has not been reaped server side yet. Because nothing
// has been sent to the model at that point, opening the chat stream is retried
// a few times with a short backoff.
func (c *Client) StreamChat(ctx context.Context, cookie string, req ChatRequest, onDelta func(DeltaKind, string) error) error {
	cookie, err := validateCookie(cookie)
	if err != nil {
		return err
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return errors.New("elm: empty prompt")
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		if err := c.SetModel(ctx, cookie, model); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.ChatID) == "" {
		req.ChatID, err = c.NewChat(ctx, cookie)
		if err != nil {
			return err
		}
	}
	headerName, csrfToken, err := c.CSRF(ctx, cookie)
	if err != nil {
		return err
	}

	// A drain notification asks for one clean retry on a fresh connection. It is
	// only honoured while no text reached the client, so a retry can never
	// duplicate a streamed answer.
	for round := 0; round < 2; round++ {
		ws, err := c.openChatStreamWithRetry(ctx, cookie, req, headerName, csrfToken)
		if err != nil {
			return err
		}
		emitted := false
		readErr := c.readChatStreamWithContext(ctx, ws, req.ChatID, &emitted, func(kind DeltaKind, text string) error {
			emitted = true
			return onDelta(kind, text)
		})
		_ = ws.Close()
		if readErr == nil {
			return nil
		}
		if errors.Is(readErr, errChatRetry) && !emitted {
			debugf("chat %s: broker asked for a retry, reconnecting", req.ChatID)
			continue
		}
		return readErr
	}
	return errChatRetry
}

// openChatStreamWithRetry opens the chat socket, retrying the initial
// connection while the previous chat socket is still being reaped server side.
func (c *Client) openChatStreamWithRetry(ctx context.Context, cookie string, req ChatRequest, headerName, csrfToken string) (*wsConn, error) {
	var lastErr error
	for attempt := 0; attempt < chatStartAttempts; attempt++ {
		ws, err := c.openChatStream(ctx, cookie, req, headerName, csrfToken)
		if err == nil {
			return ws, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(chatStartBackoff * time.Duration(attempt+1)):
		}
	}
	return nil, lastErr
}

// readChatStreamWithContext closes the socket when the request context ends.
func (c *Client) readChatStreamWithContext(ctx context.Context, ws *wsConn, chatID string, emitted *bool, onDelta func(DeltaKind, string) error) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-done:
		}
	}()
	return c.readChatStream(ctx, ws, chatID, emitted, onDelta)
}

// openChatStream dials the chat websocket, performs the STOMP handshake,
// subscribes to the chat topic and submits the prompt.
func (c *Client) openChatStream(ctx context.Context, cookie string, req ChatRequest, headerName, csrfToken string) (*wsConn, error) {
	ws, err := dialWebSocket(ctx, c.wsURL(), map[string]string{"Cookie": cookie, "Origin": c.baseURL})
	if err != nil {
		return nil, err
	}
	debugf("open chat=%s prompt=%d bytes promptId=%d", req.ChatID, len(req.Prompt), req.PromptID)
	ok := false
	defer func() {
		if !ok {
			_ = ws.Close()
		}
	}()

	connectHeaders := []stompHeader{
		{Key: "accept-version", Value: "1.2,1.1,1.0"},
		{Key: "heart-beat", Value: "0,0"},
		{Key: "host", Value: "/"},
	}
	if headerName != "" && csrfToken != "" {
		connectHeaders = append(connectHeaders, stompHeader{Key: headerName, Value: csrfToken})
	}
	if err := ws.WriteMessage(encodeStompFrame("CONNECT", connectHeaders, "")); err != nil {
		return nil, fmt.Errorf("elm: stomp connect: %w", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(connectTimeout))
	for {
		msg, err := ws.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("elm: stomp handshake: %w", err)
		}
		frame, okFrame, err := parseStompFrame(msg)
		if err != nil {
			return nil, err
		}
		if !okFrame {
			continue
		}
		switch frame.Command {
		case "CONNECTED":
		case "ERROR":
			return nil, fmt.Errorf("elm: stomp connect rejected: %s", frame.Header("message"))
		default:
			continue
		}
		break
	}

	if err := ws.WriteMessage(encodeStompFrame("SUBSCRIBE", []stompHeader{
		{Key: "id", Value: "sub-0"},
		{Key: "destination", Value: "/topic/query-" + req.ChatID},
	}, "")); err != nil {
		return nil, fmt.Errorf("elm: stomp subscribe: %w", err)
	}
	// The broker publishes drain notifications here when a backend node is
	// shutting down or asks for the query to be retried (the web app uses the
	// same destination). The reply is delivered on the chat subscription.
	if err := ws.WriteMessage(encodeStompFrame("SUBSCRIBE", []stompHeader{
		{Key: "id", Value: "sub-drain"},
		{Key: "destination", Value: "/user/exchange/amq.direct/drain"},
	}, "")); err != nil {
		return nil, fmt.Errorf("elm: stomp subscribe drain: %w", err)
	}
	payload := map[string]any{"query": req.Prompt, "promptId": nil}
	if req.PromptID > 0 {
		payload["promptId"] = req.PromptID
	}
	queryBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if err := ws.WriteMessage(encodeStompFrame("SEND", []stompHeader{
		{Key: "destination", Value: "/app/queue/query-" + req.ChatID},
		{Key: "content-type", Value: "application/json"},
		{Key: "content-length", Value: strconv.Itoa(len(queryBody))},
	}, string(queryBody))); err != nil {
		return nil, fmt.Errorf("elm: stomp send: %w", err)
	}
	ok = true
	return ws, nil
}

// readChatStream consumes chat events until the chat finishes.
func (c *Client) readChatStream(ctx context.Context, ws *wsConn, chatID string, emitted *bool, onDelta func(DeltaKind, string) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_ = ws.SetReadDeadline(time.Now().Add(streamIdleTimeout))
		msg, err := ws.ReadMessage()
		if err != nil {
			if errors.Is(err, ErrWebSocketClosed) {
				return nil
			}
			return fmt.Errorf("elm: chat stream: %w", err)
		}
		frame, ok, err := parseStompFrame(msg)
		if err != nil || !ok {
			continue
		}
		switch frame.Command {
		case "ERROR":
			return fmt.Errorf("elm: chat stream error: %s", frame.Header("message"))
		case "MESSAGE":
		default:
			continue
		}
		if sub := frame.Header("subscription"); sub == "sub-drain" || (sub == "" && isDrainNotice(frame.Body)) {
			if wantsRetry(frame.Body, chatID) && (emitted == nil || !*emitted) {
				return errChatRetry
			}
			// Once text reached the client a retry cannot be replayed without
			// duplicating it, so the notice is ignored and the stream goes on.
			continue
		}
		event, err := parseChatEvent(frame.Body)
		if err != nil {
			debugf("unparsed frame: %s", string(frame.Body[:min(len(frame.Body), 200)]))
			continue
		}
		debugf("frame type=%q reply=%d finished=%v", event.ReplyType, len(event.Reply), isFinishedEvent(event))
		switch {
		case isFinishedEvent(event):
			return nil
		case isErrorEvent(event):
			return fmt.Errorf("elm: chat ended with %s", event.ReplyType)
		case isUserEvent(event):
			continue
		case event.Reply == "":
			continue
		case isThinkingEvent(event):
			if err := onDelta(DeltaThinking, event.Reply); err != nil {
				return err
			}
		default:
			if err := onDelta(DeltaContent, event.Reply); err != nil {
				return err
			}
		}
	}
}
