package central

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"relay-central/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

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

func TestManagementPanelURLUsesNormalizedHTTPSDomainAndBasePath(t *testing.T) {
	record := serverRecord{
		Address:  "RP2.WAKEUP-AI.TOP.",
		Scheme:   "HTTPS",
		Port:     2083,
		BasePath: " /manage/token/ ",
	}

	got, err := managementPanelURL(record)
	if err != nil {
		t.Fatalf("managementPanelURL() error = %v", err)
	}
	if want := "https://rp2.wakeup-ai.top:2083/manage/token/"; got != want {
		t.Errorf("managementPanelURL() = %q, want %q", got, want)
	}
}

func TestManagementPanelURLRejectsUnsafeOrCertificateIncompatibleTargets(t *testing.T) {
	for _, record := range []serverRecord{
		{Address: "153.75.235.141", Scheme: "https", Port: 2083},
		{Address: "rp2.wakeup-ai.top", Scheme: "http", Port: 2083},
		{Address: "rp2.wakeup-ai.top", Scheme: "https", Port: 2083, BasePath: "/safe?next=bad"},
	} {
		if got, err := managementPanelURL(record); err == nil || got != "" {
			t.Errorf("managementPanelURL(%+v) = %q, %v; want rejected target", record, got, err)
		}
	}
}

func TestServerViewOmitsPanelURLForLegacyIPAddressRecord(t *testing.T) {
	view := (serverRecord{Address: "153.75.235.141", Scheme: "https", Port: 2083}).toView()
	if view.PanelURL != "" {
		t.Errorf("PanelURL = %q, want empty for legacy IP record", view.PanelURL)
	}
}

func TestRemovedRemotePanelRoutesReturnNotFound(t *testing.T) {
	app, err := New(Config{
		DataDir:       t.TempDir(),
		AdminPassword: "test-password",
		MasterKey:     base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, route := range []string{"/servers/server-1/panel", "/api/servers/server-1/lines"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		if route[0:4] == "/api" {
			app.sessions["test-session"] = time.Now().Add(time.Hour)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-session"})
		}
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", route, recorder.Code, http.StatusNotFound)
		}
	}
}

func TestRefreshSynchronizesServerSummaryWithoutExposingManagementLatency(t *testing.T) {
	app, err := New(Config{
		DataDir:       t.TempDir(),
		AdminPassword: "test-password",
		MasterKey:     base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ciphertext, err := app.store.encrypt("central-token")
	if err != nil {
		t.Fatalf("encrypt() error = %v", err)
	}
	record := serverRecord{ID: "server-1", Name: "node", Address: "node.example.com", Scheme: "https", Port: 2083, TokenCiphertext: ciphertext, Status: "unknown", LatencyMS: 987}
	if err := app.store.add(record); err != nil {
		t.Fatalf("add() error = %v", err)
	}
	app.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch {
		case strings.HasSuffix(request.URL.Path, "/central/capabilities"):
			body = `{"success":true,"obj":{"product":"relay-panel","role":"node","centralProtocolVersion":1,"nodeId":"stable-node","readOnly":true}}`
		case strings.HasSuffix(request.URL.Path, "/central/summary"):
			body = `{"success":true,"obj":{"centralProtocolVersion":1,"nodeId":"stable-node","panelVersion":"2.0.22","xray":{"state":"running","error":""},"lines":{"total":3,"healthy":2,"abnormal":1,"expired":0},"traffic":{"totalBytes":123456}}}`
		default:
			t.Fatalf("unexpected request URL %q", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	app.sessions["test-session"] = time.Now().Add(time.Hour)
	start := httptest.NewRequest(http.MethodPost, "/api/operations/refresh", bytes.NewBufferString(`{}`))
	start.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-session"})
	started := httptest.NewRecorder()
	app.Handler().ServeHTTP(started, start)
	if started.Code != http.StatusAccepted {
		t.Fatalf("refresh status = %d, want %d: %s", started.Code, http.StatusAccepted, started.Body.String())
	}

	deadline := time.Now().Add(time.Second)
	for app.refreshActive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if app.refreshActive() {
		t.Fatal("refresh did not finish")
	}
	updated, err := app.store.get(record.ID)
	if err != nil {
		t.Fatalf("get() error = %v", err)
	}
	if updated.TotalTraffic != 123456 || updated.PanelVersion != "2.0.22" || updated.LineCount != 3 || updated.AbnormalCount != 1 {
		t.Fatalf("refresh did not save summary: %+v", updated)
	}
	if updated.LatencyMS != 987 {
		t.Fatalf("legacy latency was unexpectedly rewritten: %d", updated.LatencyMS)
	}

	list := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	list.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-session"})
	listed := httptest.NewRecorder()
	app.Handler().ServeHTTP(listed, list)
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d", listed.Code)
	}
	if strings.Contains(listed.Body.String(), "latencyMs") {
		t.Fatalf("server API still exposes latencyMs: %s", listed.Body.String())
	}
}

func TestDeleteServersRequiresBoundSingleUseSecondConfirmation(t *testing.T) {
	app, err := New(Config{
		DataDir:       t.TempDir(),
		AdminPassword: "test-password",
		MasterKey:     base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, record := range []serverRecord{{ID: "server-a", Name: "alpha"}, {ID: "server-b", Name: "beta"}} {
		if err := app.store.add(record); err != nil {
			t.Fatalf("add(%s) error = %v", record.ID, err)
		}
	}
	app.sessions["test-session"] = time.Now().Add(time.Hour)

	prepare := httptest.NewRequest(http.MethodPost, "/api/servers/delete-preparation", bytes.NewBufferString(`{"ids":["server-a","server-b"]}`))
	prepare.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-session"})
	prepared := httptest.NewRecorder()
	app.Handler().ServeHTTP(prepared, prepare)
	if prepared.Code != http.StatusOK {
		t.Fatalf("prepare status = %d, body = %s", prepared.Code, prepared.Body.String())
	}
	var response struct {
		ConfirmationID string `json:"confirmationId"`
	}
	if err := json.NewDecoder(prepared.Body).Decode(&response); err != nil {
		t.Fatalf("decode preparation = %v", err)
	}
	if response.ConfirmationID == "" {
		t.Fatal("missing confirmation ID")
	}

	body := `{"ids":["server-a","server-b"],"confirmationId":"` + response.ConfirmationID + `"}`
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/servers", bytes.NewBufferString(body))
	deleteRequest.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-session"})
	deleted := httptest.NewRecorder()
	app.Handler().ServeHTTP(deleted, deleteRequest)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", deleted.Code, deleted.Body.String())
	}
	if got := len(app.store.records()); got != 0 {
		t.Fatalf("remaining records = %d, want 0", got)
	}

	replay := httptest.NewRequest(http.MethodDelete, "/api/servers", bytes.NewBufferString(body))
	replay.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-session"})
	replayed := httptest.NewRecorder()
	app.Handler().ServeHTTP(replayed, replay)
	if replayed.Code != http.StatusForbidden {
		t.Fatalf("replay status = %d, want %d", replayed.Code, http.StatusForbidden)
	}
}
