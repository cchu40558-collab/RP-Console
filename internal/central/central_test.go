package central

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

func TestSiteSettingsRequireLoginAndExposeHelperAvailability(t *testing.T) {
	app, err := New(Config{
		DataDir:         t.TempDir(),
		AdminPassword:   "test-password",
		MasterKey:       base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
		PrivilegedApply: "/usr/local/lib/rp-console/apply-site",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	unauthenticated := httptest.NewRecorder()
	app.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated site status = %d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}

	app.sessions["test-session"] = time.Now().Add(time.Hour)
	request := httptest.NewRequest(http.MethodGet, "/api/site", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-session"})
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated site status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response siteView
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.CanApply {
		t.Error("CanApply = false, want true when the configured helper is available")
	}
}

func TestValidateSiteDomain(t *testing.T) {
	for _, value := range []string{"rp-console.wakeup-ai.top", "RP-CONSOLE.EXAMPLE.COM"} {
		if _, err := validateSiteDomain(value); err != nil {
			t.Errorf("validateSiteDomain(%q) error = %v", value, err)
		}
	}
	for _, value := range []string{"localhost", "https://example.com", "bad_name.example.com", "-bad.example.com"} {
		if _, err := validateSiteDomain(value); err == nil {
			t.Errorf("validateSiteDomain(%q) succeeded, want error", value)
		}
	}
}

func TestSiteApplyJobLifecyclePreservesPreviousSiteUntilSuccess(t *testing.T) {
	app, err := New(Config{
		DataDir:       t.TempDir(),
		AdminPassword: "test-password",
		MasterKey:     base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	previous := siteConfig{Domain: "old.example.com", CertificateSHA256: "old"}
	if err := app.store.saveSite(previous); err != nil {
		t.Fatalf("saveSite() error = %v", err)
	}
	job := siteApplyJob{ID: "job-1", Domain: "new.example.com", Status: "queued", Stage: "queued"}
	if err := app.store.queueSiteApply(job); err != nil {
		t.Fatalf("queueSiteApply() error = %v", err)
	}
	if !app.store.siteApplyActive() {
		t.Fatal("site apply should be active while queued")
	}
	if err := app.store.finishSiteApply(job.ID, "failed", "failed", "test failure", nil); err != nil {
		t.Fatalf("finishSiteApply(failed) error = %v", err)
	}
	view := app.store.siteView(true)
	if view.Domain != previous.Domain {
		t.Fatalf("failed job changed site domain to %q, want %q", view.Domain, previous.Domain)
	}
	if view.Job.Status != "failed" {
		t.Fatalf("job status = %q, want failed", view.Job.Status)
	}
	updated := siteConfig{Domain: job.Domain, CertificateSHA256: "new"}
	if err := app.store.finishSiteApply(job.ID, "succeeded", "complete", "done", &updated); err != nil {
		t.Fatalf("finishSiteApply(succeeded) error = %v", err)
	}
	view = app.store.siteView(true)
	if view.Domain != updated.Domain || view.Job.Status != "succeeded" {
		t.Fatalf("success view = %+v, want updated site and succeeded job", view)
	}
}
