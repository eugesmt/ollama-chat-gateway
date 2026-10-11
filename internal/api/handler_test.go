package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-local-service/internal/config"
	"llm-local-service/internal/domain"
	"llm-local-service/internal/ollama"
	"llm-local-service/internal/queue"
	"llm-local-service/web"
)

type immediateClient struct{}

func (immediateClient) Chat(context.Context, domain.ChatRequest) (<-chan domain.StreamEvent, error) {
	stream := make(chan domain.StreamEvent, 1)
	stream <- domain.StreamEvent{Token: "hello"}
	close(stream)
	return stream, nil
}

func apiTestConfig() config.Config {
	return config.Config{
		OllamaModel:     "test-model",
		WorkerCount:     1,
		QueueSize:       2,
		MaxInputChars:   10,
		MaxMessages:     2,
		MaxOutputTokens: 10,
		NumCtx:          128,
		RateLimitRPM:    10,
		RequestTimeout:  time.Second,
		JobTTL:          time.Hour,
	}
}

func TestValidateRejectsOversizedAndInvalidMessages(t *testing.T) {
	cfg := apiTestConfig()
	if err := validate(jobRequest{Messages: []domain.Message{{Role: "user", Content: "12345678901"}}}, cfg); err == nil || !strings.Contains(err.Error(), "input exceeds") {
		t.Fatalf("oversized validation error = %v", err)
	}
	if err := validate(jobRequest{Messages: []domain.Message{{Role: "tool", Content: "ok"}}}, cfg); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("role validation error = %v", err)
	}
	if err := validate(jobRequest{Messages: []domain.Message{{Role: "user", Content: ""}}}, cfg); err == nil {
		t.Fatal("empty content was accepted")
	}
}

func TestHandlerAuthRateLimitAndSSEReplay(t *testing.T) {
	cfg := apiTestConfig()
	cfg.APIKey = "secret"
	cfg.RateLimitRPM = 1
	manager := queue.New(cfg, immediateClient{})
	defer manager.Close()
	handler := NewHandler(cfg, manager, nil, nil)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("X-API-Key", "secret")
	request.RemoteAddr = "192.0.2.10:1234"
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, request)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, body=%s", created.Code, created.Body.String())
	}
	var response struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID == "" {
		t.Fatal("missing job id")
	}

	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"messages":[{"role":"user","content":"again"}]}`))
	secondRequest.Header.Set("X-API-Key", "secret")
	secondRequest.RemoteAddr = "192.0.2.10:5678"
	handler.ServeHTTP(second, secondRequest)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit status = %d", second.Code)
	}

	stream := httptest.NewRecorder()
	streamRequest := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+response.ID+"/events", nil)
	streamRequest.Header.Set("X-API-Key", "secret")
	handler.ServeHTTP(stream, streamRequest)
	body := stream.Body.String()
	for _, fragment := range []string{"event: status", "event: token", "hello", "event: done"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("SSE body missing %q: %s", fragment, body)
		}
	}
}

func TestHandlerReturns413ForInputLimit(t *testing.T) {
	cfg := apiTestConfig()
	manager := queue.New(cfg, immediateClient{})
	defer manager.Close()
	handler := NewHandler(cfg, manager, nil, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"messages":[{"role":"user","content":"12345678901"}]}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestDefaultUIBurstTriggersRateLimit(t *testing.T) {
	cfg := apiTestConfig()
	cfg.RateLimitRPM = 5
	cfg.QueueSize = 8
	manager := queue.New(cfg, immediateClient{})
	defer manager.Close()
	handler := NewHandler(cfg, manager, nil, nil)

	for index := 0; index < 6; index++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		request.RemoteAddr = "192.0.2.20:1234"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		want := http.StatusAccepted
		if index == 5 {
			want = http.StatusTooManyRequests
		}
		if recorder.Code != want {
			t.Fatalf("request %d status = %d, want %d; body=%s", index+1, recorder.Code, want, recorder.Body.String())
		}
	}
}

func TestHandlerFullPathWithFakeOllamaAndEmbeddedWeb(t *testing.T) {
	fakeOllama := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"hello"}}`)
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":" world"},"done":true}`)
	})

	cfg := apiTestConfig()
	cfg.APIKey = "secret"
	cfg.OllamaURL = "http://ollama.test"
	cfg.MaxInputChars = 100
	modelClient := ollama.NewWithHTTPClient(cfg, &http.Client{Transport: fakeRoundTripper{handler: fakeOllama}})
	manager := queue.New(cfg, modelClient)
	defer manager.Close()
	handler := NewHandler(cfg, manager, web.Handler(), nil)

	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	rootBody, _ := io.ReadAll(root.Body)
	if root.Code != http.StatusOK || !strings.Contains(string(rootBody), "Приватный AI-сервис") {
		t.Fatalf("root = %d, body contains embedded UI=%v", root.Code, strings.Contains(string(rootBody), "Приватный AI-сервис"))
	}
	for _, asset := range []string{"/app.js", "/styles.css"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("asset %s status = %d", asset, response.Code)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, request)
	var job struct {
		ID        string `json:"job_id"`
		EventsURL string `json:"events_url"`
	}
	if err := json.NewDecoder(created.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	if created.Code != http.StatusAccepted || job.ID == "" || job.EventsURL == "" {
		t.Fatalf("create = %d, job = %+v", created.Code, job)
	}

	// Native EventSource cannot set Authorization, so the query key is accepted
	// for this endpoint only and the completed job can still be replayed.
	events := httptest.NewRecorder()
	handler.ServeHTTP(events, httptest.NewRequest(http.MethodGet, job.EventsURL+"?api_key=secret", nil))
	eventBody, _ := io.ReadAll(events.Body)
	body := string(eventBody)
	if events.Code != http.StatusOK || !strings.Contains(events.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("SSE response = %d, content-type=%q", events.Code, events.Header().Get("Content-Type"))
	}
	for _, fragment := range []string{"event: status", `"queue_position"`, "event: token", `"text":"hello"`, "event: done", `"status":"completed"`, `"ttft_ms"`, `"duration_ms"`} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("SSE body missing %q: %s", fragment, body)
		}
	}
	lastTokenID := lastSSEEventID(body, "token")
	if lastTokenID == "" {
		t.Fatalf("SSE body has no token event ID: %s", body)
	}

	resumed := httptest.NewRecorder()
	resumeRequest := httptest.NewRequest(http.MethodGet, job.EventsURL+"?api_key=secret", nil)
	resumeRequest.Header.Set("Last-Event-ID", lastTokenID)
	handler.ServeHTTP(resumed, resumeRequest)
	resumedBody := resumed.Body.String()
	if strings.Contains(resumedBody, "event: token") || strings.Contains(resumedBody, `"text":"hello"`) {
		t.Fatalf("known Last-Event-ID replayed token events: %s", resumedBody)
	}
	if !strings.Contains(resumedBody, "event: done") {
		t.Fatalf("known Last-Event-ID omitted subsequent done event: %s", resumedBody)
	}

	unknown := httptest.NewRecorder()
	unknownRequest := httptest.NewRequest(http.MethodGet, job.EventsURL+"?api_key=secret", nil)
	unknownRequest.Header.Set("Last-Event-ID", "unknown-event-id")
	handler.ServeHTTP(unknown, unknownRequest)
	if !strings.Contains(unknown.Body.String(), `"text":"hello"`) {
		t.Fatalf("unknown Last-Event-ID did not safely replay all events: %s", unknown.Body.String())
	}

	queryOnOtherEndpoint := httptest.NewRecorder()
	handler.ServeHTTP(queryOnOtherEndpoint, httptest.NewRequest(http.MethodGet, "/api/v1/queue?api_key=secret", nil))
	if queryOnOtherEndpoint.Code != http.StatusUnauthorized {
		t.Fatalf("query API key was accepted on non-SSE endpoint: %d", queryOnOtherEndpoint.Code)
	}
}

type fakeRoundTripper struct {
	handler http.Handler
}

func (t fakeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func lastSSEEventID(body, eventName string) string {
	var result string
	for _, block := range strings.Split(body, "\n\n") {
		if !strings.Contains(block, "event: "+eventName+"\n") {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "id: ") {
				result = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
			}
		}
	}
	return result
}
