package central

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"relay-central/internal/config"
)

func TestHealthzReturnsEmbeddedVersion(t *testing.T) {
	app, err := New(Config{
		DataDir:       t.TempDir(),
		AdminPassword: "test-password",
		MasterKey:     base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var response struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if response.Status != "ok" {
		t.Errorf("status = %q, want ok", response.Status)
	}
	if response.Version != config.Version {
		t.Errorf("version = %q, want embedded version %q", response.Version, config.Version)
	}
}
