package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"llm-local-service/internal/config"
	"llm-local-service/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestClientChatDecodesNDJSONAndSendsOptions(t *testing.T) {
	client := New(config.Config{OllamaURL: "http://ollama.test", OllamaModel: "default", NumCtx: 2048, MaxOutputTokens: 99})
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !body.Stream || body.Model != "override" || body.Options.NumCtx != 2048 || body.Options.NumPredict != 99 {
			t.Errorf("request body = %+v", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("{\"message\":{\"role\":\"assistant\",\"content\":\"Hel\"}}\n{\"message\":{\"content\":\"lo\"},\"done\":true}\n")), Header: make(http.Header)}, nil
	})}
	stream, err := client.Chat(context.Background(), domain.ChatRequest{Model: "override", Messages: []domain.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for event := range stream {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		got.WriteString(event.Token)
	}
	if got.String() != "Hello" {
		t.Fatalf("tokens = %q", got.String())
	}
}

func TestDecodeStreamReportsModelErrorAndMalformedJSON(t *testing.T) {
	for name, input := range map[string]string{
		"model error": `{"error":"model unavailable"}`,
		"malformed":   `{"message":`,
	} {
		t.Run(name, func(t *testing.T) {
			err := DecodeStream(strings.NewReader(input), func(domain.StreamEvent) error { return nil })
			if err == nil {
				t.Fatal("expected stream error")
			}
		})
	}
}

func TestClientNon2xxIncludesBody(t *testing.T) {
	client := New(config.Config{OllamaURL: "http://ollama.test"})
	client.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway", Body: io.NopCloser(strings.NewReader("upstream failed")), Header: make(http.Header)}, nil
	})}
	_, err := client.Chat(context.Background(), domain.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "upstream failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeStreamHonorsCallbackCancellation(t *testing.T) {
	err := DecodeStream(strings.NewReader(`{"message":{"content":"x"}}`), func(domain.StreamEvent) error {
		return context.DeadlineExceeded
	})
	if err != context.DeadlineExceeded {
		t.Fatalf("error = %v", err)
	}
}
