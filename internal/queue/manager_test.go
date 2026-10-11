package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"llm-local-service/internal/config"
	"llm-local-service/internal/domain"
)

type blockingClient struct {
	mu      sync.Mutex
	calls   []string
	started chan string
	release chan struct{}
}

func (c *blockingClient) Chat(ctx context.Context, request domain.ChatRequest) (<-chan domain.StreamEvent, error) {
	c.mu.Lock()
	c.calls = append(c.calls, request.Messages[0].Content)
	c.mu.Unlock()
	c.started <- request.Messages[0].Content
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	stream := make(chan domain.StreamEvent, 1)
	stream <- domain.StreamEvent{Token: request.Messages[0].Content}
	close(stream)
	return stream, nil
}

func testConfig() config.Config {
	return config.Config{
		OllamaModel:     "test-model",
		WorkerCount:     1,
		QueueSize:       2,
		MaxInputChars:   100,
		MaxMessages:     10,
		MaxOutputTokens: 10,
		NumCtx:          128,
		RequestTimeout:  time.Minute,
		JobTTL:          time.Hour,
	}
}

func waitStatus(t *testing.T, manager *Manager, id string, status Status) Snapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := manager.Get(id)
		if err == nil && snapshot.Status == status {
			return snapshot
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatalf("job %s did not reach %s", id, status)
	return Snapshot{}
}

func TestManagerQueueOrderAndFull(t *testing.T) {
	client := &blockingClient{started: make(chan string, 4), release: make(chan struct{}, 4)}
	manager := New(testConfig(), client)
	defer manager.Close()

	first, err := manager.Submit(domain.ChatRequest{Messages: []domain.Message{{Role: "user", Content: "first"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-client.started; got != "first" {
		t.Fatalf("first worker call = %q", got)
	}
	second, err := manager.Submit(domain.ChatRequest{Messages: []domain.Message{{Role: "user", Content: "second"}}})
	if err != nil {
		t.Fatal(err)
	}
	third, err := manager.Submit(domain.ChatRequest{Messages: []domain.Message{{Role: "user", Content: "third"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Submit(domain.ChatRequest{Messages: []domain.Message{{Role: "user", Content: "rejected"}}}); err != ErrQueueFull {
		t.Fatalf("full queue error = %v, want %v", err, ErrQueueFull)
	}
	if stats := manager.Stats(); stats.Queued != 2 || stats.QueueSize != 2 {
		t.Fatalf("queue stats = %+v", stats)
	}

	for range 2 {
		client.release <- struct{}{}
		<-client.started
	}
	client.release <- struct{}{}
	// The final release is consumed by the third request.
	waitStatus(t, manager, first.ID, StatusCompleted)
	waitStatus(t, manager, second.ID, StatusCompleted)
	waitStatus(t, manager, third.ID, StatusCompleted)
	client.mu.Lock()
	calls := append([]string(nil), client.calls...)
	client.mu.Unlock()
	want := []string{"first", "second", "third"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call order = %v, want %v", calls, want)
		}
	}
}

func TestManagerCancelQueuedJobPublishesTerminalEvents(t *testing.T) {
	client := &blockingClient{started: make(chan string, 2), release: make(chan struct{}, 2)}
	manager := New(testConfig(), client)
	defer manager.Close()

	first, err := manager.Submit(domain.ChatRequest{Messages: []domain.Message{{Role: "user", Content: "running"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-client.started
	queued, err := manager.Submit(domain.ChatRequest{Messages: []domain.Message{{Role: "user", Content: "queued"}}})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := manager.Cancel(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("cancel status = %s", cancelled.Status)
	}
	replay, _, unsubscribe, terminal, err := manager.Subscribe(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	if !terminal || len(replay) < 3 {
		t.Fatalf("terminal replay = %+v, terminal=%v", replay, terminal)
	}
	if replay[len(replay)-1].Name != "done" {
		t.Fatalf("last event = %s", replay[len(replay)-1].Name)
	}
	client.release <- struct{}{}
	waitStatus(t, manager, first.ID, StatusCompleted)
}
