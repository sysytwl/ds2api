package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"elm2api/internal/elmcore"
)

func main() {
	configPath := flag.String("config", os.Getenv("ELM_CONFIG"), "path to config JSON (env ELM_CONFIG)")
	listen := flag.String("listen", "", "listen address, overrides config listen (default 127.0.0.1:8787)")
	flag.Parse()

	if v := strings.TrimSpace(os.Getenv("ELM_DEBUG")); v != "" && v != "0" && v != "false" {
		elmcore.SetDebug(true)
		log.Print("elm2api: ELM_DEBUG on, logging every upstream frame")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("elm2api: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	upstream := elmcore.New(cfg.BaseURL)
	pool := NewPool(upstream, cfg.DefaultModel, cfg.MaxConcurrency, cfg.QueueWaitSeconds, cfg.ModelListTTL)
	pool.SetStatePath(cfg.StateFile)
	pool.SetEmptyPrompt(cfg.emptyPromptEnabled())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	sessions := cfg.Sessions
	cfg.Sessions = nil
	for _, session := range sessions {
		view, addErr := pool.Add(ctx, session.Name, session.Cookie)
		if addErr != nil {
			if errors.Is(addErr, elmcore.ErrSessionExpired) {
				logExpired(session.Name, "", cfg.Listen)
			} else {
				log.Printf("elm2api: session %q ignored: %v", session.Name, addErr)
			}
			pool.addExpired(session.Name, session.Cookie)
			continue
		}
		log.Printf("elm2api: logged in %s (%s) model=%s", view.ID, view.Email, view.Model)
		if cfg.DefaultModel == "" && view.Model != "" {
			cfg.DefaultModel = view.Model
		}
	}
	cancel()
	pool.LoadState()
	if len(pool.Views()) == 0 {
		logNoSession(cfg.Listen, cfg.BaseURL)
	}

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	startSessionWatchdog(watchCtx, upstream, pool, time.Duration(cfg.CheckInterval)*time.Second, cfg.Listen)

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           NewServer(cfg, pool, upstream).Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		log.Printf("elm2api: listening on http://%s (upstream %s, %d session(s), concurrency %d)", cfg.Listen, cfg.BaseURL, len(pool.Views()), cfg.MaxConcurrency)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("elm2api: listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("elm2api: shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}

// startSessionWatchdog periodically revalidates every cookie so an expired
// session is reported in the shell even when no request arrives.
func startSessionWatchdog(ctx context.Context, up Upstream, pool *Pool, interval time.Duration, listen string) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A panic here would take the whole process down, and the only
				// job of this loop is health reporting.
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("elm2api: session watchdog panic: %v", r)
						}
					}()
					checkSessions(ctx, up, pool, listen)
				}()
			}
		}
	}()
}

func checkSessions(ctx context.Context, up Upstream, pool *Pool, listen string) {
	for _, s := range pool.snapshotSessions() {
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := up.IsLoggedIn(reqCtx, s.Cookie)
		cancel()
		if err == nil {
			if s.Expired() {
				s.markExpired(false)
				logRecovered(s.Name, s.User.Email)
			}
			continue
		}
		if errors.Is(err, elmcore.ErrSessionExpired) {
			if !s.Expired() {
				s.markExpired(true)
				logExpired(s.Name, s.User.Email, listen)
			}
			continue
		}
		log.Printf("elm2api: session %q revalidation failed (will retry): %v", s.Name, err)
	}
}
