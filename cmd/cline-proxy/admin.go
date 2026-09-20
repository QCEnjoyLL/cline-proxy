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

// newAPIKey 生成代理侧 API Key。
//
// 必须是 crypto/rand：代理 Key 是暴露在公网上的唯一凭据，一旦可预测就等于没有
// 鉴权。旧实现用 UnixMilli + UnixNano%1e6 拼出来，熵只有约 20 bit 且完全由时间
// 决定，攻击者按已知的生成时刻枚举即可伪造。
//
// 与随机密码不同的是，这里不静默退化成基于时间的值——生成不出安全 Key 就该让
// 调用方报错，而不是发出一个看似正常、实则可预测的密钥。
func newAPIKey() (string, error) {
	b := make([]byte, apiKeyRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return apiKeyPrefix + hex.EncodeToString(b), nil
}

const (
	// apiKeyPrefix 便于在配置里一眼认出这是本代理发出的 Key。
	apiKeyPrefix = "cline_"
	// 24 字节 = 192 bit 熵，hex 后 48 字符，长度也不易被误截断。
	apiKeyRandomBytes = 24
)

func newAdminSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	token := hex.EncodeToString(b)
	adminSessionsMu.Lock()
	for session, expiry := range adminSessions {
		if time.Now().After(expiry) {
			delete(adminSessions, session)
		}
	}
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
		w.Write([]byte(renderPage(adminLoginHTML)))
		return
	}
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	loginIP := remoteLoginIP(r)
	if !adminLoginAttempts.allow(loginIP, time.Now()) {
		w.Header().Set("Retry-After", "300")
		writeAPI(w, http.StatusTooManyRequests, apiResponse{Error: "登录尝试过于频繁，请稍后重试"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, bodyTooLargeStatus(err), apiResponse{Error: "invalid or oversized login request"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
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
	adminLoginAttempts.clear(loginIP)

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

// oauthSessionTTL 是 OAuth 会话在内存里保留的时长。
//
// 为什么需要它：oauthSessions 只在 handleOAuthStart 里插入、此前从不删除，
// 而每条 oauthSessionState 都带着 device code。反复点「开始 OAuth 登录」会让
// 这个 map 单调增长（每条还对应一个最多存活约 5 分钟的后台 goroutine）。
// 设备码流程本身约 5 分钟过期，前端也在 Done 之后停止轮询，所以 15 分钟足够
// 覆盖「最后一次状态查询」，更久的记录可以安全丢弃。
const oauthSessionTTL = 15 * time.Minute

// pruneOAuthSessionsLocked 清掉已过期的 OAuth 会话，返回清理条数。
// 调用方必须已持有 oauthSessionsMu。CreatedAt 为零值的记录（理论上不该出现）
// 也一并清掉，避免它们永远留下。
func pruneOAuthSessionsLocked(now time.Time) int {
	removed := 0
	for id, st := range oauthSessions {
		if st == nil || st.CreatedAt.IsZero() || now.Sub(st.CreatedAt) > oauthSessionTTL {
			delete(oauthSessions, id)
			removed++
		}
	}
	return removed
}

// registerOAuthSession 登记一个新的 OAuth 会话，并顺手清掉过期的。
//
// 把「登记」和「清理」收在一处：这个 map 没有别的地方会删除条目，所以清理
// 必须挂在唯一的写入口上，否则会随「点击开始 OAuth 登录」的次数单调增长。
func registerOAuthSession(id string, state *oauthSessionState) {
	oauthSessionsMu.Lock()
	defer oauthSessionsMu.Unlock()
	pruneOAuthSessionsLocked(time.Now())
	oauthSessions[id] = state
}

type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
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
	mux.HandleFunc("/admin/api/accounts/enable", corsHandler(handleAdminAccountEnable))
	mux.HandleFunc("/admin/api/accounts/disable", corsHandler(handleAdminAccountDisable))
	mux.HandleFunc("/admin/api/cooldowns", corsHandler(handleAdminCooldowns))
	mux.HandleFunc("/admin/api/accounts/detail", corsHandler(handleAdminAccountDetail))
	mux.HandleFunc("/admin/api/cooldowns/clear", corsHandler(handleAdminCooldownClear))
	mux.HandleFunc("/admin/api/keys", corsHandler(handleAdminGetKeys))
	mux.HandleFunc("/admin/api/keys/generate", corsHandler(handleAdminGenerateKey))
	mux.HandleFunc("/admin/api/keys/delete", corsHandler(handleAdminDeleteKey))
	mux.HandleFunc("/admin/api/models", corsHandler(handleAdminModels))
	mux.HandleFunc("/admin/api/models/batch", corsHandler(handleAdminModelsBatchAdd))
	mux.HandleFunc("/admin/api/models/delete", corsHandler(handleAdminModelDelete))
	mux.HandleFunc("/admin/api/recommended-models", corsHandler(handleAdminRecommendedModels))
	// 全部模型清单：与推荐分组不同，这份是平铺的完整列表，面板展开折叠块时才请求。
	mux.HandleFunc("/admin/api/model-catalog", corsHandler(handleAdminModelCatalog))
	mux.HandleFunc("/admin/api/config", corsHandler(handleAdminConfig))
	mux.HandleFunc("/admin/api/config/update", corsHandler(handleAdminUpdateConfig))
	// 上游渠道配置（见 upstream.go）：列表 / 保存 / 探测。
	mux.HandleFunc("/admin/api/upstreams", corsHandler(handleAdminUpstreams))
	mux.HandleFunc("/admin/api/upstreams/save", corsHandler(handleAdminUpstreamSave))
	mux.HandleFunc("/admin/api/upstreams/delete", corsHandler(handleAdminUpstreamDelete))
	mux.HandleFunc("/admin/api/upstreams/probe", corsHandler(handleAdminUpstreamProbe))
}

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(renderPage(adminHTML)))
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
	// 读 CurrentIdx 必须持池锁：pickAccount 在锁内更新它。
	p := loadPool()
	poolMu.Lock()
	poolIndex := p.CurrentIdx
	poolMu.Unlock()
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"accounts":  accounts,
			"total":     len(accounts),
			"poolIndex": poolIndex,
		},
	})
}

// GET /admin/api/accounts/export[?ids=a,b,c]
//
// 不带 ids 时导出全部（默认行为，与旧版一致）；带 ids 时只导出指定账号，
// 供面板的「导出选中」和「单独导出某一个」复用同一个接口。
// 导出的结构就是批量导入能接受的格式，所以导出文件可以直接重新导入。
func handleAdminAccountExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	// ids 为空 = 不过滤。
	var wanted map[string]struct{}
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		wanted = make(map[string]struct{})
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				wanted[id] = struct{}{}
			}
		}
		if len(wanted) == 0 {
			wanted = nil
		}
	}

	p := loadPool()
	poolMu.Lock()
	accounts := make([]accountTransfer, 0, len(p.Accounts))
	for _, account := range p.Accounts {
		if account == nil {
			continue
		}
		if wanted != nil {
			if _, ok := wanted[account.AccountID]; !ok {
				continue
			}
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
		// 走 accountCount：锁外 len(loadPool().Accounts) 与 addAccount 竞争。
		req.Email = fmt.Sprintf("user_%d", accountCount()+1)
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

	if err := addAccount(acc); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "save account: " + err.Error()})
		return
	}
	// 用 truncateEmail 保持与请求路径日志一致：容器日志的可见范围通常比
	// 管理面板宽，不该在里面出现完整账号邮箱。
	log.Printf("Account added via API: %s", truncateEmail(req.Email))

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

	found, err := removeAccount(req.AccountID)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "delete account: " + err.Error()})
	} else if found {
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

	registerOAuthSession(sessionID, state)

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
		if err := addAccount(acc); err != nil {
			oauthSessionsMu.Lock()
			state.Done, state.Success, state.Error = true, false, "save account: "+err.Error()
			oauthSessionsMu.Unlock()
			return
		}

		oauthSessionsMu.Lock()
		state.Done = true
		state.Success = true
		state.Email = email
		oauthSessionsMu.Unlock()
		log.Printf("OAuth account added: %s", truncateEmail(email))
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
	// 必须在锁内把字段拷出来：state 的这些字段由后台 goroutine 在
	// oauthSessionsMu 保护下改写（见 handleOAuthStart），之前只把指针取到锁外、
	// 再在锁外读 state.Done/Email，是数据竞争——string 是两个机器字，极端调度
	// 下可能读到「指针是新值、长度是旧值」的撕裂值。
	var done, success bool
	var email, stateErr string
	if ok {
		done, success = state.Done, state.Success
		email, stateErr = state.Email, state.Error
	}
	oauthSessionsMu.Unlock()

	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "session not found"})
		return
	}

	resp := map[string]any{
		"done":    done,
		"success": success,
	}
	if done {
		resp["email"] = email
		if !success {
			resp["error"] = stateErr
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
				// 只报长度、不回显前缀：这条错误会随响应交给客户端，而这批 token 是
				// 用户正在导入的账号凭据，16 个字符已足以用来关联/比对。
				errors = append(errors, fmt.Sprintf("token (len %d): %v", len(token), err))
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
			if resp.Data.RefreshToken != "" {
				acc.RefreshToken = resp.Data.RefreshToken
			}
			if err := addAccount(acc); err != nil {
				errors = append(errors, fmt.Sprintf("%s: save account: %v", email, err))
				continue
			}
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
		if resp.Data.RefreshToken != "" {
			acc.RefreshToken = resp.Data.RefreshToken
		}
		if err := addAccount(acc); err != nil {
			errors = append(errors, fmt.Sprintf("%s: save account: %v", email, err))
			continue
		}
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
	// 不在 poolMu 内刷新：refreshAccountToken → savePool 会取 poolMu，
	// 持锁调用即自锁死；而且持锁跨越 N 次网络请求会阻塞所有代理请求。
	// 先在锁内拷贝一份账号指针，再在锁外逐个刷新。
	p := loadPool()
	poolMu.Lock()
	accounts := append([]*Account(nil), p.Accounts...)
	poolMu.Unlock()
	failed := 0
	for _, a := range accounts {
		if err := refreshAccountToken(a); err != nil {
			failed++
			log.Printf("Refresh failed for %s: %v", truncateEmail(a.Email), err)
		}
	}
	if failed > 0 {
		writeAPI(w, http.StatusBadGateway, apiResponse{Error: fmt.Sprintf("%d of %d accounts failed to refresh; see server log", failed, len(accounts))})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All tokens refreshed"})
}

// POST /admin/api/accounts/delete-all
func handleAdminDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	previous, cursor := p.Accounts, p.CurrentIdx
	p.Accounts, p.CurrentIdx = []*Account{}, 0
	if err := savePoolLocked(); err != nil {
		p.Accounts, p.CurrentIdx = previous, cursor
		poolMu.Unlock()
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "delete accounts: " + err.Error()})
		return
	}
	cooldowns.clearAll()
	poolMu.Unlock()
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

	// 重置：状态归位、清空用量计数、刷新 Token，
	// 并清掉该账号的全部「账号×模型」冷却——否则 UI 显示已恢复，
	// 实际仍会被 pickAccount 跳过最多一整个冷却周期。
	// 写入共享账号对象必须持池锁：savePool 会在池锁内 marshal，
	// 锁外改字段属于数据竞争。
	if err := refreshAccountToken(acc); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "reset failed: " + err.Error()})
		return
	}
	poolMu.Lock()
	previousTotal, previousDaily, previousDate := acc.UsageCount, acc.DailyUsageCount, acc.DailyUsageDate
	acc.UsageCount, acc.DailyUsageCount, acc.DailyUsageDate = 0, 0, ""
	if err := savePoolLocked(); err != nil {
		acc.UsageCount, acc.DailyUsageCount, acc.DailyUsageDate = previousTotal, previousDaily, previousDate
		poolMu.Unlock()
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "save reset: " + err.Error()})
		return
	}
	poolMu.Unlock()
	cooldowns.clearAccount(req.AccountID)

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account reset"})
}

// accountEnabledDisabled 是 enable/disable 两个 handler 共用的请求体。
type accountEnabledDisabled struct {
	AccountID string `json:"accountId"`
}

// setAccountDisabled 设置账号的手动禁用标记并持久化。
// 返回 (是否找到账号, 错误)。
//
// 冷却的清理放在「启用」分支：启用意味着用户要让账号真正回到轮询，
// 此时必须清掉它的全部冷却记录，否则会顶着旧冷却继续被跳过。
// 禁用时不清理也无妨——禁用的账号本就不会被选中。
func setAccountDisabled(accountID string, disabled bool) (bool, error) {
	acc := getAccountByID(accountID)
	if acc == nil {
		return false, nil
	}

	poolMu.Lock()
	prev := acc.Disabled // 记录旧值：取反回滚在「重复禁用」时会写反
	acc.Disabled = disabled
	if err := savePoolLocked(); err != nil {
		acc.Disabled = prev
		poolMu.Unlock()
		return true, err
	}
	poolMu.Unlock()

	if !disabled {
		// 启用：清掉该账号全部冷却，让它真正回到轮询
		cooldowns.clearAccount(accountID)
	}
	return true, nil
}

// POST /admin/api/accounts/enable  body: { accountId }
// 手动启用：清掉禁用标记与全部冷却，账号立即回到轮询。
func handleAdminAccountEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req accountEnabledDisabled
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	found, err := setAccountDisabled(req.AccountID, false)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}
	if !found {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account enabled"})
}

// POST /admin/api/accounts/disable  body: { accountId }
// 手动禁用：不参与轮询且不会自动恢复，直到再次启用。
func handleAdminAccountDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req accountEnabledDisabled
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	found, err := setAccountDisabled(req.AccountID, true)
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}
	if !found {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account disabled"})
}

// Global proxy config (mutable via API)
var (
	proxyConfig   = defaultProxyConfig()
	proxyConfigMu sync.Mutex
)

// GET /admin/api/cooldowns
// 返回当前全部「账号×模型」冷却记录（含剩余秒数），供面板展示。
func handleAdminCooldowns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	entries := cooldowns.snapshot(time.Now())
	if entries == nil {
		entries = []cooldownEntry{}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"cooldowns": entries,
	}})
}

type proxyConfigData struct {
	Strategy        string            `json:"strategy"`
	Headers         map[string]string `json:"headers"`
	HeadersComplete bool              `json:"headersComplete,omitempty"`
}

// defaultCooldownMinutes 是冷却时长的兜底值（分钟）。
// 实际值存在账号池文件里（AccountPool.CooldownMinutes），见 pool.go 的
// cooldownMinutes / setCooldownMinutes。
const defaultCooldownMinutes = 30

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
	// 必须拷贝：直接把共享切片交给 json.Marshal，会和并发的
	// 生成/删除密钥（append 会换底层数组）竞争。
	p := loadPool()
	poolMu.Lock()
	keys := append([]string(nil), p.Keys...)
	poolMu.Unlock()
	if keys == nil {
		keys = []string{}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": keys}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	key, err := newAPIKey()
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "failed to generate key"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	previous := p.Keys
	p.Keys = append(p.Keys, key)
	if err := savePoolLocked(); err != nil {
		p.Keys = previous
		poolMu.Unlock()
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "save key: " + err.Error()})
		return
	}
	poolMu.Unlock()
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
	previous := append([]string(nil), p.Keys...)
	for i, k := range p.Keys {
		if k == req.Key {
			p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
			break
		}
	}
	if err := savePoolLocked(); err != nil {
		p.Keys = previous
		poolMu.Unlock()
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "delete key: " + err.Error()})
		return
	}
	poolMu.Unlock()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Key deleted"})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg := getProxyConfig()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address":         "127.0.0.1:3457",
		"strategy":        cfg.Strategy,
		"version":         versionLabel(),
		"poolPath":        poolPath,
		"defaultModel":    getDefaultModel(),
		"headers":         cfg.Headers,
		"cooldownMinutes": cooldownMinutes(),
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
		Strategy        string            `json:"strategy"`
		Headers         map[string]string `json:"headers"`
		ReplaceHeaders  bool              `json:"replaceHeaders"`
		DefaultModel    *string           `json:"defaultModel"`
		CooldownMinutes *int              `json:"cooldownMinutes"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	// ---- 先把所有参数校验完，再做任何修改 ----
	// 否则会出现「客户端收到 400，但服务端已部分生效」的不一致。
	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid strategy, must be: round_robin, fill, random"})
			return
		}
	}

	var cooldownToApply *int
	if req.CooldownMinutes != nil {
		// 允许 1~1440（1 分钟到 1 天）；0 视为恢复默认
		m := *req.CooldownMinutes
		if m < 0 || m > 1440 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "cooldownMinutes must be 0-1440"})
			return
		}
		if m == 0 {
			m = defaultCooldownMinutes
		}
		cooldownToApply = &m
	}

	// 同一次请求的设置一起校验、一起保存；写盘失败时全部回滚。
	// 池锁同时串行化配置更新，避免并发请求覆盖彼此的请求头修改。
	p := loadPool()
	poolMu.Lock()
	if req.DefaultModel != nil {
		id := normalizeExistingModelID(*req.DefaultModel)
		if !modelExistsLocked(p, id) {
			poolMu.Unlock()
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: errModelUnknown.Error()})
			return
		}
		req.DefaultModel = &id
	}

	// 必须基于副本修改：getProxyConfig() 返回共享指针，就地改 Headers
	// 会与其他 goroutine 的 map 遍历并发，触发
	// "concurrent map iteration and map write" 直接终止整个进程。
	cur := getProxyConfig()
	next := &proxyConfigData{
		Strategy:        cur.Strategy,
		Headers:         make(map[string]string, len(cur.Headers)),
		HeadersComplete: cur.HeadersComplete,
	}
	if req.ReplaceHeaders {
		next.HeadersComplete = true
	} else {
		for k, v := range cur.Headers {
			next.Headers[k] = v
		}
	}
	if req.Strategy != "" {
		next.Strategy = req.Strategy
	}
	for k, v := range req.Headers {
		next.Headers[k] = v
	}
	previousConfig, previousModel, previousCooldown := p.ProxyConfig, p.DefaultModel, p.CooldownMinutes
	p.ProxyConfig = next
	if req.DefaultModel != nil {
		p.DefaultModel = *req.DefaultModel
	}
	if cooldownToApply != nil {
		p.CooldownMinutes = *cooldownToApply
	}
	if err := savePoolLocked(); err != nil {
		p.ProxyConfig, p.DefaultModel, p.CooldownMinutes = previousConfig, previousModel, previousCooldown
		poolMu.Unlock()
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "persist config: " + err.Error()})
		return
	}
	setProxyConfig(next)
	if previousCooldown <= 0 {
		previousCooldown = defaultCooldownMinutes
	}
	if cooldownToApply != nil && *cooldownToApply != previousCooldown {
		cooldowns.clearAll()
	}
	defaultModel := p.DefaultModel
	if !modelExistsLocked(p, defaultModel) {
		defaultModel = firstAvailableModelLocked(p)
	}
	cooldown := p.CooldownMinutes
	if cooldown <= 0 {
		cooldown = defaultCooldownMinutes
	}
	poolMu.Unlock()

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":        next.Strategy,
		"headers":         next.Headers,
		"defaultModel":    defaultModel,
		"cooldownMinutes": cooldown,
	}})
}

// GET /admin/api/models lists user-added models.
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
	poolMu.Lock()
	// total 必须在锁内取：写响应时读 len(p.Accounts) 已经解锁，会与 addAccount /
	// removeAccount 在锁内的 append / 重建切片竞争（读 slice header 即 data race）。
	total := len(p.Accounts)
	active, expired, disabled := 0, 0, 0
	for _, a := range p.Accounts {
		if a.Disabled {
			disabled++
			continue // 手动禁用不算「活跃」
		}
		switch a.Status {
		case "active":
			active++
		case "expired":
			expired++
		}
	}
	poolMu.Unlock()

	// strategy 必须回读真实配置：设置页用它回填下拉框，
	// 写死常量会让改成 fill / random 后仍显示 round_robin。
	cfg := getProxyConfig()

	// 冷却数来自真实的「账号×模型」冷却表（旧的 Status=="cooldown" 已不再写入）
	cooling := len(cooldowns.snapshot(time.Now()))

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":    total,
			"active":   active,
			"cooldown": cooling,
			"expired":  expired,
			"disabled": disabled,
			"strategy": cfg.Strategy,
			"version":  versionLabel(),
		},
	})
}

// renderPage 把页面里的版本号占位符换成实际版本。
//
// 页面是 go:embed 的静态 HTML，没有模板引擎；不替换的话界面上会直接显示
// __CLINE_PROXY_VERSION__ 这个 token。
func renderPage(html string) string {
	return strings.ReplaceAll(html, versionPlaceholder, versionLabel())
}
