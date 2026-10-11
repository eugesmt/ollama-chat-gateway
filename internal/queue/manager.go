package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"llm-local-service/internal/config"
	"llm-local-service/internal/domain"
)

var (
	ErrQueueFull = errors.New("job queue is full")
	ErrStopped   = errors.New("job manager is stopped")
	ErrNotFound  = errors.New("job not found")
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

type Event struct {
	Name string    `json:"-"`
	Data any       `json:"data"`
	At   time.Time `json:"at"`
}

type Snapshot struct {
	ID            string     `json:"job_id"`
	Model         string     `json:"model,omitempty"`
	Status        Status     `json:"status"`
	Error         string     `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	WaitMS        int64      `json:"wait_ms,omitempty"`
	TTFTMS        int64      `json:"ttft_ms,omitempty"`
	DurationMS    int64      `json:"duration_ms,omitempty"`
	QueuePosition int        `json:"queue_position,omitempty"`
}

type QueueStats struct {
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	QueueSize int `json:"queue_size"`
	Capacity  int `json:"capacity"`
	Workers   int `json:"workers"`
}

type job struct {
	id         string
	request    domain.ChatRequest
	created    time.Time
	started    time.Time
	firstToken time.Time
	ended      time.Time
	status     Status
	err        string
	ctx        context.Context
	cancel     context.CancelFunc
	events     []Event
	subs       map[int]chan Event
	nextSub    int
}

type Manager struct {
	mu       sync.RWMutex
	cfg      config.Config
	client   domain.ModelClient
	queue    chan *job
	jobs     map[string]*job
	rootCtx  context.Context
	stop     context.CancelFunc
	wg       sync.WaitGroup
	stopped  bool
	sequence atomic.Uint64
}

func New(cfg config.Config, client domain.ModelClient) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:     cfg,
		client:  client,
		queue:   make(chan *job, cfg.QueueSize),
		jobs:    make(map[string]*job),
		rootCtx: ctx,
		stop:    cancel,
	}
	for i := 0; i < cfg.WorkerCount; i++ {
		m.wg.Add(1)
		go m.worker()
	}
	if cfg.JobTTL > 0 {
		m.wg.Add(1)
		go m.janitor()
	}
	return m
}

func (m *Manager) Submit(request domain.ChatRequest) (Snapshot, error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return Snapshot{}, ErrStopped
	}
	if len(m.queue) >= cap(m.queue) {
		return Snapshot{}, ErrQueueFull
	}
	jobCtx, cancel := context.WithCancel(m.rootCtx)
	id := fmt.Sprintf("job-%d", m.sequence.Add(1))
	j := &job{
		id:      id,
		request: request,
		created: now,
		status:  StatusQueued,
		ctx:     jobCtx,
		cancel:  cancel,
		subs:    make(map[int]chan Event),
	}
	m.jobs[id] = j
	position := len(m.queue) + 1
	j.events = append(j.events, Event{Name: "status", Data: map[string]any{
		"job_id": id, "status": string(StatusQueued), "queue_position": position,
	}, At: now})
	select {
	case m.queue <- j:
		return m.snapshotLocked(j), nil
	default:
		delete(m.jobs, id)
		cancel()
		return Snapshot{}, ErrQueueFull
	}
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.rootCtx.Done():
			return
		case j := <-m.queue:
			if j == nil {
				return
			}
			if !m.claim(j) {
				continue
			}
			m.run(j)
		}
	}
}

func (m *Manager) claim(j *job) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.status != StatusQueued {
		return false
	}
	now := time.Now()
	j.status = StatusRunning
	j.started = now
	m.addEventLocked(j, "status", m.statusDataLocked(j))
	return true
}

func (m *Manager) run(j *job) {
	ctx := j.ctx
	var cancel context.CancelFunc
	if m.cfg.RequestTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, m.cfg.RequestTimeout)
		defer cancel()
	}
	stream, err := m.client.Chat(ctx, j.request)
	if err != nil {
		m.finishError(j, err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			if j.ctx.Err() != nil {
				m.cancelJob(j)
			} else {
				m.finishError(j, ctx.Err())
			}
			return
		case event, ok := <-stream:
			if !ok {
				m.finish(j)
				return
			}
			if event.Err != nil {
				if errors.Is(j.ctx.Err(), context.Canceled) {
					m.cancelJob(j)
				} else {
					m.finishError(j, event.Err)
				}
				return
			}
			if event.Token != "" {
				m.token(j, event.Token)
			}
		}
	}
}

func (m *Manager) token(j *job, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.status == StatusRunning {
		if j.firstToken.IsZero() {
			j.firstToken = time.Now()
		}
		m.addEventLocked(j, "token", map[string]any{"job_id": j.id, "text": token})
	}
}

func (m *Manager) finish(j *job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if terminal(j.status) {
		return
	}
	now := time.Now()
	j.status = StatusCompleted
	j.ended = now
	m.addEventLocked(j, "status", m.statusDataLocked(j))
	m.addEventLocked(j, "done", m.doneDataLocked(j))
	m.closeSubscribersLocked(j)
}

func (m *Manager) finishError(j *job, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if terminal(j.status) {
		return
	}
	now := time.Now()
	j.status = StatusFailed
	j.ended = now
	j.err = err.Error()
	m.addEventLocked(j, "status", m.statusDataLocked(j))
	m.addEventLocked(j, "error", map[string]any{"job_id": j.id, "error": j.err})
	m.addEventLocked(j, "done", m.doneDataLocked(j))
	m.closeSubscribersLocked(j)
}

func (m *Manager) cancelJob(j *job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if terminal(j.status) {
		return
	}
	j.cancel()
	j.status = StatusCancelled
	j.ended = time.Now()
	j.err = "job cancelled"
	m.addEventLocked(j, "status", m.statusDataLocked(j))
	m.addEventLocked(j, "done", m.doneDataLocked(j))
	m.closeSubscribersLocked(j)
}

func (m *Manager) doneDataLocked(j *job) map[string]any {
	data := map[string]any{"job_id": j.id, "status": string(j.status)}
	if j.err != "" {
		data["error"] = j.err
	}
	m.addTimingDataLocked(j, data)
	return data
}

func (m *Manager) statusDataLocked(j *job) map[string]any {
	data := map[string]any{"job_id": j.id, "status": string(j.status)}
	if j.status == StatusQueued {
		data["queue_position"] = len(m.queue) + 1
	}
	if j.err != "" {
		data["error"] = j.err
	}
	if terminal(j.status) || j.status == StatusRunning {
		m.addTimingDataLocked(j, data)
	}
	return data
}

func (m *Manager) addTimingDataLocked(j *job, data map[string]any) {
	if !j.started.IsZero() {
		data["wait_ms"] = j.started.Sub(j.created).Milliseconds()
	}
	if !j.firstToken.IsZero() {
		data["ttft_ms"] = j.firstToken.Sub(j.created).Milliseconds()
	}
	if !j.ended.IsZero() && !j.started.IsZero() {
		data["duration_ms"] = j.ended.Sub(j.started).Milliseconds()
	}
	if !j.ended.IsZero() {
		data["total_ms"] = j.ended.Sub(j.created).Milliseconds()
	}
}

func (m *Manager) addEventLocked(j *job, name string, data any) {
	event := Event{Name: name, Data: data, At: time.Now()}
	j.events = append(j.events, event)
	for _, subscriber := range j.subs {
		select {
		case subscriber <- event:
		default:
			// The history remains available for a reconnect. The subscriber
			// buffer is sized from the output-token limit to avoid this path
			// during normal Ollama streams.
		}
	}
}

func (m *Manager) closeSubscribersLocked(j *job) {
	for id, subscriber := range j.subs {
		close(subscriber)
		delete(j.subs, id)
	}
}

func (m *Manager) snapshotLocked(j *job) Snapshot {
	snapshot := Snapshot{ID: j.id, Model: j.request.Model, Status: j.status, Error: j.err, CreatedAt: j.created, QueuePosition: len(m.queue)}
	if !j.started.IsZero() {
		started := j.started
		snapshot.StartedAt = &started
		snapshot.WaitMS = started.Sub(j.created).Milliseconds()
	}
	if !j.firstToken.IsZero() {
		snapshot.TTFTMS = j.firstToken.Sub(j.created).Milliseconds()
	}
	if !j.ended.IsZero() {
		ended := j.ended
		snapshot.EndedAt = &ended
		if !j.started.IsZero() {
			snapshot.DurationMS = ended.Sub(j.started).Milliseconds()
		}
	}
	return snapshot
}

func (m *Manager) Get(id string) (Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	return m.snapshotLocked(j), nil
}

func (m *Manager) Subscribe(id string) ([]Event, <-chan Event, func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, nil, nil, false, ErrNotFound
	}
	replay := append([]Event(nil), j.events...)
	if terminal(j.status) {
		return replay, nil, func() {}, true, nil
	}
	j.nextSub++
	subID := j.nextSub
	bufferSize := m.cfg.MaxOutputTokens + 32
	if bufferSize < 256 {
		bufferSize = 256
	}
	// Ollama is capped by MaxOutputTokens, so this holds the complete stream
	// for a slow SSE reader instead of silently dropping the terminal event.
	channel := make(chan Event, bufferSize)
	j.subs[subID] = channel
	return replay, channel, func() { m.unsubscribe(id, subID, channel) }, false, nil
}

func (m *Manager) unsubscribe(id string, subID int, channel <-chan Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return
	}
	if current, exists := j.subs[subID]; exists && (<-chan Event)(current) == channel {
		delete(j.subs, subID)
		close(current)
	}
}

func (m *Manager) Cancel(id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	if !terminal(j.status) {
		j.cancel()
		j.status = StatusCancelled
		j.ended = time.Now()
		j.err = "job cancelled"
		m.addEventLocked(j, "status", m.statusDataLocked(j))
		m.addEventLocked(j, "done", m.doneDataLocked(j))
		m.closeSubscribersLocked(j)
	}
	return m.snapshotLocked(j), nil
}

func (m *Manager) Stats() QueueStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stats := QueueStats{QueueSize: len(m.queue), Capacity: cap(m.queue), Workers: m.cfg.WorkerCount}
	for _, j := range m.jobs {
		switch j.status {
		case StatusQueued:
			stats.Queued++
		case StatusRunning:
			stats.Running++
		case StatusCompleted:
			stats.Completed++
		case StatusFailed:
			stats.Failed++
		case StatusCancelled:
			stats.Cancelled++
		}
	}
	return stats
}

func (m *Manager) janitor() {
	defer m.wg.Done()
	interval := m.cfg.JobTTL / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.rootCtx.Done():
			return
		case now := <-ticker.C:
			m.cleanup(now)
		}
	}
}

func (m *Manager) cleanup(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, j := range m.jobs {
		if terminal(j.status) && now.Sub(j.ended) >= m.cfg.JobTTL {
			delete(m.jobs, id)
		}
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	for _, j := range m.jobs {
		if !terminal(j.status) {
			j.cancel()
			j.status = StatusCancelled
			j.ended = time.Now()
			j.err = "service shutting down"
			m.addEventLocked(j, "status", m.statusDataLocked(j))
			m.addEventLocked(j, "done", m.doneDataLocked(j))
			m.closeSubscribersLocked(j)
		}
	}
	m.stop()
	m.mu.Unlock()
	m.wg.Wait()
}

func terminal(status Status) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}
