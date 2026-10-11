package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultModel = "llama3.2:3b"

type Config struct {
	ListenAddr      string
	OllamaURL       string
	OllamaModel     string
	WorkerCount     int
	QueueSize       int
	MaxInputChars   int
	MaxMessages     int
	MaxOutputTokens int
	NumCtx          int
	RateLimitRPM    int
	JobTTL          time.Duration
	RequestTimeout  time.Duration
	APIKey          string
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:      envString("LISTEN_ADDR", ":8080"),
		OllamaURL:       envString("OLLAMA_URL", "http://127.0.0.1:11434"),
		OllamaModel:     envString("OLLAMA_MODEL", DefaultModel),
		WorkerCount:     envInt("WORKER_COUNT", 1),
		QueueSize:       envInt("QUEUE_SIZE", 8),
		MaxInputChars:   envInt("MAX_INPUT_CHARS", 8000),
		MaxMessages:     envInt("MAX_MESSAGES", 20),
		MaxOutputTokens: envInt("MAX_OUTPUT_TOKENS", 512),
		NumCtx:          envInt("NUM_CTX", 4096),
		RateLimitRPM:    envInt("RATE_LIMIT_RPM", 5),
		APIKey:          strings.TrimSpace(os.Getenv("API_KEY")),
	}

	var err error
	if c.JobTTL, err = envDuration("JOB_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if c.RequestTimeout, err = envDuration("REQUEST_TIMEOUT", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if c.WorkerCount < 1 || c.QueueSize < 1 || c.MaxInputChars < 1 || c.MaxMessages < 1 || c.MaxOutputTokens < 1 || c.NumCtx < 1 {
		return Config{}, fmt.Errorf("worker, queue, input, message, output and context limits must be positive")
	}
	if c.RateLimitRPM < 0 {
		return Config{}, fmt.Errorf("RATE_LIMIT_RPM must not be negative")
	}
	if c.JobTTL < 0 || c.RequestTimeout < 0 {
		return Config{}, fmt.Errorf("JOB_TTL and REQUEST_TIMEOUT must not be negative")
	}
	return c, nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return duration, nil
}
