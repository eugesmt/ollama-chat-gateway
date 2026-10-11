package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-local-service/internal/api"
	"llm-local-service/internal/config"
	"llm-local-service/internal/ollama"
	"llm-local-service/internal/queue"
	"llm-local-service/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	modelClient := ollama.New(cfg)
	manager := queue.New(cfg, modelClient)
	defer manager.Close()
	handler := api.NewHandler(cfg, manager, web.Handler(), logger)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE responses must be allowed to stay open.
		IdleTimeout:       2 * time.Minute,
	}

	stop, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	go func() {
		<-stop.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown failed", "error", err)
		}
		manager.Close()
	}()

	logger.Info("private LLM service started", "listen", cfg.ListenAddr, "ollama", cfg.OllamaURL, "model", cfg.OllamaModel, "workers", cfg.WorkerCount, "queue_capacity", cfg.QueueSize)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server stopped", "error", err)
		os.Exit(1)
	}
}
