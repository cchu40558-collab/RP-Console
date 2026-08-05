package central

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"relay-central/internal/config"
)

//go:embed static/*
var staticFiles embed.FS

const (
	apiPrefix        = "/api"
	sessionCookie    = "relay_central_session"
	sessionLifetime  = 12 * time.Hour
	probeTimeout     = 8 * time.Second
	backgroundPeriod = 30 * time.Second
	siteApplyTimeout = 5 * time.Minute
)

var Version = config.Version

type Config struct {
	DataDir           string
	AdminPassword     string
	MasterKey         string
	AllowPrivateNodes bool
	PrivilegedApply   string
}

type App struct {
	store              *store
	adminPassword      string
	allowPrivateNodes  bool
	sessions           map[string]time.Time
	sessionMu          sync.Mutex
	httpClient         *http.Client
	dataDir            string
	privilegedApply    string
	siteApplyMu        sync.Mutex
	siteApplyRunning   bool
	runSiteApply       func(context.Context, string) ([]byte, error)
	serverStateMu      sync.Mutex
	refreshMu          sync.RWMutex
	refresh            *refreshOperation
	deleteMu           sync.Mutex
	deletePreparations map[string]deletePreparation
}

type serverRecord struct {
	ID              string    `json:"id"`
	NodeID          string    `json:"nodeId"`
	Name            string    `json:"name"`
	Address         string    `json:"address"`
	Scheme          string    `json:"scheme"`
	Port            int       `json:"port"`
	BasePath        string    `json:"basePath"`
	ValidFrom       string    `json:"validFrom"`
	ValidUntil      string    `json:"validUntil"`
	TokenCiphertext string    `json:"tokenCiphertext"`
	Status          string    `json:"status"`
	LatencyMS       int64     `json:"latencyMs"`
	TotalTraffic    int64     `json:"totalTraffic"`
	PanelVersion    string    `json:"panelVersion"`
	XrayState       string    `json:"xrayState"`
	LineCount       int       `json:"lineCount"`
	AbnormalCount   int       `json:"abnormalCount"`
	LastError       string    `json:"lastError"`
	LastHeartbeat   time.Time `json:"lastHeartbeat"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type serverView struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Address       string    `json:"address"`
	Scheme        string    `json:"scheme"`
	Port          int       `json:"port"`
	BasePath      string    `json:"basePath"`
	PanelURL      string    `json:"panelUrl,omitempty"`
	ValidFrom     string    `json:"validFrom"`
	ValidUntil    string    `json:"validUntil"`
	Status        string    `json:"status"`
	TotalTraffic  int64     `json:"totalTraffic"`
	PanelVersion  string    `json:"panelVersion"`
	XrayState     string    `json:"xrayState"`
	LineCount     int       `json:"lineCount"`
	AbnormalCount int       `json:"abnormalCount"`
	LastError     string    `json:"lastError"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
}

type refreshOperation struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	TotalServers     int       `json:"totalServers"`
	CompletedServers int       `json:"completedServers"`
	FailedServers    int       `json:"failedServers"`
	CreatedAt        time.Time `json:"createdAt"`
	StartedAt        time.Time `json:"startedAt"`
	FinishedAt       time.Time `json:"finishedAt"`
}

type deleteRequest struct {
	IDs            []string `json:"ids"`
	ConfirmationID string   `json:"confirmationId"`
}

type deletePreparation struct {
	ID        string
	SessionID string
	ServerIDs []string
	ExpiresAt time.Time
}

type event struct {
	ID        string    `json:"id"`
	ServerID  string    `json:"serverId"`
	Server    string    `json:"server"`
	Action    string    `json:"action"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

type persistedState struct {
	Servers []serverRecord `json:"servers"`
	Events  []event        `json:"events"`
	Site    siteConfig     `json:"site"`
	SiteJob siteApplyJob   `json:"siteJob"`
}

// siteConfig deliberately contains no private-key material. The certificate
// and its key are staged as fixed files under the application's data directory.
type siteConfig struct {
	Domain              string    `json:"domain"`
	CertificateSHA256   string    `json:"certificateSha256"`
	CertificateNotAfter time.Time `json:"certificateNotAfter"`
	AppliedAt           time.Time `json:"appliedAt"`
}

type siteView struct {
	Domain            string           `json:"domain"`
	CertificateSHA256 string           `json:"certificateSha256"`
	CertificateExpiry time.Time        `json:"certificateExpiry"`
	AppliedAt         time.Time        `json:"appliedAt"`
	CanApply          bool             `json:"canApply"`
	Job               siteApplyJobView `json:"job"`
}

type siteApplyJob struct {
	ID         string    `json:"id"`
	Domain     string    `json:"domain"`
	Status     string    `json:"status"`
	Stage      string    `json:"stage"`
	Message    string    `json:"message"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

type siteApplyJobView struct {
	ID         string    `json:"id"`
	Domain     string    `json:"domain"`
	Status     string    `json:"status"`
	Stage      string    `json:"stage"`
	Message    string    `json:"message"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

type privilegedSiteRequest struct {
	Domain string `json:"domain"`
}

type store struct {
	mu    sync.RWMutex
	path  string
	block cipher.Block
	state persistedState
}

type envelope struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

type serverInput struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	Scheme     string `json:"scheme"`
	Port       int    `json:"port"`
	BasePath   string `json:"basePath"`
	ValidFrom  string `json:"validFrom"`
	ValidUntil string `json:"validUntil"`
	APIToken   string `json:"apiToken"`
}

type remoteCapabilities struct {
	Product                string   `json:"product"`
	Role                   string   `json:"role"`
	CentralProtocolVersion int      `json:"centralProtocolVersion"`
	PanelVersion           string   `json:"panelVersion"`
	NodeID                 string   `json:"nodeId"`
	ReadOnly               bool     `json:"readOnly"`
	Features               []string `json:"features"`
}

type remoteSummary struct {
	CentralProtocolVersion int    `json:"centralProtocolVersion"`
	NodeID                 string `json:"nodeId"`
	PanelVersion           string `json:"panelVersion"`
	Xray                   struct {
		State string `json:"state"`
		Error string `json:"error"`
	} `json:"xray"`
	Lines struct {
		Total    int `json:"total"`
		Healthy  int `json:"healthy"`
		Abnormal int `json:"abnormal"`
		Expired  int `json:"expired"`
	} `json:"lines"`
	Traffic struct {
		TotalBytes int64 `json:"totalBytes"`
	} `json:"traffic"`
}

func New(cfg Config) (*App, error) {
	if cfg.AdminPassword == "" {
		return nil, errors.New("CENTRAL_ADMIN_PASSWORD is required")
	}
	block, err := newBlock(cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	s, err := loadStore(filepath.Join(cfg.DataDir, "central-state.json"), block)
	if err != nil {
		return nil, err
	}
	a := &App{
		store:              s,
		adminPassword:      cfg.AdminPassword,
		allowPrivateNodes:  cfg.AllowPrivateNodes,
		dataDir:            cfg.DataDir,
		privilegedApply:    strings.TrimSpace(cfg.PrivilegedApply),
		runSiteApply:       runPrivilegedSiteApply,
		sessions:           make(map[string]time.Time),
		deletePreparations: make(map[string]deletePreparation),
		httpClient: &http.Client{
			Timeout: probeTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	if s.recoverInterruptedSiteApply() {
		log.Printf("RP Console: a pending site apply job was marked failed after restart")
	}
	go a.heartbeatLoop()
	return a, nil
}

func newBlock(value string) (cipher.Block, error) {
	key, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		key, err = base64.StdEncoding.DecodeString(value)
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("CENTRAL_MASTER_KEY must be a base64-encoded 32-byte key")
	}
	return aes.NewCipher(key)
}

func loadStore(file string, block cipher.Block) (*store, error) {
	s := &store{path: file, block: block}
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read central state: %w", err)
	}
	if err := json.Unmarshal(raw, &s.state); err != nil {
		return nil, fmt.Errorf("decode central state: %w", err)
	}
	return s, nil
}

func (s *store) saveLocked() error {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *store) encrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	gcm, err := cipher.NewGCM(s.block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(value), nil)
	return base64.RawStdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func (s *store) decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	gcm, err := cipher.NewGCM(s.block)
	if err != nil {
		return "", err
	}
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(raw) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("decrypt node credential")
	}
	return string(plain), nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
	})
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.requireAuth(a.logout))
	mux.HandleFunc("GET /api/session", a.requireAuth(a.session))
	mux.HandleFunc("GET /api/site", a.requireAuth(a.getSite))
	mux.HandleFunc("POST /api/site/apply", a.requireAuth(a.applySite))
	mux.HandleFunc("GET /api/servers", a.requireAuth(a.listServers))
	mux.HandleFunc("POST /api/servers", a.requireAuth(a.createServer))
	mux.HandleFunc("PATCH /api/servers/{id}", a.requireAuth(a.updateServer))
	mux.HandleFunc("POST /api/servers/delete-preparation", a.requireAuth(a.prepareDeleteServers))
	mux.HandleFunc("DELETE /api/servers", a.requireAuth(a.deleteServers))
	mux.HandleFunc("POST /api/operations/refresh", a.requireAuth(a.startRefresh))
	mux.HandleFunc("GET /api/operations/current", a.requireAuth(a.currentRefresh))
	mux.HandleFunc("GET /api/events", a.requireAuth(a.listEvents))
	mux.HandleFunc("/", a.static)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *App) static(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	raw, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "static page unavailable", http.StatusInternalServerError)
		return
	}
	nonce, err := randomID(16)
	if err != nil {
		http.Error(w, "create page nonce", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; style-src 'nonce-"+nonce+"'; script-src 'nonce-"+nonce+"'")
	page := strings.ReplaceAll(string(raw), "{{NONCE}}", nonce)
	_, _ = w.Write([]byte(strings.ReplaceAll(page, "{{VERSION}}", Version)))
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Password == "" {
		writeError(w, http.StatusBadRequest, "password is required")
		return
	}
	if !hmac.Equal([]byte(body.Password), []byte(a.adminPassword)) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := randomID(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create session")
		return
	}
	expires := time.Now().Add(sessionLifetime)
	a.sessionMu.Lock()
	a.sessions[token] = expires
	a.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: requestScheme(r) == "https", Expires: expires})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		a.sessionMu.Lock()
		delete(a.sessions, cookie.Value)
		a.sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: requestScheme(r) == "https"})
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) session(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (a *App) getSite(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.store.siteView(a.privilegedApply != ""))
}

func (a *App) applySite(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	if a.privilegedApply == "" {
		writeError(w, http.StatusServiceUnavailable, "本站未安装受限的站点配置助手，请先升级 RP Console")
		return
	}
	// The helper consumes fixed staging names. Serialize the complete upload and
	// apply flow so two authenticated browser sessions cannot mix certificate files.
	a.siteApplyMu.Lock()
	defer a.siteApplyMu.Unlock()
	if a.siteApplyRunning || a.store.siteApplyActive() {
		writeError(w, http.StatusConflict, "A site configuration task is already running")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "证书文件过大或表单格式不正确")
		return
	}
	domain, err := validateSiteDomain(r.FormValue("domain"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	certFile, _, err := r.FormFile("certificate")
	if err != nil {
		writeError(w, http.StatusBadRequest, "请选择 Origin Certificate 证书文件")
		return
	}
	defer certFile.Close()
	keyFile, _, err := r.FormFile("privateKey")
	if err != nil {
		writeError(w, http.StatusBadRequest, "请选择 Origin Certificate 私钥文件")
		return
	}
	defer keyFile.Close()
	certPEM, err := io.ReadAll(io.LimitReader(certFile, 768<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取证书文件失败")
		return
	}
	keyPEM, err := io.ReadAll(io.LimitReader(keyFile, 768<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取私钥文件失败")
		return
	}
	leaf, err := validateSiteTLS(domain, certPEM, keyPEM)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.stageSiteFiles(domain, certPEM, keyPEM); err != nil {
		writeError(w, http.StatusInternalServerError, "保存站点证书失败")
		return
	}
	jobID, err := randomID(16)
	if err != nil {
		a.clearSiteStaging()
		writeError(w, http.StatusInternalServerError, "could not create site apply job")
		return
	}
	job := siteApplyJob{ID: jobID, Domain: domain, Status: "queued", Stage: "queued", Message: "Waiting to apply site configuration", CreatedAt: time.Now().UTC()}
	if err := a.store.queueSiteApply(job); err != nil {
		a.clearSiteStaging()
		writeError(w, http.StatusConflict, "A site configuration task is already running")
		return
	}
	a.siteApplyRunning = true
	go a.completeSiteApply(job, leaf)
	writeJSON(w, http.StatusAccepted, a.store.siteView(true))
}

func runPrivilegedSiteApply(ctx context.Context, helper string) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "--", helper).CombinedOutput()
}

func (a *App) completeSiteApply(job siteApplyJob, leaf *x509.Certificate) {
	_ = a.store.updateSiteApply(job.ID, "running", "starting", "Checking firewall and site configuration")
	ctx, cancel := context.WithTimeout(context.Background(), siteApplyTimeout)
	defer cancel()
	output, err := a.runSiteApply(ctx, a.privilegedApply)
	a.clearSiteStaging()
	if err != nil {
		message := "Site apply failed; the previous certificate and Nginx configuration were retained"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			message = "Site apply exceeded five minutes; the previous certificate and Nginx configuration were retained"
		}
		log.Printf("RP Console site apply %s failed: %s", job.ID, helperOutputSummary(output))
		_ = a.store.finishSiteApply(job.ID, "failed", "failed", message, nil)
		a.store.addEvent(serverRecord{Name: "RP Console"}, "site-apply-failed", "error", message)
	} else {
		site := siteConfig{Domain: job.Domain, CertificateSHA256: certificateDigest(leaf.Raw), CertificateNotAfter: leaf.NotAfter.UTC(), AppliedAt: time.Now().UTC()}
		if err := a.store.finishSiteApply(job.ID, "succeeded", "complete", "Certificate, Nginx, and firewall checks completed", &site); err != nil {
			log.Printf("RP Console site apply %s completed but state persistence failed: %v", job.ID, err)
		} else {
			a.store.addEvent(serverRecord{Name: "RP Console"}, "site-applied", "info", "Site certificate, Nginx configuration, and UFW rules applied")
		}
	}
	a.siteApplyMu.Lock()
	a.siteApplyRunning = false
	a.siteApplyMu.Unlock()
}

func helperOutputSummary(output []byte) string {
	value := strings.TrimSpace(strings.ReplaceAll(string(output), "\n", " | "))
	if value == "" {
		return "helper returned no diagnostic output"
	}
	if len(value) > 500 {
		return value[:500] + "..."
	}
	return value
}

func validateSiteDomain(value string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(value))
	if len(domain) > 253 || !strings.Contains(domain, ".") {
		return "", errors.New("请输入完整的站点域名")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("站点域名格式不正确")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return "", errors.New("站点域名只能包含小写字母、数字、连字符和点")
			}
		}
	}
	return domain, nil
}

func validateSiteTLS(domain string, certPEM, keyPEM []byte) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) == 0 {
		return nil, errors.New("证书或私钥不是有效的 PEM，或二者不匹配")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, errors.New("无法读取证书")
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return nil, errors.New("证书不包含填写的站点域名")
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, errors.New("证书已经过期")
	}
	return leaf, nil
}

func (a *App) stageSiteFiles(domain string, certPEM, keyPEM []byte) error {
	dir := filepath.Join(a.dataDir, "site-tls")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	request, err := json.Marshal(privilegedSiteRequest{Domain: domain})
	if err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(dir, "origin.crt"), certPEM, 0644); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(dir, "origin.key"), keyPEM, 0600); err != nil {
		return err
	}
	return writePrivateFile(filepath.Join(dir, "request.json"), request, 0600)
}

func (a *App) clearSiteStaging() {
	dir := filepath.Join(a.dataDir, "site-tls")
	for _, name := range []string{"request.json", "origin.crt", "origin.key"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func writePrivateFile(filename string, data []byte, mode os.FileMode) error {
	temporary := filename + ".new"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		return err
	}
	return os.Rename(temporary, filename)
}

func certificateDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || !a.validSession(cookie.Value) {
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		next(w, r)
	}
}

func (a *App) validSession(token string) bool {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	expires, ok := a.sessions[token]
	if !ok || time.Now().After(expires) {
		delete(a.sessions, token)
		return false
	}
	return true
}

func (a *App) listServers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"servers": a.store.views(), "summary": a.store.summary()})
}

func (a *App) createServer(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	var input serverInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.APIToken == "" {
		writeError(w, http.StatusBadRequest, "子站 API Token 不能为空")
		return
	}
	if err := validateRemoteHost(input.Address, a.allowPrivateNodes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.serverStateMu.Lock()
	defer a.serverStateMu.Unlock()
	if a.refreshActive() {
		writeError(w, http.StatusConflict, "刷新进行中，暂时不能添加服务器")
		return
	}
	ciphertext, err := a.store.encrypt(input.APIToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encrypt API token")
		return
	}
	id, err := randomID(12)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create server ID")
		return
	}
	now := time.Now().UTC()
	record := serverRecord{ID: id, Name: input.Name, Address: input.Address, Scheme: input.Scheme, Port: input.Port, BasePath: input.BasePath, ValidFrom: input.ValidFrom, ValidUntil: input.ValidUntil, TokenCiphertext: ciphertext, Status: "unknown", CreatedAt: now, UpdatedAt: now}
	if err := a.store.add(record); err != nil {
		writeError(w, http.StatusInternalServerError, "save server")
		return
	}
	a.store.addEvent(record, "server-added", "info", "子站已登记，等待首次检测")
	view := record.toView()
	writeJSON(w, http.StatusCreated, map[string]any{"server": view})
}

func (a *App) updateServer(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	id := r.PathValue("id")
	var input serverInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRemoteHost(input.Address, a.allowPrivateNodes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.serverStateMu.Lock()
	defer a.serverStateMu.Unlock()
	if a.refreshActive() {
		writeError(w, http.StatusConflict, "刷新进行中，暂时不能编辑服务器")
		return
	}
	updated, err := a.store.update(id, input)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save server")
		return
	}
	a.store.addEvent(updated, "server-updated", "info", "服务器资料已更新")
	writeJSON(w, http.StatusOK, map[string]any{"server": updated.toView()})
}

func (a *App) startRefresh(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	operation, started, err := a.beginRefresh()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建刷新任务失败")
		return
	}
	if !started {
		writeJSON(w, http.StatusConflict, map[string]any{"operation": operation})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": operation})
}

func (a *App) currentRefresh(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"operation": a.refreshView()})
}

func (a *App) prepareDeleteServers(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	var request deleteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ids, err := normalizeServerIDs(request.IDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.serverStateMu.Lock()
	defer a.serverStateMu.Unlock()
	if a.refreshActive() {
		writeError(w, http.StatusConflict, "刷新进行中，暂时不能删除服务器")
		return
	}
	servers, err := a.store.getMany(ids)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "部分服务器不存在，请刷新列表后重试")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取待删除服务器失败")
		return
	}
	confirmationID, err := a.createDeletePreparation(sessionToken(r), ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建删除确认失败")
		return
	}
	items := make([]map[string]string, 0, len(servers))
	for _, server := range servers {
		items = append(items, map[string]string{"id": server.ID, "name": server.Name})
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["name"] < items[j]["name"] })
	writeJSON(w, http.StatusOK, map[string]any{"confirmationId": confirmationID, "servers": items, "expiresInSeconds": 60})
}

func (a *App) deleteServers(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	var request deleteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ids, err := normalizeServerIDs(request.IDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.ConfirmationID == "" {
		writeError(w, http.StatusBadRequest, "缺少第二次删除确认")
		return
	}
	a.serverStateMu.Lock()
	defer a.serverStateMu.Unlock()
	if a.refreshActive() {
		writeError(w, http.StatusConflict, "刷新进行中，暂时不能删除服务器")
		return
	}
	if !a.consumeDeletePreparation(request.ConfirmationID, sessionToken(r), ids) {
		writeError(w, http.StatusForbidden, "删除确认已过期或与当前选择不一致")
		return
	}
	removed, err := a.store.removeMany(ids)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "部分服务器不存在，请刷新列表后重试")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "删除服务器失败")
		return
	}
	names := make([]string, 0, len(removed))
	for _, server := range removed {
		names = append(names, server.Name)
	}
	sort.Strings(names)
	a.store.addEvent(serverRecord{Name: "RP Console"}, "servers-deleted", "info", fmt.Sprintf("已删除 %d 台服务器登记：%s", len(names), strings.Join(names, "、")))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": len(removed)})
}

func (a *App) listEvents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"events": a.store.events()})
}

func (a *App) heartbeatLoop() {
	ticker := time.NewTicker(backgroundPeriod)
	defer ticker.Stop()
	for range ticker.C {
		if a.refreshActive() {
			continue
		}
		for _, record := range a.store.records() {
			updated := a.syncServer(record)
			a.serverStateMu.Lock()
			if !a.refreshActive() {
				_ = a.store.replace(updated)
			}
			a.serverStateMu.Unlock()
		}
	}
}

func (a *App) beginRefresh() (refreshOperation, bool, error) {
	a.serverStateMu.Lock()
	defer a.serverStateMu.Unlock()
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.refresh != nil && (a.refresh.Status == "queued" || a.refresh.Status == "running") {
		return *a.refresh, false, nil
	}
	id, err := randomID(12)
	if err != nil {
		return refreshOperation{}, false, err
	}
	records := a.store.records()
	now := time.Now().UTC()
	operation := &refreshOperation{ID: id, Status: "running", TotalServers: len(records), CreatedAt: now, StartedAt: now}
	a.refresh = operation
	go a.runRefresh(id, records)
	return *operation, true, nil
}

func (a *App) runRefresh(id string, records []serverRecord) {
	if len(records) == 0 {
		a.completeRefresh(id, 0, 0)
		return
	}
	sem := make(chan struct{}, 3)
	var group sync.WaitGroup
	for _, record := range records {
		record := record
		group.Add(1)
		go func() {
			defer group.Done()
			sem <- struct{}{}
			updated := a.syncServer(record)
			<-sem
			a.serverStateMu.Lock()
			_ = a.store.replace(updated)
			a.serverStateMu.Unlock()
			a.advanceRefresh(id, updated.Status == "red")
		}()
	}
	group.Wait()
	a.completeRefreshFromProgress(id)
}

func (a *App) advanceRefresh(id string, failed bool) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.refresh == nil || a.refresh.ID != id || a.refresh.Status != "running" {
		return
	}
	a.refresh.CompletedServers++
	if failed {
		a.refresh.FailedServers++
	}
}

func (a *App) completeRefreshFromProgress(id string) {
	a.refreshMu.Lock()
	if a.refresh == nil || a.refresh.ID != id || a.refresh.Status != "running" {
		a.refreshMu.Unlock()
		return
	}
	completed, failed := a.refresh.CompletedServers, a.refresh.FailedServers
	a.refreshMu.Unlock()
	a.completeRefresh(id, completed, failed)
}

func (a *App) completeRefresh(id string, completed, failed int) {
	a.refreshMu.Lock()
	if a.refresh == nil || a.refresh.ID != id || a.refresh.Status != "running" {
		a.refreshMu.Unlock()
		return
	}
	a.refresh.CompletedServers = completed
	a.refresh.FailedServers = failed
	switch {
	case failed == 0:
		a.refresh.Status = "succeeded"
	case failed == a.refresh.TotalServers:
		a.refresh.Status = "failed"
	default:
		a.refresh.Status = "partial"
	}
	a.refresh.FinishedAt = time.Now().UTC()
	operation := *a.refresh
	a.refreshMu.Unlock()
	a.store.addEvent(serverRecord{Name: "RP Console"}, "servers-refreshed", "info", fmt.Sprintf("已同步 %d 台服务器；成功 %d 台，异常 %d 台", operation.TotalServers, operation.TotalServers-operation.FailedServers, operation.FailedServers))
}

func (a *App) refreshActive() bool {
	a.refreshMu.RLock()
	defer a.refreshMu.RUnlock()
	return a.refresh != nil && (a.refresh.Status == "queued" || a.refresh.Status == "running")
}

func (a *App) refreshView() any {
	a.refreshMu.RLock()
	defer a.refreshMu.RUnlock()
	if a.refresh == nil {
		return nil
	}
	view := *a.refresh
	return view
}

func (a *App) syncServer(record serverRecord) serverRecord {
	token, err := a.store.decrypt(record.TokenCiphertext)
	if err != nil {
		return failedProbe(record, "无法读取子站 API 凭据")
	}
	if token == "" {
		return failedProbe(record, "未配置子站 API Token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	var capabilities remoteCapabilities
	if _, err := a.request(ctx, record, token, "central/capabilities", &capabilities); err != nil {
		return failedProbe(record, fmt.Sprintf("总站无法验证子站接入协议：%v", err))
	}
	if capabilities.Product != "relay-panel" || capabilities.Role != "node" || capabilities.CentralProtocolVersion != 1 || !capabilities.ReadOnly || capabilities.NodeID == "" {
		return failedProbe(record, "子站版本不支持总站接入协议")
	}
	if record.NodeID != "" && record.NodeID != capabilities.NodeID {
		return failedProbe(record, "子站节点标识已变化，请删除后重新登记该服务器")
	}
	record.NodeID = capabilities.NodeID
	var summary remoteSummary
	if _, err := a.request(ctx, record, token, "central/summary", &summary); err != nil {
		return failedProbe(record, fmt.Sprintf("子站运行摘要读取失败：%v", err))
	}
	if summary.CentralProtocolVersion != 1 || summary.NodeID != capabilities.NodeID {
		return failedProbe(record, "子站返回的摘要协议或节点标识不一致")
	}
	record.PanelVersion = summary.PanelVersion
	record.XrayState = summary.Xray.State
	record.LineCount = summary.Lines.Total
	record.AbnormalCount = summary.Lines.Abnormal
	record.TotalTraffic = summary.Traffic.TotalBytes
	if record.XrayState != "running" {
		return failedProbe(record, "子站 Xray 未正常运行："+summary.Xray.Error)
	}
	record.LastError = ""
	record.LastHeartbeat = time.Now().UTC()
	record.Status = recordStatus(record)
	record.UpdatedAt = record.LastHeartbeat
	return record
}

func failedProbe(record serverRecord, reason string) serverRecord {
	record.Status = "red"
	record.LastError = reason
	record.LastHeartbeat = time.Now().UTC()
	record.UpdatedAt = record.LastHeartbeat
	return record
}

func recordStatus(record serverRecord) string {
	if record.XrayState != "" && record.XrayState != "running" {
		return "red"
	}
	if record.AbnormalCount > 0 {
		return "red"
	}
	if record.ValidUntil != "" {
		until, _ := time.Parse("2006-01-02", record.ValidUntil)
		if !until.IsZero() && !time.Now().After(until.AddDate(0, 0, 1)) && time.Until(until.AddDate(0, 0, 1)) <= 7*24*time.Hour {
			return "yellow"
		}
	}
	return "green"
}

func (a *App) request(ctx context.Context, record serverRecord, token, endpoint string, target any) ([]byte, error) {
	urlValue, err := nodeURL(record, endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlValue, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	response, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var msg envelope
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, errors.New("invalid node response")
	}
	if !msg.Success {
		if msg.Msg == "" {
			msg.Msg = "node rejected request"
		}
		return nil, errors.New(msg.Msg)
	}
	if target != nil && len(msg.Obj) > 0 && string(msg.Obj) != "null" {
		if err := json.Unmarshal(msg.Obj, target); err != nil {
			return nil, errors.New("invalid node payload")
		}
	}
	return msg.Obj, nil
}

func nodeURL(record serverRecord, endpoint string) (string, error) {
	host := record.Address
	if net.ParseIP(host) != nil && strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if record.Port < 1 || record.Port > 65535 {
		return "", errors.New("invalid node port")
	}
	u := url.URL{Scheme: record.Scheme, Host: net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(record.Port))}
	if strings.Contains(record.Address, ":") && net.ParseIP(record.Address) != nil {
		u.Host = net.JoinHostPort(record.Address, strconv.Itoa(record.Port))
	}
	basePath, err := normalizeBasePath(record.BasePath)
	if err != nil {
		return "", err
	}
	u.Path = path.Join(basePath, "panel", "api", endpoint)
	return u.String(), nil
}

// managementPanelURL is intentionally separate from nodeURL: it is a browser
// navigation target, while nodeURL addresses the node's read-only API.
func managementPanelURL(record serverRecord) (string, error) {
	host, err := normalizeManagementDomain(record.Address)
	if err != nil {
		return "", err
	}
	if strings.ToLower(strings.TrimSpace(record.Scheme)) != "https" {
		return "", errors.New("node management panel must use HTTPS")
	}
	if record.Port < 1 || record.Port > 65535 {
		return "", errors.New("invalid node port")
	}
	basePath, err := normalizeBasePath(record.BasePath)
	if err != nil {
		return "", err
	}
	if basePath == "" {
		basePath = "/"
	} else {
		basePath += "/"
	}
	return (&url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(host, strconv.Itoa(record.Port)),
		Path:   basePath,
	}).String(), nil
}

func validateInput(input *serverInput) error {
	input.Name = strings.TrimSpace(input.Name)
	input.Address = strings.TrimSpace(input.Address)
	input.Scheme = strings.ToLower(strings.TrimSpace(input.Scheme))
	if input.Name == "" || len(input.Name) > 80 {
		return errors.New("服务器名称必须为 1 到 80 个字符")
	}
	if input.Address == "" || len(input.Address) > 253 {
		return errors.New("请输入有效的管理地址")
	}
	address, err := normalizeManagementDomain(input.Address)
	if err != nil {
		return err
	}
	input.Address = address
	if input.Scheme != "https" {
		return errors.New("子站管理连接必须使用 HTTPS")
	}
	if input.Port < 1 || input.Port > 65535 {
		return errors.New("端口必须在 1 到 65535 之间")
	}
	if input.ValidFrom == "" {
		return errors.New("请填写服务器有效期起始日期")
	}
	basePath, err := normalizeBasePath(input.BasePath)
	if err != nil {
		return err
	}
	input.BasePath = basePath
	from, err := time.Parse("2006-01-02", input.ValidFrom)
	if err != nil {
		return errors.New("起始日期必须为 YYYY-MM-DD")
	}
	if input.ValidUntil != "" {
		until, err := time.Parse("2006-01-02", input.ValidUntil)
		if err != nil || until.Before(from) {
			return errors.New("结束日期必须不早于起始日期")
		}
	}
	if input.APIToken != "" && len(input.APIToken) > 1024 {
		return errors.New("API Token 过长")
	}
	return nil
}

func normalizeManagementDomain(value string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil {
		return "", errors.New("子站管理地址必须填写证书对应的域名，不能使用 IP")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("子站管理地址必须填写完整域名")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("请输入有效的管理域名")
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return "", errors.New("请输入有效的管理域名")
			}
		}
	}
	return host, nil
}

func validateRemoteHost(host string, allowPrivate bool) error {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return validateIP(ip, allowPrivate)
	}
	if strings.Contains(host, "/") || strings.Contains(host, "@") || strings.Contains(host, ":") {
		return errors.New("管理地址只能是 IP 或域名")
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return errors.New("无法解析管理地址")
	}
	for _, ip := range ips {
		address, ok := netip.AddrFromSlice(ip)
		if !ok {
			return errors.New("无法解析管理地址")
		}
		if err := validateIP(address, allowPrivate); err != nil {
			return err
		}
	}
	return nil
}

func validateIP(ip netip.Addr, allowPrivate bool) error {
	if !ip.IsValid() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return errors.New("管理地址不能是本机、未指定或链路本地地址")
	}
	if !allowPrivate && ip.IsPrivate() {
		return errors.New("私网子站默认被禁止；确认使用 WireGuard 后设置 CENTRAL_ALLOW_PRIVATE_NODES=true")
	}
	return nil
}

func normalizeBasePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return "", nil
	}
	if strings.ContainsAny(value, "\\\\?#%") {
		return "", errors.New("管理基础路径不能包含查询参数、片段或转义字符")
	}
	segments := strings.Split(strings.Trim(value, "/"), "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("管理基础路径格式无效")
		}
		for _, r := range segment {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-._~", r)) {
				return "", errors.New("管理基础路径只能包含字母、数字和 -._~")
			}
		}
	}
	return "/" + strings.Join(segments, "/"), nil
}

func normalizeServerIDs(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 100 {
		return nil, errors.New("请选择 1 到 100 台服务器")
	}
	ids := append([]string(nil), values...)
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || len(id) > 128 {
			return nil, errors.New("服务器选择无效")
		}
	}
	sort.Strings(ids)
	for index := 1; index < len(ids); index++ {
		if ids[index] == ids[index-1] {
			return nil, errors.New("服务器选择重复")
		}
	}
	return ids, nil
}

func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (a *App) createDeletePreparation(sessionID string, serverIDs []string) (string, error) {
	id, err := randomID(18)
	if err != nil {
		return "", err
	}
	now := time.Now()
	a.deleteMu.Lock()
	defer a.deleteMu.Unlock()
	for key, preparation := range a.deletePreparations {
		if now.After(preparation.ExpiresAt) {
			delete(a.deletePreparations, key)
		}
	}
	a.deletePreparations[id] = deletePreparation{ID: id, SessionID: sessionID, ServerIDs: append([]string(nil), serverIDs...), ExpiresAt: now.Add(time.Minute)}
	return id, nil
}

func (a *App) consumeDeletePreparation(id, sessionID string, serverIDs []string) bool {
	a.deleteMu.Lock()
	defer a.deleteMu.Unlock()
	preparation, ok := a.deletePreparations[id]
	if !ok || time.Now().After(preparation.ExpiresAt) || preparation.SessionID != sessionID || !sameStringSlice(preparation.ServerIDs, serverIDs) {
		return false
	}
	delete(a.deletePreparations, id)
	return true
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *store) add(record serverRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.state.Servers {
		if strings.EqualFold(existing.Name, record.Name) {
			return errors.New("server name already exists")
		}
	}
	s.state.Servers = append(s.state.Servers, record)
	return s.saveLocked()
}

var errNotFound = errors.New("not found")

func (s *store) get(id string) (serverRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, record := range s.state.Servers {
		if record.ID == id {
			return record, nil
		}
	}
	return serverRecord{}, errNotFound
}

func (s *store) getMany(ids []string) ([]serverRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byID := make(map[string]serverRecord, len(s.state.Servers))
	for _, record := range s.state.Servers {
		byID[record.ID] = record
	}
	items := make([]serverRecord, 0, len(ids))
	for _, id := range ids {
		record, ok := byID[id]
		if !ok {
			return nil, errNotFound
		}
		items = append(items, record)
	}
	return items, nil
}

func (s *store) update(id string, input serverInput) (serverRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Servers {
		if s.state.Servers[i].ID != id {
			continue
		}
		for _, other := range s.state.Servers {
			if other.ID != id && strings.EqualFold(other.Name, input.Name) {
				return serverRecord{}, errors.New("server name already exists")
			}
		}
		record := &s.state.Servers[i]
		record.Name, record.Address, record.Scheme, record.Port = input.Name, input.Address, input.Scheme, input.Port
		record.BasePath, record.ValidFrom, record.ValidUntil = input.BasePath, input.ValidFrom, input.ValidUntil
		if input.APIToken != "" {
			ciphertext, err := s.encrypt(input.APIToken)
			if err != nil {
				return serverRecord{}, err
			}
			record.TokenCiphertext = ciphertext
		}
		record.UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(); err != nil {
			return serverRecord{}, err
		}
		return *record, nil
	}
	return serverRecord{}, errNotFound
}

func (s *store) replace(updated serverRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Servers {
		if s.state.Servers[i].ID == updated.ID {
			s.state.Servers[i] = updated
			return s.saveLocked()
		}
	}
	return errNotFound
}

func (s *store) removeMany(ids []string) ([]serverRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	removed := make([]serverRecord, 0, len(ids))
	remaining := make([]serverRecord, 0, len(s.state.Servers)-len(ids))
	for _, record := range s.state.Servers {
		if _, ok := wanted[record.ID]; ok {
			removed = append(removed, record)
			continue
		}
		remaining = append(remaining, record)
	}
	if len(removed) != len(ids) {
		return nil, errNotFound
	}
	original := s.state.Servers
	s.state.Servers = remaining
	if err := s.saveLocked(); err != nil {
		s.state.Servers = original
		return nil, err
	}
	return removed, nil
}

func (s *store) records() []serverRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := append([]serverRecord(nil), s.state.Servers...)
	return rows
}

func (s *store) views() []serverView {
	rows := s.records()
	views := make([]serverView, 0, len(rows))
	for _, row := range rows {
		views = append(views, row.toView())
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

func (s *store) summary() map[string]int {
	result := map[string]int{"total": 0, "green": 0, "yellow": 0, "red": 0}
	for _, record := range s.records() {
		result["total"]++
		result[record.Status]++
	}
	return result
}

func (s *store) addEvent(record serverRecord, action, level, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := randomID(10)
	s.state.Events = append([]event{{ID: id, ServerID: record.ID, Server: record.Name, Action: action, Level: level, Message: message, CreatedAt: time.Now().UTC()}}, s.state.Events...)
	if len(s.state.Events) > 100 {
		s.state.Events = s.state.Events[:100]
	}
	_ = s.saveLocked()
}

func (s *store) events() []event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]event, len(s.state.Events))
	copy(items, s.state.Events)
	return items
}

func (s *store) siteView(canApply bool) siteView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job := s.state.SiteJob
	return siteView{
		Domain:            s.state.Site.Domain,
		CertificateSHA256: s.state.Site.CertificateSHA256,
		CertificateExpiry: s.state.Site.CertificateNotAfter,
		AppliedAt:         s.state.Site.AppliedAt,
		CanApply:          canApply,
		Job:               siteApplyJobView{ID: job.ID, Domain: job.Domain, Status: job.Status, Stage: job.Stage, Message: job.Message, CreatedAt: job.CreatedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt},
	}
}

func (s *store) saveSite(site siteConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Site = site
	return s.saveLocked()
}

func (s *store) siteApplyActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.SiteJob.Status == "queued" || s.state.SiteJob.Status == "running"
}

func (s *store) queueSiteApply(job siteApplyJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SiteJob.Status == "queued" || s.state.SiteJob.Status == "running" {
		return errors.New("site apply already active")
	}
	s.state.SiteJob = job
	return s.saveLocked()
}

func (s *store) updateSiteApply(id, status, stage, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SiteJob.ID != id {
		return errors.New("site apply job not found")
	}
	s.state.SiteJob.Status = status
	s.state.SiteJob.Stage = stage
	s.state.SiteJob.Message = message
	if status == "running" && s.state.SiteJob.StartedAt.IsZero() {
		s.state.SiteJob.StartedAt = time.Now().UTC()
	}
	return s.saveLocked()
}

func (s *store) finishSiteApply(id, status, stage, message string, site *siteConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SiteJob.ID != id {
		return errors.New("site apply job not found")
	}
	if site != nil {
		s.state.Site = *site
	}
	s.state.SiteJob.Status = status
	s.state.SiteJob.Stage = stage
	s.state.SiteJob.Message = message
	s.state.SiteJob.FinishedAt = time.Now().UTC()
	return s.saveLocked()
}

func (s *store) recoverInterruptedSiteApply() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SiteJob.Status != "queued" && s.state.SiteJob.Status != "running" {
		return false
	}
	s.state.SiteJob.Status = "failed"
	s.state.SiteJob.Stage = "interrupted"
	s.state.SiteJob.Message = "RP Console restarted before the previous site apply completed; verify the current certificate before retrying"
	s.state.SiteJob.FinishedAt = time.Now().UTC()
	_ = s.saveLocked()
	return true
}

func (r serverRecord) toView() serverView {
	panelURL, _ := managementPanelURL(r)
	return serverView{ID: r.ID, Name: r.Name, Address: r.Address, Scheme: r.Scheme, Port: r.Port, BasePath: r.BasePath, PanelURL: panelURL, ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil, Status: r.Status, TotalTraffic: r.TotalTraffic, PanelVersion: r.PanelVersion, XrayState: r.XrayState, LineCount: r.LineCount, AbnormalCount: r.AbnormalCount, LastError: r.LastError, LastHeartbeat: r.LastHeartbeat}
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("请求数据格式不正确")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func randomID(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host) && u.Scheme == requestScheme(r)
}

func requestScheme(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-Proto"); strings.EqualFold(forwarded, "https") {
		return "https"
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func signedDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
