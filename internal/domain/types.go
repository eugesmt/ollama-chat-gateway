package domain

import "context"

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string    `json:"model,omitempty"`
	Messages []Message `json:"messages"`
}

type StreamEvent struct {
	Token string
	Err   error
}

type ModelClient interface {
	Chat(ctx context.Context, request ChatRequest) (<-chan StreamEvent, error)
}
