package elmweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ds2api/internal/auth"
	"ds2api/internal/config"
	dsclient "ds2api/internal/deepseek/client"
	"ds2api/internal/elmcore"
)

const (
	streamTotalTimeout = 10 * time.Minute
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

// Client adapts the ELM web chat to the ds2api DeepSeek caller contract, so
// every ds2api surface can run on an ELM account.
type Client struct {
	cfg  ConfigReader
	core *elmcore.Client

	mu   sync.Mutex
	last map[string]string
}

// New builds an ELM adapter for ds2api.
func New(cfg ConfigReader) *Client {
	base := ""
	if cfg != nil {
		base = cfg.ElmBaseURL()
	}
	return &Client{cfg: cfg, core: elmcore.New(base), last: map[string]string{}}
}

// Login returns the ELM browser session cookie stored on the account token.
func (c *Client) Login(_ context.Context, acc config.Account) (string, error) {
	token := strings.TrimSpace(acc.Token)
	if token == "" {
		return "", fmt.Errorf("elm web: account %q has no ELM session cookie in token", strings.TrimSpace(acc.Identifier()))
	}
	return elmcore.NormalizeCookie(token), nil
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
	return c.core.ChatID(ctx, cookie)
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
	prompt, _ := payload["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("elm web: empty prompt")
	}
	sessionID, _ := payload["chat_session_id"].(string)
	model := c.modelFor(payload)
	pr, pw := io.Pipe()
	streamCtx, cancel := context.WithTimeout(ctx, streamTotalTimeout)
	go func() {
		defer cancel()
		defer func() { _ = pw.Close() }()
		err := c.stream(streamCtx, cookie, sessionID, prompt, model, pw)
		if err != nil {
			config.Logger.Warn("[elm] chat failed", "error", err)
		}
	}()
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Body:       pr,
	}, nil
}

func (c *Client) stream(ctx context.Context, cookie, sessionID, prompt, model string, w io.Writer) error {
	if model != "" {
		c.mu.Lock()
		last := c.last[cookie]
		c.mu.Unlock()
		if last != model {
			if err := c.core.SetModel(ctx, cookie, model); err != nil {
				return err
			}
			c.mu.Lock()
			c.last[cookie] = model
			c.mu.Unlock()
		}
	}
	return c.core.StreamChat(ctx, cookie, sessionID, prompt, "", func(kind elmcore.DeltaKind, text string) error {
		path := "response/content"
		if kind == elmcore.DeltaThinking {
			path = "response/thinking_content"
		}
		return writeDeepSeekSSE(w, path, text)
	})
}

func sessionCookie(a *auth.RequestAuth) (string, error) {
	if a == nil {
		return "", ErrNoSessionCookie
	}
	cookie := elmcore.NormalizeCookie(a.DeepSeekToken)
	if cookie == "" {
		return "", ErrNoSessionCookie
	}
	if strings.ContainsAny(cookie, "\r\n") {
		return "", errors.New("elm web: invalid session cookie")
	}
	return cookie, nil
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
