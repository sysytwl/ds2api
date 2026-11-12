package elmcore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExpiredSessionDetection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/open/is-logged-in", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"isLoggedIn":false}`))
	})
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := New(server.URL)
	ctx := context.Background()

	if _, err := client.IsLoggedIn(ctx, "SESSION=abc"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("IsLoggedIn error = %v, want ErrSessionExpired", err)
	}
	if _, err := client.Settings(ctx, "SESSION=abc"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Settings error = %v, want ErrSessionExpired", err)
	}
}

func TestLoggedInSessionOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/open/is-logged-in", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"isLoggedIn":true}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ok, err := New(server.URL).IsLoggedIn(context.Background(), "SESSION=abc")
	if err != nil || !ok {
		t.Fatalf("IsLoggedIn = %v, %v", ok, err)
	}
}
