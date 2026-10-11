package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndex(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()

	Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", res.Code, http.StatusOK)
	}
	if !strings.Contains(res.Body.String(), "Приватный AI-сервис") {
		t.Fatal("index page does not contain the application title")
	}
}

func TestHandlerServesAssets(t *testing.T) {
	for _, path := range []string{"/styles.css", "/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()

		Handler().ServeHTTP(res, req)

		if res.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, res.Code, http.StatusOK)
		}
	}
}
