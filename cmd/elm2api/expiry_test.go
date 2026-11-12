package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExpiredCookieOnLogin(t *testing.T) {
	f := &fakeUpstream{loggedIn: false, expired: true}
	pool := NewPool(f, "", 2, 1, 60)
	if _, err := pool.Add(context.Background(), "main", "SESSION=dead"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("Add error = %v", err)
	}
}

func TestChatExpiredSessionReturns401(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	f.mu.Lock()
	f.expired = true
	f.mu.Unlock()
	cfg := DefaultConfig()
	ts := httptest.NewServer(NewServer(cfg, pool, f).Handler())
	defer ts.Close()

	body := `{"model":"Qwen/Qwen3.5-397B-A17B-FP8","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var payload struct {
		Error map[string]any `json:"error"`
		Hint  string         `json:"hint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error["code"] != "elm_session_expired" {
		t.Fatalf("error payload = %#v", payload.Error)
	}
	if !strings.Contains(payload.Hint, "SESSION") {
		t.Fatalf("hint missing cookie instructions: %q", payload.Hint)
	}

	// the failed chat must mark the session as expired for the dashboard
	resp, err = http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var status struct {
		Warnings []string `json:"warnings"`
		Sessions []struct {
			Expired bool `json:"expired"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if len(status.Warnings) == 0 {
		t.Fatal("expected status warnings for expired session")
	}
	if len(status.Sessions) == 0 || !status.Sessions[0].Expired {
		t.Fatalf("session not marked expired: %#v", status.Sessions)
	}
}

func TestNoSessionReturns503WithHint(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool := NewPool(f, "", 2, 1, 60)
	cfg := DefaultConfig()
	ts := httptest.NewServer(NewServer(cfg, pool, f).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var payload struct {
		Hint string `json:"hint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload.Hint, "SESSION") {
		t.Fatalf("hint missing: %q", payload.Hint)
	}
}

func TestWatchdogMarksExpired(t *testing.T) {
	f := &fakeUpstream{loggedIn: true, models: testModels}
	pool, _ := newTestPool(t, f, 2)
	f.mu.Lock()
	f.expired = true
	f.loggedIn = false
	f.mu.Unlock()
	checkSessions(context.Background(), f, pool, "127.0.0.1:8787")
	views := pool.Views()
	if len(views) != 1 || !views[0].Expired {
		t.Fatalf("watchdog did not mark session expired: %#v", views)
	}
}
