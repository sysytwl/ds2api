package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ds2api/internal/elmcore"
)

func main() {
	configPath := flag.String("config", os.Getenv("ELM_CONFIG"), "path to config JSON (env ELM_CONFIG)")
	listen := flag.String("listen", "", "listen address, overrides config listen (default 127.0.0.1:8787)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("elm2api: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	upstream := elmcore.New(cfg.BaseURL)
	pool := NewPool(upstream, cfg.DefaultModel, cfg.MaxConcurrency, cfg.QueueWaitSeconds, cfg.ModelListTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	sessions := cfg.Sessions
	cfg.Sessions = nil
	for _, session := range sessions {
		view, addErr := pool.Add(ctx, session.Name, session.Cookie)
		if addErr != nil {
			log.Printf("elm2api: session %q ignored: %v", session.Name, addErr)
			continue
		}
		log.Printf("elm2api: logged in %s (%s) model=%s", view.ID, view.Email, view.Model)
		if cfg.DefaultModel == "" && view.Model != "" {
			cfg.DefaultModel = view.Model
		}
	}
	cancel()
	if len(pool.Views()) == 0 {
		log.Fatal("elm2api: no usable ELM session; check the SESSION cookie")
	}

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
