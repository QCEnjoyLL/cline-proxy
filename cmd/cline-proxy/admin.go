package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Admin panel login. Credentials via ADMIN_USER / ADMIN_PASSWORD env vars.
// If ADMIN_PASSWORD is not set, a random password is generated and printed at startup.
var (
	adminAuthOnce sync.Once
	adminUser     string
	adminPass     string
)

// In-memory session store: token -> expiry
var (
	adminSessions   = make(map[string]time.Time)
	adminSessionsMu sync.RWMutex
)

const (
	adminSessionCookieName = "cline_admin_session"
	adminSessionTTL        = 24 * time.Hour
)

func loadAdminCredentials() {
	adminAuthOnce.Do(func() {
		adminUser = os.Getenv("ADMIN_USER")
		if adminUser == "" {
			adminUser = "admin"
		}
		adminPass = os.Getenv("ADMIN_PASSWORD")
		if adminPass == "" {
			adminPass = randomPassword(8)
			log.Printf("[admin] ADMIN_PASSWORD not set; generated random password: %s", adminPass)
		}
	})
}

func randomPassword(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("admin-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func newAdminSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	token := hex.EncodeToString(b)
	adminSessionsMu.Lock()
	adminSessions[token] = time.Now().Add(adminSessionTTL)
	adminSessionsMu.Unlock()
	return token
}

func validAdminSession(r *http.Request) bool {
	c, err := r.Cookie(adminSessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	adminSessionsMu.RLock()
	exp, ok := adminSessions[c.Value]
	adminSessionsMu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		adminSessionsMu.Lock()
		delete(adminSessions, c.Value)
		adminSessionsMu.Unlock()
		return false
	}
	return true
}

func clearAdminSession(r *http.Request) {
	if c, err := r.Cookie(adminSessionCookieName); err == nil {
		adminSessionsMu.Lock()
		delete(adminSessions, c.Value)
		adminSessionsMu.Unlock()
	}
}

func adminSessionCookie(r *http.Request, token string, maxAge int) *http.Cookie {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	return &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   maxAge,
	}
}

// requireAdminAuth protects all /admin/ routes with a login session and
// rejects cross-origin state-changing requests (CSRF guard).
func requireAdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/admin/") {
			next.ServeHTTP(w, r)
			return
		}

		if r.URL.Path == "/admin/login" || r.URL.Path == "/admin/logout" {
			next.ServeHTTP(w, r)
			return
		}

		if !validAdminSession(r) {
			if strings.HasPrefix(r.URL.Path, "/admin/api/") {
				writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "unauthorized"})
				return
			}
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				if u, err := url.Parse(origin); err == nil && !strings.EqualFold(u.Host, r.Host) {
					writeAPI(w, http.StatusForbidden, apiResponse{Error: "cross-origin request rejected"})
					return
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

// GET /admin/login serves the login page; POST /admin/login validates credentials.
func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(adminLoginHTML))
		return
	}
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid request"})
		return
	}

	loadAdminCredentials()
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(adminUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(adminPass)) == 1
	if !userOK || !passOK {
		writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "用户名或密码错误"})
		return
	}

	token := newAdminSession()
	if token == "" {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "failed to create session"})
		return
	}
	http.SetCookie(w, adminSessionCookie(r, token, int(adminSessionTTL.Seconds())))
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

// GET /admin/logout clears the session and redirects to the login page.
func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	clearAdminSession(r)
	http.SetCookie(w, adminSessionCookie(r, "", -1))
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

// In-memory OAuth login state for async browser login
var (
	oauthSessions   = make(map[string]*oauthSessionState)
	oauthSessionsMu sync.Mutex
)

type oauthSessionState struct {
	DeviceCode string
	UserCode   string
	AuthURL    string
	CreatedAt  time.Time
	Done       bool
	Success    bool
	Email      string
	Error      string
}

type apiResponse struct {
	Success bool        `json:"success"`
	Data    any         `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
}

type accountTransfer struct {
	RefreshToken string `json:"refreshToken"`
	Email        string `json:"email"`
}

func writeAPI(w http.ResponseWriter, status int, resp apiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/", adminStaticHandler)
	mux.HandleFunc("/admin/login", corsHandler(handleAdminLogin))
	mux.HandleFunc("/admin/logout", corsHandler(handleAdminLogout))
	mux.HandleFunc("/admin/api/accounts", corsHandler(handleAdminAccounts))
	mux.HandleFunc("/admin/api/accounts/export", corsHandler(handleAdminAccountExport))
	mux.HandleFunc("/admin/api/accounts/add", corsHandler(handleAdminAccountAdd))
	mux.HandleFunc("/admin/api/accounts/delete", corsHandler(handleAdminAccountDelete))
	mux.HandleFunc("/admin/api/oauth/start", corsHandler(handleOAuthStart))
	mux.HandleFunc("/admin/api/oauth/status", corsHandler(handleOAuthStatus))
	mux.HandleFunc("/admin/api/sso/import", corsHandler(handleSSOImport))
	mux.HandleFunc("/admin/api/stats", corsHandler(handleAdminStats))
	mux.HandleFunc("/admin/api/batch-import", corsHandler(handleBatchImport))
	mux.HandleFunc("/admin/api/accounts/refresh-all", corsHandler(handleAdminRefreshAll))
	mux.HandleFunc("/admin/api/accounts/delete-all", corsHandler(handleAdminDeleteAll))
	mux.HandleFunc("/admin/api/accounts/reset", corsHandler(handleAdminAccountReset))
	mux.HandleFunc("/admin/api/keys", corsHandler(handleAdminGetKeys))
	mux.HandleFunc("/admin/api/keys/generate", corsHandler(handleAdminGenerateKey))
	mux.HandleFunc("/admin/api/keys/delete", corsHandler(handleAdminDeleteKey))
	mux.HandleFunc("/admin/api/models", corsHandler(handleAdminModels))
	mux.HandleFunc("/admin/api/models/batch", corsHandler(handleAdminModelsBatchAdd))
	mux.HandleFunc("/admin/api/models/delete", corsHandler(handleAdminModelDelete))
	mux.HandleFunc("/admin/api/recommended-models", corsHandler(handleAdminRecommendedModels))
	mux.HandleFunc("/admin/api/config", corsHandler(handleAdminConfig))
	mux.HandleFunc("/admin/api/config/update", corsHandler(handleAdminUpdateConfig))
}

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(adminHTML))
		return
	}
	http.NotFound(w, r)
}

// GET /admin/api/accounts
func handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	accounts := listAccounts()
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"accounts":   accounts,
			"total":      len(accounts),
			"poolIndex":  loadPool().CurrentIdx,
		},
	})
}

// GET /admin/api/accounts/export returns data accepted by the batch import API.
func handleAdminAccountExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	p := loadPool()
	poolMu.Lock()
	accounts := make([]accountTransfer, 0, len(p.Accounts))
	for _, account := range p.Accounts {
		if account == nil {
			continue
		}
		accounts = append(accounts, accountTransfer{
			RefreshToken: account.RefreshToken,
			Email:        account.Email,
		})
	}
	poolMu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cline-accounts.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(accounts); err != nil {
		log.Printf("Failed to export accounts: %v", err)
	}
}

// POST /admin/api/accounts/add  body: { refreshToken, email }
func handleAdminAccountAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "refreshToken is required"})
		return
	}

	// Validate by refreshing
	resp, err := refreshClineToken(req.RefreshToken)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid refreshToken: " + err.Error()})
		return
	}

	if req.Email == "" {
		req.Email = fmt.Sprintf("user_%d", len(loadPool().Accounts)+1)
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        req.Email,
		RefreshToken: req.RefreshToken,
		AccessToken:  "workos:" + resp.Data.AccessToken,
		ExpiresAt:    parseExpiry(resp.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}

	addAccount(acc)
	log.Printf("Account added via API: %s", req.Email)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Account %s added", req.Email),
		Data: map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    acc.Status,
		},
	})
}

// POST /admin/api/accounts/delete  body: { accountId }
func handleAdminAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	if removeAccount(req.AccountID) {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account deleted"})
	} else {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "Account not found"})
	}
}

// POST /admin/api/oauth/start  -- Start OAuth device login, returns URL
func handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	device, err := workosDeviceAuth()
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	sessionID := fmt.Sprintf("oauth_%d", time.Now().UnixMilli())
	state := &oauthSessionState{
		DeviceCode: device.DeviceCode,
		UserCode:   device.UserCode,
		AuthURL:    authURL,
		CreatedAt:  time.Now(),
	}

	oauthSessionsMu.Lock()
	oauthSessions[sessionID] = state
	oauthSessionsMu.Unlock()

	// Start polling in background
	go func() {
		interval := device.Interval
		if interval < 5 {
			interval = 5
		}
		expiresIn := device.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 300
		}

		workosTok, err := pollWorkosToken(device.DeviceCode, interval, expiresIn)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		cline, err := registerWithCline(workosTok.AccessToken, workosTok.RefreshToken)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		email := "unknown"
		if cline.Data.UserInfo != nil && cline.Data.UserInfo.Email != "" {
			email = cline.Data.UserInfo.Email
		}

		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: cline.Data.RefreshToken,
			AccessToken:  "workos:" + cline.Data.AccessToken,
			ExpiresAt:    parseExpiry(cline.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)

		oauthSessionsMu.Lock()
		state.Done = true
		state.Success = true
		state.Email = email
		oauthSessionsMu.Unlock()
		log.Printf("OAuth account added: %s", email)
	}()

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"sessionId":       sessionID,
			"verificationUri": authURL,
			"userCode":        device.UserCode,
		},
	})
}

// GET /admin/api/oauth/status?sessionId=xxx
func handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "sessionId required"})
		return
	}

	oauthSessionsMu.Lock()
	state, ok := oauthSessions[sessionID]
	oauthSessionsMu.Unlock()

	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "session not found"})
		return
	}

	resp := map[string]any{
		"done":    state.Done,
		"success": state.Success,
	}
	if state.Done {
		resp["email"] = state.Email
		if !state.Success {
			resp["error"] = state.Error
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: resp})
}

// POST /admin/api/sso/import  body: { ssoCookies: string, email?: string }
func handleSSOImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		SSOCookies string `json:"ssoCookies"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.SSOCookies == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "ssoCookies is required"})
		return
	}

	// SSO cookies import - try to use WorkOS device auth (requires browser)
	// For direct SSO cookie conversion, we'd need the WorkOS session cookie
	// to exchange for tokens. This is a placeholder that accepts WorkOS session
	// cookies. In practice, users should use OAuth or direct refreshToken.
	//
	// SSO cookie format expected: workos_session=xxx or similar
	lines := strings.Split(req.SSOCookies, "\n")
	imported := 0
	errors := []string{}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Try to use the cookie as a refresh token directly (common format)
		if strings.HasPrefix(line, "workos:") || len(line) > 20 {
			token := strings.TrimPrefix(line, "workos:")
			resp, err := refreshClineToken(token)
			if err != nil {
				errors = append(errors, fmt.Sprintf("token %s...: %v", truncate(token, 16), err))
				continue
			}
			email := req.Email
			if email == "" {
				email = fmt.Sprintf("sso_user_%d", time.Now().UnixMilli())
			}

			acc := &Account{
				AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
				Email:        email,
				RefreshToken: token,
				AccessToken:  "workos:" + resp.Data.AccessToken,
				ExpiresAt:    parseExpiry(resp.Data.ExpiresAt) - 60000,
				Status:       "active",
				CreatedAt:    time.Now(),
			}
			addAccount(acc)
			imported++
		}
	}

	result := map[string]any{
		"imported": imported,
		"failed":   len(errors),
	}
	if len(errors) > 0 {
		result["errors"] = errors
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data:    result,
	})
}

// POST /admin/api/batch-import  body: { tokens: [{ refreshToken, email }] }
func handleBatchImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Tokens []accountTransfer `json:"tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if len(req.Tokens) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "tokens array is empty"})
		return
	}

	imported := 0
	errors := []string{}

	for _, t := range req.Tokens {
		if t.RefreshToken == "" {
			continue
		}
		resp, err := refreshClineToken(t.RefreshToken)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", t.Email, err))
			continue
		}
		email := t.Email
		if email == "" {
			email = fmt.Sprintf("batch_%d", time.Now().UnixMilli())
		}
		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: t.RefreshToken,
			AccessToken:  "workos:" + resp.Data.AccessToken,
			ExpiresAt:    parseExpiry(resp.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)
		imported++
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data: map[string]any{
			"imported": imported,
			"failed":   len(errors),
			"errors":   errors,
		},
	})
}

// POST /admin/api/accounts/refresh-all
func handleAdminRefreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for _, a := range p.Accounts {
		if err := refreshAccountToken(a); err != nil {
			log.Printf("Refresh failed for %s: %v", a.Email, err)
		}
	}
	poolMu.Unlock()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All tokens refreshed"})
}

// POST /admin/api/accounts/delete-all
func handleAdminDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All accounts deleted"})
}

// POST /admin/api/accounts/reset  body: { accountId }
func handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	// Reset status to active, clear both usage counters, and refresh token
	acc.Status = "active"
	acc.UsageCount = 0
	acc.DailyUsageCount = 0
	acc.DailyUsageDate = ""
	if err := refreshAccountToken(acc); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "reset failed: " + err.Error()})
		return
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account reset"})
}

// Global proxy config (mutable via API)
var (
	proxyConfig   = defaultProxyConfig()
	proxyConfigMu sync.Mutex
)

type proxyConfigData struct {
	Strategy string            `json:"strategy"`
	Headers  map[string]string `json:"headers"`
}

func defaultProxyConfig() *proxyConfigData {
	return &proxyConfigData{
		Strategy: "round_robin",
		Headers: map[string]string{
			"User-Agent":         "Cline/3.0.47",
			"HTTP-Referer":       "https://cline.bot",
			"X-Title":            "Cline",
			"X-IS-MULTIROOT":     "false",
			"X-CLIENT-TYPE":      "cline-cli",
			"X-CLIENT-VERSION":   "3.0.47",
			"X-PLATFORM":         "terminal",
			"X-PLATFORM-VERSION": "3.0.47",
			"X-CORE-VERSION":     "0.0.66",
		},
	}
}

func getProxyConfig() *proxyConfigData {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	return proxyConfig
}

func setProxyConfig(c *proxyConfigData) {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	proxyConfig = c
}

// GET /admin/api/keys
func handleAdminGetKeys(w http.ResponseWriter, r *http.Request) {
	p := loadPool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": p.Keys}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	key := fmt.Sprintf("cline_%x_%x", time.Now().UnixMilli(), time.Now().UnixNano()%1000000)
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"key": key}})
}

// POST /admin/api/keys/delete  body: { key }
func handleAdminDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for i, k := range p.Keys {
		if k == req.Key {
			p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
			break
		}
	}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Key deleted"})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg := getProxyConfig()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address":      "127.0.0.1:3457",
		"strategy":     cfg.Strategy,
		"version":      "go-1.1",
		"poolPath":     poolPath,
		"defaultModel": getDefaultModel(),
		"headers":      cfg.Headers,
	}})
}

// POST /admin/api/config  body: { strategy?, headers? }
func handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Strategy     string            `json:"strategy"`
		Headers      map[string]string `json:"headers"`
		DefaultModel *string           `json:"defaultModel"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid strategy, must be: round_robin, fill, random"})
			return
		}
	}

	if req.DefaultModel != nil {
		if err := setDefaultModel(*req.DefaultModel); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errModelStorage) {
				status = http.StatusInternalServerError
			}
			writeAPI(w, status, apiResponse{Error: err.Error()})
			return
		}
	}

	cfg := getProxyConfig()
	changed := false
	if req.Strategy != "" {
		cfg.Strategy = req.Strategy
		changed = true
	}

	if req.Headers != nil {
		for k, v := range req.Headers {
			cfg.Headers[k] = v
		}
		changed = true
	}

	if changed {
		setProxyConfig(cfg)
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":     cfg.Strategy,
		"headers":      cfg.Headers,
		"defaultModel": getDefaultModel(),
	}})
}

// GET /admin/api/models lists default and custom models.
// POST /admin/api/models adds a custom model with body: { id }.
func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"models": allModels()}})
	case http.MethodPost:
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
			return
		}
		id, err := addCustomModel(req.ID)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errModelStorage) {
				status = http.StatusInternalServerError
			} else if errors.Is(err, errModelExists) {
				status = http.StatusConflict
			}
			writeAPI(w, status, apiResponse{Error: err.Error()})
			return
		}
		writeAPI(w, http.StatusCreated, apiResponse{Success: true, Data: map[string]any{"id": id}})
	default:
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
	}
}

// POST /admin/api/models/delete deletes a custom model with body: { id }.
func handleAdminModelDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if err := deleteCustomModel(req.ID); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errModelStorage) {
			status = http.StatusInternalServerError
		} else if errors.Is(err, errModelNotFound) {
			status = http.StatusNotFound
		}
		writeAPI(w, status, apiResponse{Error: err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

// GET /admin/api/stats
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	p := loadPool()
	active, cooldown, expired := 0, 0, 0
	for _, a := range p.Accounts {
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "expired":
			expired++
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":    len(p.Accounts),
			"active":   active,
			"cooldown": cooldown,
			"expired":  expired,
			"strategy": "round_robin",
			"version":  "go-1.1",
		},
	})
}
