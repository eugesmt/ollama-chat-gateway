package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"llm-local-service/internal/config"
	"llm-local-service/internal/domain"
)

type Client struct {
	baseURL string
	model   string
	ctx     int
	output  int
	http    *http.Client
}

func New(cfg config.Config) *Client {
	return NewWithHTTPClient(cfg, &http.Client{})
}

// NewWithHTTPClient is useful when the caller needs transport-level settings
// such as a proxy, custom timeouts, or an in-process test transport.
func NewWithHTTPClient(cfg config.Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{
		baseURL: strings.TrimRight(cfg.OllamaURL, "/"),
		model:   cfg.OllamaModel,
		ctx:     cfg.NumCtx,
		output:  cfg.MaxOutputTokens,
		http:    httpClient,
	}
}

type request struct {
	Model    string           `json:"model"`
	Messages []domain.Message `json:"messages"`
	Stream   bool             `json:"stream"`
	Options  requestOptions   `json:"options"`
}

type requestOptions struct {
	NumCtx     int `json:"num_ctx"`
	NumPredict int `json:"num_predict"`
}

type response struct {
	Message *struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done  bool   `json:"done"`
	Error string `json:"error"`
}

func (c *Client) Chat(ctx context.Context, input domain.ChatRequest) (<-chan domain.StreamEvent, error) {
	model := input.Model
	if model == "" {
		model = c.model
	}
	body, err := json.Marshal(request{
		Model:    model,
		Messages: input.Messages,
		Stream:   true,
		Options: requestOptions{
			NumCtx:     c.ctx,
			NumPredict: c.output,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode Ollama request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Ollama: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if len(bytes.TrimSpace(message)) == 0 {
			return nil, fmt.Errorf("Ollama returned HTTP %s", resp.Status)
		}
		return nil, fmt.Errorf("Ollama returned HTTP %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}

	events := make(chan domain.StreamEvent, 32)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		err := DecodeStream(resp.Body, func(event domain.StreamEvent) error {
			select {
			case events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			events <- domain.StreamEvent{Err: err}
		}
	}()
	return events, nil
}

// DecodeStream decodes Ollama's newline-delimited JSON response. It is kept
// independent of HTTP so malformed streams and server-side errors are easy to test.
func DecodeStream(reader io.Reader, emit func(domain.StreamEvent) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var item response
		if err := json.Unmarshal(line, &item); err != nil {
			return fmt.Errorf("decode Ollama stream: %w", err)
		}
		if item.Error != "" {
			return fmt.Errorf("Ollama: %s", item.Error)
		}
		if item.Message != nil && item.Message.Content != "" {
			if err := emit(domain.StreamEvent{Token: item.Message.Content}); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Ollama stream: %w", err)
	}
	return nil
}
