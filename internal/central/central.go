package central

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFiles embed.FS

const (
	apiPrefix        = "/api"
	sessionCookie    = "relay_central_session"
	sessionLifetime  = 12 * time.Hour
	probeTimeout     = 8 * time.Second
	backgroundPeriod = 30 * time.Second
)

type Config struct {
	DataDir           string
	AdminPassword     string
	MasterKey         string
	AllowPrivateNodes bool
}

type App struct {
	store             *store
	adminPassword     string
	allowPrivateNodes bool
	sessions          map[string]time.Time
	sessionMu         sync.Mutex
	httpClient        *http.Client
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
	ValidFrom     string    `json:"validFrom"`
	ValidUntil    string    `json:"validUntil"`
	Status        string    `json:"status"`
	LatencyMS     int64     `json:"latencyMs"`
	TotalTraffic  int64     `json:"totalTraffic"`
	PanelVersion  string    `json:"panelVersion"`
	XrayState     string    `json:"xrayState"`
	LineCount     int       `json:"lineCount"`
	AbnormalCount int       `json:"abnormalCount"`
	LastError     string    `json:"lastError"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
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

type remoteLine struct {
	ID                     int    `json:"id"`
	Name                   string `json:"name"`
	Type                   string `json:"type"`
	Status                 string `json:"status"`
	LastError              string `json:"lastError"`
	ValidFrom              int64  `json:"validFrom"`
	ValidUntil             int64  `json:"validUntil"`
	ManualReenableRequired bool   `json:"manualReenableRequired"`
	TotalTraffic           int64  `json:"totalTraffic"`
	InboundLatencyMS       int64  `json:"inboundLatencyMs"`
	OutboundLatencyMS      int64  `json:"outboundLatencyMs"`
	LastCheckedAt          int64  `json:"lastCheckedAt"`
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
		store:             s,
		adminPassword:     cfg.AdminPassword,
		allowPrivateNodes: cfg.AllowPrivateNodes,
		sessions:          make(map[string]time.Time),
		httpClient: &http.Client{
			Timeout: probeTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
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
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.requireAuth(a.logout))
	mux.HandleFunc("GET /api/session", a.requireAuth(a.session))
	mux.HandleFunc("GET /api/servers", a.requireAuth(a.listServers))
	mux.HandleFunc("POST /api/servers", a.requireAuth(a.createServer))
	mux.HandleFunc("PATCH /api/servers/{id}", a.requireAuth(a.updateServer))
	mux.HandleFunc("POST /api/servers/{id}/probe", a.requireAuth(a.probeServer))
	mux.HandleFunc("GET /api/servers/{id}/lines", a.requireAuth(a.listRemoteLines))
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
	if r.URL.Path != "/" && r.URL.Path != "/index.html" && !isServerPanelPath(r.URL.Path) {
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
	_, _ = w.Write([]byte(strings.ReplaceAll(string(raw), "{{NONCE}}", nonce)))
}

func isServerPanelPath(value string) bool {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	return len(parts) == 3 && parts[0] == "servers" && parts[1] != "" && parts[2] == "panel"
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
	record := serverRecord{ID: id, Name: strings.TrimSpace(input.Name), Address: strings.TrimSpace(input.Address), Scheme: input.Scheme, Port: input.Port, BasePath: cleanBasePath(input.BasePath), ValidFrom: input.ValidFrom, ValidUntil: input.ValidUntil, TokenCiphertext: ciphertext, Status: "unknown", CreatedAt: now, UpdatedAt: now}
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

func (a *App) probeServer(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid request origin")
		return
	}
	id := r.PathValue("id")
	record, err := a.store.get(id)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load server")
		return
	}
	updated := a.probe(record)
	if err := a.store.replace(updated); err != nil {
		writeError(w, http.StatusInternalServerError, "save probe result")
		return
	}
	level := "info"
	message := "总站检测正常"
	if updated.Status == "red" {
		level = "error"
		message = updated.LastError
	}
	a.store.addEvent(updated, "probe", level, message)
	writeJSON(w, http.StatusOK, map[string]any{"server": updated.toView()})
}

func (a *App) listRemoteLines(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.get(r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load server")
		return
	}
	lines, err := a.fetchRemoteLines(record)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": record.toView(), "lines": lines})
}

func (a *App) listEvents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"events": a.store.events()})
}

func (a *App) heartbeatLoop() {
	ticker := time.NewTicker(backgroundPeriod)
	defer ticker.Stop()
	for range ticker.C {
		for _, record := range a.store.records() {
			updated := a.probe(record)
			_ = a.store.replace(updated)
		}
	}
}

func (a *App) probe(record serverRecord) serverRecord {
	started := time.Now()
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
	record.LatencyMS = time.Since(started).Milliseconds()
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

func (a *App) fetchRemoteLines(record serverRecord) ([]map[string]any, error) {
	token, err := a.store.decrypt(record.TokenCiphertext)
	if err != nil || token == "" {
		return nil, errors.New("无法读取子站 API 凭据")
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	var lines []remoteLine
	if _, err := a.request(ctx, record, token, "central/lines", &lines); err != nil {
		return nil, fmt.Errorf("读取子站线路失败：%w", err)
	}
	result := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		result = append(result, map[string]any{
			"id": line.ID, "name": line.Name, "type": line.Type, "status": line.Status,
			"lastError": line.LastError,
			"validFrom": line.ValidFrom, "validUntil": line.ValidUntil, "manualReenableRequired": line.ManualReenableRequired,
			"totalTraffic": line.TotalTraffic, "inboundLatencyMs": line.InboundLatencyMS,
			"outboundLatencyMs": line.OutboundLatencyMS, "lastCheckedAt": line.LastCheckedAt,
		})
	}
	return result, nil
}

func failedProbe(record serverRecord, reason string) serverRecord {
	record.Status = "red"
	record.LatencyMS = 0
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
	u.Path = path.Join(cleanBasePath(record.BasePath), "panel", "api", endpoint)
	return u.String(), nil
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
	if input.Scheme != "https" {
		return errors.New("子站管理连接必须使用 HTTPS")
	}
	if input.Port < 1 || input.Port > 65535 {
		return errors.New("端口必须在 1 到 65535 之间")
	}
	if input.ValidFrom == "" {
		return errors.New("请填写服务器有效期起始日期")
	}
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

func cleanBasePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return ""
	}
	return "/" + strings.Trim(value, "/")
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
		record.Name, record.Address, record.Scheme, record.Port = strings.TrimSpace(input.Name), strings.TrimSpace(input.Address), input.Scheme, input.Port
		record.BasePath, record.ValidFrom, record.ValidUntil = cleanBasePath(input.BasePath), input.ValidFrom, input.ValidUntil
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

func (r serverRecord) toView() serverView {
	return serverView{ID: r.ID, Name: r.Name, Address: r.Address, Scheme: r.Scheme, Port: r.Port, BasePath: r.BasePath, ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil, Status: r.Status, LatencyMS: r.LatencyMS, TotalTraffic: r.TotalTraffic, PanelVersion: r.PanelVersion, XrayState: r.XrayState, LineCount: r.LineCount, AbnormalCount: r.AbnormalCount, LastError: r.LastError, LastHeartbeat: r.LastHeartbeat}
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
