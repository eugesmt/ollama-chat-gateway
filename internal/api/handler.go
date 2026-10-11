package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"llm-local-service/internal/config"
	"llm-local-service/internal/domain"
	"llm-local-service/internal/queue"
	"llm-local-service/internal/ratelimit"
)

type Server struct {
	cfg      config.Config
	manager  *queue.Manager
	limiter  *ratelimit.Limiter
	frontend http.Handler
	log      *slog.Logger
}

type jobRequest struct {
	Model    string           `json:"model"`
	Messages []domain.Message `json:"messages"`
	ClientID string           `json:"client_id"`
}

func NewHandler(cfg config.Config, manager *queue.Manager, frontend http.Handler, logger *slog.Logger) http.Handler {
	if frontend == nil {
		frontend = fallbackFrontend()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:      cfg,
		manager:  manager,
		limiter:  ratelimit.New(cfg.RateLimitRPM, time.Minute),
		frontend: frontend,
		log:      logger,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		s.json(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	if r.URL.Path == "/readyz" {
		stats := s.manager.Stats()
		s.json(w, http.StatusOK, map[string]any{"status": "ready", "queue": stats})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if !s.authorized(r) {
			s.errorJSON(w, http.StatusUnauthorized, "unauthorized", "valid API key required")
			return
		}
		s.serveAPI(w, r)
		return
	}
	if r.URL.Path == "/" || !strings.HasPrefix(r.URL.Path, "/") {
		s.frontend.ServeHTTP(w, r)
		return
	}
	// Let an embedded frontend serve its own assets and client-side routes.
	s.frontend.ServeHTTP(w, r)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/jobs":
		s.createJob(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/queue":
		s.json(w, http.StatusOK, s.manager.Stats())
	case strings.HasPrefix(r.URL.Path, "/api/v1/jobs/"):
		s.jobRoute(w, r)
	default:
		s.errorJSON(w, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

func (s *Server) jobRoute(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/")
	parts := strings.Split(strings.Trim(tail, "/"), "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 || (len(parts) == 2 && parts[1] != "events") {
		s.errorJSON(w, http.StatusNotFound, "not_found", "job endpoint not found")
		return
	}
	id := parts[0]
	if len(parts) == 2 {
		if r.Method != http.MethodGet {
			s.errorJSON(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.events(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getJob(w, id)
	case http.MethodDelete:
		s.cancelJob(w, id)
	default:
		s.errorJSON(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.Allow(clientIP(r), time.Now()) {
		w.Header().Set("Retry-After", "60")
		s.errorJSON(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests")
		return
	}
	var input jobRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(maxBodySize(s.cfg))))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		if strings.Contains(err.Error(), "request body too large") || strings.Contains(err.Error(), "http: request body too large") {
			s.errorJSON(w, http.StatusRequestEntityTooLarge, "input_too_large", "request body is too large")
			return
		}
		s.errorJSON(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		s.errorJSON(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON object")
		return
	}
	if err := validate(input, s.cfg); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errInputTooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		s.errorJSON(w, code, "invalid_request", err.Error())
		return
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		model = s.cfg.OllamaModel
	}
	snapshot, err := s.manager.Submit(domain.ChatRequest{Model: model, Messages: input.Messages})
	if err != nil {
		if errors.Is(err, queue.ErrQueueFull) {
			s.errorJSON(w, http.StatusServiceUnavailable, "queue_full", "job queue is full")
			return
		}
		s.errorJSON(w, http.StatusServiceUnavailable, "service_stopped", err.Error())
		return
	}
	s.log.Info("job queued", "job_id", snapshot.ID, "client_ip", clientIP(r), "model", model)
	s.json(w, http.StatusAccepted, map[string]any{
		"job_id":     snapshot.ID,
		"status":     string(snapshot.Status),
		"events_url": "/api/v1/jobs/" + snapshot.ID + "/events",
	})
}

var errInputTooLarge = errors.New("input too large")

func validate(input jobRequest, cfg config.Config) error {
	if len(input.Messages) == 0 {
		return errors.New("messages must not be empty")
	}
	if len(input.Messages) > cfg.MaxMessages {
		return fmt.Errorf("messages exceed max of %d", cfg.MaxMessages)
	}
	chars := 0
	for _, message := range input.Messages {
		if message.Role != "user" && message.Role != "assistant" && message.Role != "system" {
			return fmt.Errorf("unsupported message role %q", message.Role)
		}
		if strings.TrimSpace(message.Content) == "" {
			return errors.New("message content must not be empty")
		}
		chars += utf8.RuneCountInString(message.Content)
	}
	if chars > cfg.MaxInputChars {
		return fmt.Errorf("input exceeds max of %d characters: %w", cfg.MaxInputChars, errInputTooLarge)
	}
	return nil
}

func (s *Server) getJob(w http.ResponseWriter, id string) {
	snapshot, err := s.manager.Get(id)
	if errors.Is(err, queue.ErrNotFound) {
		s.errorJSON(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	s.json(w, http.StatusOK, snapshot)
}

func (s *Server) cancelJob(w http.ResponseWriter, id string) {
	snapshot, err := s.manager.Cancel(id)
	if errors.Is(err, queue.ErrNotFound) {
		s.errorJSON(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	s.json(w, http.StatusAccepted, snapshot)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, id string) {
	replay, stream, unsubscribe, terminal, err := s.manager.Subscribe(id)
	if errors.Is(err, queue.ErrNotFound) {
		s.errorJSON(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	defer unsubscribe()
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.errorJSON(w, http.StatusInternalServerError, "stream_unsupported", "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	replay = replayAfterEventID(replay, strings.TrimSpace(r.Header.Get("Last-Event-ID")))
	for _, event := range replay {
		if err := writeEvent(w, event); err != nil {
			return
		}
	}
	flusher.Flush()
	if terminal {
		return
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-stream:
			if !open {
				return
			}
			if err := writeEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w io.Writer, event queue.Event) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\nid: %s\n\n", event.Name, data, eventID(event)); err != nil {
		return err
	}
	return nil
}

func replayAfterEventID(events []queue.Event, lastEventID string) []queue.Event {
	if lastEventID == "" {
		return events
	}
	for index, event := range events {
		if eventID(event) == lastEventID {
			return events[index+1:]
		}
	}
	// An unknown ID may belong to an expired or different stream. Replaying
	// everything is safer than silently omitting output.
	return events
}

func eventID(event queue.Event) string {
	return event.At.UTC().Format(time.RFC3339Nano)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.APIKey == "" {
		return true
	}
	key := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if key == "" {
		const prefix = "Bearer "
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(auth, prefix) {
			key = strings.TrimSpace(strings.TrimPrefix(auth, prefix))
		}
	}
	if key == "" && isSSEEndpoint(r) {
		key = strings.TrimSpace(r.URL.Query().Get("api_key"))
	}
	return key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.APIKey)) == 1
}

// Query-string credentials are accepted only for the native EventSource
// endpoint, because EventSource cannot set Authorization headers. They are
// deliberately rejected for every other API route.
func isSSEEndpoint(r *http.Request) bool {
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/v1/jobs/") {
		return false
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/")
	parts := strings.Split(strings.Trim(tail, "/"), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] == "events"
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

func maxBodySize(cfg config.Config) int {
	return max(64*1024, cfg.MaxInputChars*cfg.MaxMessages*4+16*1024)
}

func (s *Server) json(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) errorJSON(w http.ResponseWriter, status int, code, message string) {
	s.json(w, status, map[string]any{"error": code, "message": message})
}

func fallbackFrontend() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Private LLM service</title></head><body><h1>Private LLM service</h1><p>Backend is running. Configure the web package to use the chat UI.</p></body></html>`)
	})
}
