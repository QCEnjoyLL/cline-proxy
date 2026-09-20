package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMaxTokens       = 128000
	defaultReasoningEffort = "high"
	// maxPendingToolCalls 是单个流里同时跟踪的工具调用数上限。
	// pendingTools 按上游给的下标索引，下标不受我们控制；没有上限的话，
	// 一个恶意/异常的上游可以用递增下标把 map 撑到内存耗尽（模型单次
	// 响应里常见工具数是几十个量级，300 足够宽松）。
	maxPendingToolCalls = 300
	// shutdownGracePeriod 是优雅退出时等待进行中请求结束的时长。
	//
	// 取 10s 是为了对齐 docker stop 的默认宽限期：超过它 docker 会直接 SIGKILL，
	// 设更长没有意义（确实需要更久就同时调大 compose 里的 stop_grace_period）。
	// 这个时间只用于「收尾」——把已经拿到的数据写完、发出终止事件，不是等上游生成完。
	shutdownGracePeriod = 10 * time.Second

	// maxRequestBodyBytes 是代理端点接受的请求体上限（32 MiB）。
	//
	// 存在的理由：这个端口在 docker-compose 里直接对外发布，而未配置 API Key 时
	// 鉴权完全放行，任何人都能 POST 任意大的 body；没有上限时 io.ReadAll 会把它
	// 整个读进内存，足以把容器撑爆。32 MiB 远高于正常用量（128k tokens 的上下文
	// 大约几百 KB～1 MB），不会误伤正常请求。
	maxRequestBodyBytes = 32 << 20
)

// upstreamError 是一个带「该回给客户端什么状态码」的上游错误。
//
// 背景：旧实现把所有上游失败都压成 HTTP 500。客户端因此无法区分
// 「上游限流（该退避后重试）」和「我的请求本身有问题（该改请求）」，
// 只能一律重试——限流时反而加剧限流，也会让 429 这类本该由客户端
// 处理的信号彻底消失。
type upstreamError struct {
	Status  int    // 回给客户端的 HTTP 状态码
	Type    string // OpenAI 风格的 error.type
	Message string
}

func (e *upstreamError) Error() string { return e.Message }

func newUpstreamError(status int, errType, format string, args ...any) *upstreamError {
	return &upstreamError{Status: status, Type: errType, Message: fmt.Sprintf(format, args...)}
}

// clientStatusForUpstream 决定把上游的状态码怎么转达给客户端。
//
// 规则与理由：
//   - 2xx/3xx 不该走到这里；出现即视为上游异常，回 502。
//   - 429 原样转达：让客户端知道「被限流了」，这是我们唯一不该自己吞掉的信号
//     （账号池已经换过号了，仍然 429 说明整体都在限流）。
//   - 401/403 转 502：这是**我们**的账号凭据失效，不是客户端的 API Key 有问题。
//     若原样回 401，客户端会以为自己的 Key 无效而反复重配。
//   - 其余 4xx 原样转达：这多半是客户端请求本身的问题（模型名、参数、
//     上下文超长），客户端需要看到真实原因才知道改什么。
//   - 5xx 转 502：上游故障是我们的问题，不该让客户端以为是他自己的错。
func clientStatusForUpstream(upstreamStatus int) int {
	switch {
	case upstreamStatus == http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case upstreamStatus == http.StatusUnauthorized || upstreamStatus == http.StatusForbidden:
		return http.StatusBadGateway
	case upstreamStatus >= 400 && upstreamStatus < 500:
		return upstreamStatus
	default:
		return http.StatusBadGateway
	}
}

// writeUpstreamError 把 callClineAPI 的错误按语义回给客户端：
// 带状态的用它的状态码，其余（网络错误、本地故障等）一律 502。
func writeUpstreamError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	errType := "api_error"
	var ue *upstreamError
	if errors.As(err, &ue) {
		status = ue.Status
		if ue.Type != "" {
			errType = ue.Type
		}
	}
	// 状态码必须落在 100..999：net/http 的 WriteHeader 对越界值会直接 panic。
	// 当前所有调用点都只给 >=400 的值，这里纯粹是防御——一个写错的状态码
	// 不该把整个进程打挂。
	if status < 100 || status > 999 {
		log.Printf("  writeUpstreamError: invalid status %d, falling back to 502", status)
		status = http.StatusBadGateway
	}
	// err 为 nil 时 err.Error() 会 panic；调用点都有守卫，这里同样只是兜底。
	msg := "upstream error"
	if err != nil {
		msg = err.Error()
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": errType},
	})
}

// bodyTooLargeStatus 把「读请求体失败」映射成状态码。
//
// http.MaxBytesReader 超限时返回的是 *http.MaxBytesError，对应 413；其它读取
// 失败（连接中断等）仍按 400 处理——之前一律回 400，客户端无法区分「body 太大」
// 和「body 坏了」。
func bodyTooLargeStatus(err error) int {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

var passThroughKeys = []string{
	"tools", "tool_choice", "parallel_tool_calls", "functions", "function_call",
	"temperature", "top_p", "top_k", "stop", "presence_penalty", "frequency_penalty",
	"response_format", "user", "n", "logit_bias", "seed", "logprobs", "top_logprobs",
	"stream_options", "metadata",
	// provider / providerOptions 是上游渠道钉住的载体（见 upstream.go）。
	// 不带它们会被 buildUpstreamBody 丢弃，客户端的渠道偏好就永远到不了上游。
	"provider", "providerOptions",
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            json.RawMessage `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ReasoningEffortAlt  string          `json:"reasoningEffort,omitempty"`
	Extra               map[string]any  `json:"-"`
}

func startProxy(port int) error {
	// 在启动时就确定后台凭据。loadAdminCredentials 是 sync.Once 且只在登录
	// handler 里被调用，导致 ADMIN_PASSWORD 留空时随机密码要等到**第一次有人
	// 尝试登录**才打印——而 README 让用户用 `docker compose logs | grep
	// ADMIN_PASSWORD` 找密码，那时日志里还什么都没有，等于把人锁在门外。
	loadAdminCredentials()
	loadPool()
	// Refresh on first use so unavailable auth services cannot block the admin
	// listener or startup health checks. Concurrent first requests share a refresh.
	activeCount := snapshotAccountStats().Active
	log.Printf("Loaded %d active accounts from pool", activeCount)

	freePort(port)

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/health", corsHandler(handleHealth))
	mux.HandleFunc("/health", corsHandler(handleHealth))

	// Admin API (frontend + REST)
	registerAdminRoutes(mux)

	apiKeyHandler := func(next http.HandlerFunc) http.HandlerFunc {
		return corsHandler(func(w http.ResponseWriter, r *http.Request) {
			// Allow requests without key if no keys configured
			p := loadPool()
			// Keys 是共享切片，管理面板随时可能生成/删除密钥（append 会换
			// 底层数组），所以持池锁取一份快照再遍历。
			poolMu.Lock()
			keys := append([]string(nil), p.Keys...)
			poolMu.Unlock()
			if len(keys) == 0 {
				next(w, r)
				return
			}

			key := r.Header.Get("x-api-key")
			if key == "" {
				if b := r.Header.Get("Authorization"); len(b) > 7 && b[:7] == "Bearer " {
					key = b[7:]
				}
			}

			if !validAPIKey(keys, key) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]string{
						"message": "invalid API key. Generate one at /admin/ or set x-api-key header",
						"type":    "auth_error",
					},
				})
				return
			}
			next(w, r)
		})
	}

	modelsHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		models := allModels()
		modelsList := make([]map[string]any, 0, len(models))
		for _, model := range models {
			owner := "cline"
			if model.Custom {
				owner = "custom"
			}
			modelsList = append(modelsList, map[string]any{
				"id": model.ID, "object": "model", "created": time.Now().UnixMilli(), "owned_by": owner,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": modelsList})
	})
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/models", modelsHandler)

	chatHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		// stats 在池锁内一次取全。这里原先是 activeCount（startProxy 启动时算的
		// 一次快照）配合锁外 len(loadPool().Accounts)——后者是锁外读 slice header，
		// 会与 addAccount/removeAccount 在锁内的 append / 重建切片竞争（读 slice
		// header 本身就是 data race，可能撕出非法指针）。
		//
		// 注：陈旧快照本身并不会把请求挡在门外——旧写法是 && 短路，账号非空时
		// 第二个操作数已为 false，守卫不会触发。所以这是竞态修复，不是行为修复。
		stats := snapshotAccountStats()
		if stats.Active == 0 && stats.Total == 0 {
			// 503 而不是 401：客户端的 API Key 是好的，是**我们**没有可用账号。
			// 回 401 会让客户端以为自己的 Key 无效，跑去反复重新配置。
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]string{
					"message": "No accounts in pool. Run with --add-account or POST /admin/login to add accounts.",
					"type":    "no_account",
				},
			})
			return
		}

		// 限制请求体大小：这个端口在 docker-compose 里是直接对外发布的，而在
		// 未配置 API Key 时 apiKeyHandler 完全放行——没有上限的话，任何人都能
		// POST 一个任意大的 body，让 io.ReadAll 把它整个读进内存直至 OOM。
		// 32 MiB 远大于正常请求（128k tokens 的上下文约几百 KB～1 MB）。
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, bodyTooLargeStatus(err), map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "request_too_large"},
			})
			return
		}

		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		isStream, _ := params["stream"].(bool)
		toolCount := 0
		if tools, ok := params["tools"]; ok {
			if t, ok := tools.([]any); ok {
				toolCount = len(t)
			}
		}
		model, _ := params["model"].(string)
		log.Printf("  client: stream=%v tools=%d model=%s", isStream, toolCount, model)

		// Override system prompt from override.md for OpenAI format
		if override := loadOverrideContent(); override != "" {
			if msgs, ok := params["messages"].([]any); ok {
				found := false
				for _, m := range msgs {
					if mm, ok := m.(map[string]any); ok {
						if mm["role"] == "system" {
							mm["content"] = override
							found = true
							break
						}
					}
				}
				if !found {
					params["messages"] = append([]any{map[string]any{"role": "system", "content": override}}, msgs...)
				}
			}
		}

		resp, err := callClineAPI(r.Context(), params, isStream)
		if err != nil {
			log.Printf("  api error: %v", err)
			// 按上游语义回状态码（429 原样转达等），不再一律压成 500。
			writeUpstreamError(w, err)
			return
		}
		defer resp.Body.Close()

		if isStream {
			handleStreamResponse(w, resp)
		} else {
			handleNonStreamResponse(w, resp)
		}
	})
	mux.HandleFunc("/v1/chat/completions", chatHandler)
	mux.HandleFunc("/chat/completions", chatHandler)

	// Anthropic Messages API support
	anthropicHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		handleAnthropicMessages(w, r)
	})
	mux.HandleFunc("/v1/messages", anthropicHandler)
	mux.HandleFunc("/messages", anthropicHandler)

	addr := fmt.Sprintf(":%d", port)
	server := &http.Server{
		Addr:    addr,
		Handler: requireAdminAuth(mux),

		// ReadHeaderTimeout 防 slowloris：只发一半请求头的连接不该长期占着。
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout 回收 keep-alive 空闲连接。
		IdleTimeout: 120 * time.Second,
		// 刻意不设 WriteTimeout：它覆盖整个响应写入过程，会把正常的
		// 长时间流式（SSE）响应一起掐断。
	}

	fmt.Println("")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  Cline Go Proxy %s - No CLI Required\n", versionLabel())
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  http://%s\n", addr)
	fmt.Printf("  http://%s/v1\n", addr)
	fmt.Println("  API Key: any value")
	if model := getDefaultModel(); model != "" {
		fmt.Printf("  Model:   %s\n", model)
	} else {
		fmt.Println("  Model:   not configured; add a model in the admin panel or specify model in requests")
	}
	fmt.Printf("  Accounts: %d total, %d active\n", len(loadPool().Accounts), activeCount)
	fmt.Println(strings.Repeat("=", 58))

	// 优雅退出：docker stop / Ctrl-C 会发 SIGTERM/SIGINT，直接被杀会让正在输出的
	// 流式响应被硬切、也没有任何收尾。这里捕获信号后给进行中的请求一点时间自然结束。
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case sig := <-sigCh:
		log.Printf("%s received: shutting down gracefully (up to %s)", sig, shutdownGracePeriod)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			// 超时（还有流式请求没结束）时强制收尾，保证进程一定能退出。
			log.Printf("graceful shutdown incomplete: %v", err)
			_ = server.Close()
		}
		return nil
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	stats := snapshotAccountStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        versionLabel(),
		"activeAccounts": stats.Active,
	})
}

func corsHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// validAPIKey 判断请求带来的 key 是否命中配置里的任意一个。
//
// 恒定时间比较：普通 == 会在第一个不同字节处提前返回，攻击者能靠响应耗时
// 逐字节试出正确的 Key。这里刻意不提前 break——一旦命中就退出，命中位置同样
// 会体现在耗时上。
//
// 注意 subtle.ConstantTimeCompare 在两侧长度不同时立即返回 0，长度差异本身
// 无法隐藏——但长度不构成可用于逐步逼近的旁路，所以无需额外处理。
func validAPIKey(configured []string, provided string) bool {
	valid := false
	for _, k := range configured {
		if subtle.ConstantTimeCompare([]byte(k), []byte(provided)) == 1 {
			valid = true
		}
	}
	return valid
}

func cleanMessages(messages []any) []any {
	cleaned := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			cleaned = append(cleaned, m)
			continue
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

// effectiveModel 返回本次请求实际会发给上游的模型 ID。
//
// 客户端未指定 model 时回退到默认模型。这个判断必须是唯一实现：
// 冷却键（pickAccount / cooldowns.mark）与请求体（buildUpstreamBody）
// 都要用它，否则两处漂移会导致冷却标记到一个永远不会被查询的 key。
func effectiveModel(params map[string]any) string {
	if m, ok := params["model"].(string); ok && strings.TrimSpace(m) != "" {
		return m
	}
	return getDefaultModel()
}

func tokenLimit(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

func buildUpstreamBody(params map[string]any, stream bool) map[string]any {
	sessionID := fmt.Sprintf("sess_%d", time.Now().UnixMilli())

	maxTokens := defaultMaxTokens
	if mt, ok := tokenLimit(params["max_tokens"]); ok {
		maxTokens = mt
	} else if mt, ok := tokenLimit(params["max_completion_tokens"]); ok {
		maxTokens = mt
	}

	model := effectiveModel(params)

	body := map[string]any{
		"model":            model,
		"max_tokens":       maxTokens,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
	}

	if msgsRaw, ok := params["messages"]; ok {
		if msgsArr, ok := msgsRaw.([]any); ok {
			body["messages"] = cleanMessages(msgsArr)
		} else {
			body["messages"] = msgsRaw
		}
	}

	if stream {
		body["stream"] = true
	}

	if re, ok := params["reasoning_effort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	} else if re, ok := params["reasoningEffort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	}

	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}

	// 上游渠道钉住与模型重定向。放在客户端字段透传**之后**，这样用户在面板里
	// 配置的偏好优先于客户端传进来的同名键——否则任意持有 API Key 的调用者
	// 都能覆盖后台配置的上游路由。
	applyUpstreamPrefs(body, model)

	return body
}

func clineHeaders(token, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("X-Task-ID", sessionID)

	cfg := getProxyConfig()
	for k, v := range cfg.Headers {
		h.Set(k, v)
	}

	return h
}

// ctx 必须是发起请求的 HTTP handler 的 r.Context()：客户端断开时它会取消，
// 从而中止已经发往上游的请求——否则客户端早走了，我们还在替它消耗账号额度、
// 占着连接直到上游自己结束。传 context.Background() 会失去这个能力。
func callClineAPI(ctx context.Context, params map[string]any, stream bool) (*http.Response, error) {
	// 冷却过滤与冷却标记都用「实际发给上游的模型」（见 effectiveModel），
	// 两者必须一致，否则冷却会标记到永远不会被查询的 key。
	model := effectiveModel(params)
	acc := pickAccount(model)
	if acc == nil {
		// 503 而不是 500：池子暂时不可用，等待 + 重试是正确反应
		// （填号/冷却恢复后即可用），不是请求本身有问题。
		return nil, newUpstreamError(http.StatusServiceUnavailable, "no_account",
			"no active accounts available. Use --login or admin API to add accounts")
	}

	if model == "" {
		return nil, newUpstreamError(http.StatusBadRequest, "invalid_request_error",
			"model is required: specify a model in the request or add a default model in the admin panel")
	}
	token, err := ensureAccountToken(acc)
	if err != nil {
		// 我方账号凭据出了问题，不是客户端的错：给 502，别让他怀疑自己的 Key。
		// 邮箱用 truncateEmail：这条 message 会经 writeUpstreamError 原样回给
		// API 客户端，任何持有有效 Key 的调用者都能读，不该拿到完整账号邮箱。
		return nil, newUpstreamError(http.StatusBadGateway, "account_error",
			"account %s token failed: %v", truncateEmail(acc.Email), err)
	}

	body := buildUpstreamBody(params, stream)

	sessionID, _ := body["session_id"].(string)

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", clineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header = clineHeaders(token, sessionID)

	toolCount := 0
	if tools, ok := params["tools"]; ok {
		if t, ok := tools.([]any); ok {
			toolCount = len(t)
		}
	}
	log.Printf("  upstream: account=%s stream=%v tools=%d msgs=%d max_tokens=%v effort=%v",
		truncateEmail(acc.Email), stream, toolCount, getMsgCount(params), body["max_tokens"], body["reasoning_effort"])

	resp, err := httpClient.Do(req)
	if err != nil {
		// 网络错误不代表账号额度问题：不冷却、不禁用，直接返回错误。
		// 旧实现把网络抖动当成限流踢账号，一次断网就能下线整个账号池。
		// 502：上游不可达是我们的链路问题，不是客户端请求写错了。
		return nil, newUpstreamError(http.StatusBadGateway, "api_error",
			"upstream request: %v", err)
	}

	if resp.StatusCode == 401 {
		resp.Body.Close()
		// Refresh token and retry
		if refreshed, refreshErr := accountToken(acc, true, token); refreshErr == nil {
			token = refreshed
			// 必须重建请求：原 req.Body 已被首次发送消费并关闭，
			// 复用同一个 *http.Request 重试会因 body 长度不符而失败，
			// 导致 token 刷新后仍然报错（旧实现在这里永远重试不成功）。
			retryReq, rerr := http.NewRequestWithContext(ctx, "POST", clineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
			if rerr != nil {
				return nil, newUpstreamError(http.StatusBadGateway, "api_error",
					"rebuild retry request: %v", rerr)
			}
			retryReq.Header = clineHeaders(token, sessionID)
			resp, err = httpClient.Do(retryReq)
			if err != nil {
				return nil, newUpstreamError(http.StatusBadGateway, "api_error",
					"upstream retry: %v", err)
			}

			if resp.StatusCode == 401 {
				resp.Body.Close()
				poolMu.Lock()
				if acc.AccessToken == token {
					acc.Status = "expired"
					if saveErr := savePoolLocked(); saveErr != nil {
						log.Printf("Persist expired account: %v", saveErr)
					}
				}
				poolMu.Unlock()
				return nil, newUpstreamError(http.StatusBadGateway, "account_error",
					"account %s token expired permanently", truncateEmail(acc.Email))
			}
		} else {
			return nil, newUpstreamError(http.StatusBadGateway, "account_error",
				"account %s refresh failed: %v", truncateEmail(acc.Email), refreshErr)
		}
	}

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			// 只冷却「该账号 × 该模型」这一组合，到点自动恢复；
			// 同账号的其它模型不受影响（上游按模型独立计额）。
			//
			// 同时解析响应体：若是「额度用尽」，上游往往给出重置时间
			// （免费模型是次日零点、花费上限带 resets_at），用它比用配置里的
			// 固定时长准确——否则会在真实重置前把请求放回去，立刻再撞 429。
			info := parseLimitInfo(string(bodyBytes), time.Now(), true)
			ttl := markCooldown(acc, model, info)
			if info.Kind != limitKindUnknown && info.Kind != "" {
				log.Printf("  limit reached: %s x %s (%s), cooldown %s, resets %s",
					truncateEmail(acc.Email), model, info.Kind, ttl.Round(time.Second), formatResetAt(info.ResetAt))
			} else {
				log.Printf("  cooldown: %s x %s for %s (unrecognized 429)",
					truncateEmail(acc.Email), model, ttl.Round(time.Second))
			}
		}
		return nil, newUpstreamError(clientStatusForUpstream(resp.StatusCode), "upstream_error",
			"API %d: %s", resp.StatusCode, truncate(string(bodyBytes), 500))
	}

	poolMu.Lock()
	bumpAccountUsage(acc, time.Now())
	poolMu.Unlock()
	if err := savePool(); err != nil {
		log.Printf("Persist account usage: %v", err)
	}
	return resp, nil
}

func truncateEmail(email string) string {
	if len(email) <= 12 {
		return email
	}
	parts := splitEmail(email)
	if len(parts) == 2 && len(parts[0]) > 3 {
		return parts[0][:3] + "***@" + parts[1]
	}
	if len(email) > 12 {
		return email[:8] + "..."
	}
	return email
}

func splitEmail(email string) []string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			return []string{email[:i], email[i+1:]}
		}
	}
	return []string{email}
}

func getMsgCount(params map[string]any) int {
	if msgs, ok := params["messages"].([]any); ok {
		return len(msgs)
	}
	return 0
}

// maxSSELineBytes 是单条 SSE 行的长度上限（1 MiB）。
//
// 需要它的原因：bufio.Reader.ReadString 在遇到不带 '\n' 的输入时会不断扩容内部
// 缓冲，所以上游（或任何中间设备）只要吐一条没有换行的超长数据，就能让内存无界
// 增长直到 OOM。正常 SSE 帧远小于 1 MiB（最长的也就是带工具参数的那几条）。
const maxSSELineBytes = 1 << 20

// errSSELineTooLong 表示上游单行超过 maxSSELineBytes。
var errSSELineTooLong = errors.New("SSE line exceeds size limit")

// readSSELine 读一行（含结尾的 '\n'），语义与 bufio.Reader.ReadString('\n') 一致，
// 但会在单行超过 maxSSELineBytes 时返回 errSSELineTooLong 而不是继续吃内存。
func readSSELine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			// 这一行还没结束，继续攒；只在超过上限时才放弃。
			if len(buf) > maxSSELineBytes {
				return "", errSSELineTooLong
			}
			continue
		}
		return string(buf), err
	}
}

func handleStreamResponse(w http.ResponseWriter, upstream *http.Response) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		return
	}

	reader := bufio.NewReader(upstream.Body)
	for {
		line, err := readSSELine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 正常结束：上游已把剩余内容给了我们（可能带一个不带换行的尾巴）。
				if line != "" {
					w.Write([]byte(line + "\n"))
				}
			} else {
				// 上游中途出错（连接被重置、单行超长等）：必须发一个 error chunk
				// 再结束。旧实现只是 break，客户端拿不到任何错误信号，只能等连接
				// 关闭才察觉，看起来就像「回答被莫名截断」。
				log.Printf("  stream: upstream read error: %v", err)
				if msg, mErr := json.Marshal(map[string]any{
					"error": map[string]string{
						"message": "upstream stream failed: " + err.Error(),
						"type":    "upstream_error",
					},
				}); mErr == nil {
					w.Write([]byte("data: " + string(msg) + "\n\n"))
					flusher.Flush()
				}
			}
			break
		}

		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[5:])
			if payload == "" || payload == "[DONE]" {
				w.Write([]byte(line + "\n\n"))
				flusher.Flush()
				continue
			}

			// Try to normalize the response
			var obj map[string]any
			if err := json.Unmarshal([]byte(payload), &obj); err == nil {
				// Some Cline responses wrap in {data: {...}}
				if data, ok := obj["data"]; ok {
					if d, ok := data.(map[string]any); ok {
						if _, hasChoices := d["choices"]; hasChoices {
							obj = d
						}
						if _, hasID := d["id"]; hasID {
							obj = d
						}
					}
				}
				normalized := normalizeOpenAIResponse(obj)
				if normBytes, err := json.Marshal(normalized); err == nil {
					w.Write([]byte("data: " + string(normBytes) + "\n\n"))
					flusher.Flush()
					continue
				}
			}
		}

		w.Write([]byte(line + "\n"))
		flusher.Flush()
	}
}

func handleNonStreamResponse(w http.ResponseWriter, upstream *http.Response) {
	var raw map[string]any
	if err := json.NewDecoder(upstream.Body).Decode(&raw); err != nil {
		// 上游回了 200 但响应体不是合法 JSON：这是上游的问题，不是客户端的请求
		// 有问题，所以是 502 而不是 500。
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	// Some Cline responses wrap in {data: {...}}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}

	out = normalizeOpenAIResponse(out)

	if msg, ok := getNested(out, "choices", 0, "message").(map[string]any); ok {
		tc, _ := msg["tool_calls"].([]any)
		content, _ := msg["content"].(string)
		log.Printf("  nonstream finish=%v tool_calls=%d content_len=%d",
			getNested(out, "choices", 0, "finish_reason"),
			len(tc), len(content))
	}

	writeJSON(w, http.StatusOK, out)
}

// Anthropic Messages API support
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// toolAccumulator 累积单个流式工具调用的分片。
//
// 字段说明：
//   - started 表示 content_block_start 已发出；emitted 表示整块（含 stop）已完成。
//     两者要分开：参数可能分帧到达，必须先把块打开、再边收边发 delta。
//   - blockIndex 是本工具块在 Anthropic 内容数组里的下标。它与 OpenAI 的
//     tool_calls[].index 是两套编号：正文若先占了 0，工具块必须从 1 开始。
//   - lastDeltaLen 记录上一次已作为 input_json_delta 发出的字节数，用于只发增量。
//
// 字段说明刻意写在这里、而不是插在字段之间：插在字段之间的注释行会打断 gofmt
// 的字段对齐分组（它会要求 index/id/name/args 按更窄的宽度重新对齐），
// 让这个结构体一直不满足 gofmt。
type toolAccumulator struct {
	index        int
	id           string
	name         string
	args         string
	started      bool
	emitted      bool
	blockIndex   int
	lastDeltaLen int
}

type anthropicReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Messages    []anthropicMsg  `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	TopK        int             `json:"top_k,omitempty"`
	Stop        json.RawMessage `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Extra       map[string]any  `json:"-"`
}

// overrideContentCache 缓存 override.md 的内容，避免每个请求都重读磁盘并刷日志。
//
// 用「修改时间 + 大小」判断是否需要重新读取，所以运维改了文件（下一次写入之后）
// 依然会被重新加载，不必重启进程。
//
// 已知局限：同一秒内、且改后长度恰好不变的编辑不会被察觉（mtime 粒度 1s）。
// 这种编辑极少见，而且再改一次或重启即可生效；换来的是去掉每请求一次的磁盘读
// 与每请求一行的日志。
var (
	overrideMu      sync.Mutex
	overrideChecked bool
	overrideMissing bool
	overrideModTime time.Time
	overrideSize    int64
	overrideContent string
)

func loadOverrideContent() string {
	overrideMu.Lock()
	defer overrideMu.Unlock()

	fi, err := os.Stat("override.md")
	if err != nil {
		// 没有 override.md 是最常见的配置（不覆盖系统提示词）。只提示一次，
		// 否则每个请求都会打一行 "not found"，把日志彻底淹掉，还容易被误当异常。
		if !overrideChecked || !overrideMissing {
			log.Printf("  override.md not present: system prompt will come from the client request")
		}
		overrideChecked, overrideMissing = true, true
		overrideContent, overrideModTime, overrideSize = "", time.Time{}, 0
		return ""
	}

	if overrideChecked && !overrideMissing && fi.ModTime().Equal(overrideModTime) && fi.Size() == overrideSize {
		return overrideContent // 没变，直接用缓存
	}

	data, rerr := os.ReadFile("override.md")
	if rerr != nil {
		log.Printf("  failed to read override.md: %v", rerr)
		return ""
	}
	content := strings.TrimSpace(string(data))

	overrideChecked, overrideMissing = true, false
	overrideModTime, overrideSize, overrideContent = fi.ModTime(), fi.Size(), content

	// 只在内容真正发生变化时打日志（进程启动后第一次读取也算一次变化）。
	if content == "" {
		log.Printf("  override.md is empty: system prompt will come from the client request")
	} else {
		log.Printf("  override.md loaded, overriding the system prompt (%d bytes)", len(content))
	}
	return content
}

func extractStringContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := []string{}
		for _, b := range blocks {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if tMap, ok := t.(map[string]any); ok {
			// Already in OpenAI format
			if tMap["type"] == "function" {
				out = append(out, t)
				continue
			}
			// Convert Anthropic format to OpenAI
			oai := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tMap["name"],
					"description": tMap["description"],
					"parameters":  tMap["input_schema"],
				},
			}
			out = append(out, oai)
		}
	}
	return out
}

func anthropicToOpenAI(req anthropicReq) map[string]any {
	openAI := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
		"messages":   []any{},
	}
	if req.Temperature != 0 {
		openAI["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		openAI["top_p"] = req.TopP
	}
	// Convert Anthropic tools to OpenAI format
	if req.Tools != nil {
		var toolsArr []any
		if err := json.Unmarshal(req.Tools, &toolsArr); err == nil {
			openAI["tools"] = anthropicToolsToOpenAI(toolsArr)
		}
	}
	if req.ToolChoice != nil {
		openAI["tool_choice"] = req.ToolChoice
	}

	msgs := []any{}

	// System prompt: use override.md if it exists, otherwise use Anthropic's system field
	sysContent := loadOverrideContent()
	if sysContent == "" && req.System != nil {
		sysContent = extractStringContent(req.System)
	}
	if sysContent != "" {
		// 这里刻意不再每请求打一行：sysContent 可能来自 override.md，也可能来自
		// 请求自带的 system 字段，之前那行 "(from override.md)" 在后者情况下是错的。
		// override.md 的加载/变化已经由 loadOverrideContent 记一次日志。
		msgs = append(msgs, map[string]any{"role": "system", "content": sysContent})
	}

	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": m.Role, "content": c})
		case []any:
			textParts := []string{}
			var toolCalls []any
			var toolResults []any

			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "image":
						// skip images
					case "tool_use":
						argsStr := "{}"
						if input, ok := b["input"]; ok && input != nil {
							if s, ok := input.(string); ok {
								argsStr = s
							} else if bts, err := json.Marshal(input); err == nil {
								argsStr = string(bts)
							}
						}
						tc := map[string]any{
							"id":   b["id"],
							"type": "function",
							"function": map[string]any{
								"name":      b["name"],
								"arguments": argsStr,
							},
						}
						toolCalls = append(toolCalls, tc)
					case "tool_result":
						tr := map[string]any{
							"role":         "tool",
							"content":      b["content"],
							"tool_call_id": b["tool_use_id"],
						}
						toolResults = append(toolResults, tr)
					}
				}
			}

			if m.Role == "assistant" && len(toolCalls) > 0 {
				msg := map[string]any{
					"role":       "assistant",
					"content":    strings.Join(textParts, "\n"),
					"tool_calls": toolCalls,
				}
				msgs = append(msgs, msg)
			} else if m.Role == "user" && len(toolResults) > 0 {
				msgs = append(msgs, toolResults...)
				if len(textParts) > 0 {
					msgs = append(msgs, map[string]any{"role": "user", "content": strings.Join(textParts, "\n")})
				}
			} else {
				content := strings.Join(textParts, "\n")
				msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
			}
		}
	}

	openAI["messages"] = msgs
	return openAI
}

func openAIToAnthropic(openAI map[string]any) map[string]any {
	out := map[string]any{
		"id":    "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"type":  "message",
		"role":  "assistant",
		"model": getNested(openAI, "model"),
	}

	choices := getNested(openAI, "choices")
	if choices == nil {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}

	choice0 := getNested(openAI, "choices", 0).(map[string]any)
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}

	text := ""
	if msg != nil {
		if c, ok := msg["content"].(string); ok {
			text = sanitizeContent(c)
		}
	}

	contentBlocks := []any{map[string]any{"type": "text", "text": text}}

	// Convert tool_calls to Anthropic tool_use blocks
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			contentBlocks = []any{} // Clear text-only, proper response has both
			if text != "" {
				contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
			}
			for _, tcItem := range tc {
				if tcMap, ok := tcItem.(map[string]any); ok {
					funcData, _ := tcMap["function"].(map[string]any)
					input := funcData["arguments"]
					// OpenAI arguments is a JSON string; Anthropic expects an object
					if argsStr, ok := input.(string); ok {
						var argsObj any
						if json.Unmarshal([]byte(argsStr), &argsObj) == nil {
							input = argsObj
						}
					}
					block := map[string]any{
						"type":  "tool_use",
						"id":    tcMap["id"],
						"name":  funcData["name"],
						"input": input,
					}
					contentBlocks = append(contentBlocks, block)
				}
			}
		}
	}

	out["content"] = contentBlocks

	switch getNested(openAI, "choices", 0, "finish_reason") {
	case "stop":
		out["stop_reason"] = "end_turn"
	case "length":
		out["stop_reason"] = "max_tokens"
	case "tool_calls":
		out["stop_reason"] = "tool_use"
	default:
		out["stop_reason"] = "end_turn"
	}

	usage := map[string]any{}
	if u := getNested(openAI, "usage"); u != nil {
		if um, ok := u.(map[string]any); ok {
			usage["input_tokens"] = um["prompt_tokens"]
			usage["output_tokens"] = um["completion_tokens"]
		}
	}
	out["usage"] = usage

	return out
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	// 同 OpenAI 端点：这个端口对外发布且可能无鉴权，必须限制请求体大小。
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, bodyTooLargeStatus(err), map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "request_too_large"},
		})
		return
	}

	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "messages is required", "type": "parse_error"},
		})
		return
	}

	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens
	}

	openAIReq := anthropicToOpenAI(req)

	log.Printf("  anthropic: model=%s stream=%v msgs=%d", req.Model, req.Stream, len(req.Messages))

	// 同 OpenAI 端点：在池锁内一次取全，避免锁外读 slice header。
	stats := snapshotAccountStats()
	if stats.Active == 0 && stats.Total == 0 {
		// 与 OpenAI 端点一致：客户端 Key 没问题，是我方没有可用账号，回 503。
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{
				"message": "No accounts in pool",
				"type":    "no_account",
			},
		})
		return
	}

	resp, err := callClineAPI(r.Context(), openAIReq, req.Stream)
	if err != nil {
		log.Printf("  anthropic api error: %v", err)
		// 与 OpenAI 端点一致：按上游语义回状态码，不一律压成 500。
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		handleAnthropicStream(w, resp)
	} else {
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			// 同 handleNonStreamResponse：上游响应体非法是上游问题，回 502。
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}
		out := raw
		if data, ok := raw["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				out = d
			}
		}
		out = normalizeOpenAIResponse(out)
		anthropicResp := openAIToAnthropic(out)

		writeJSON(w, http.StatusOK, anthropicResp)
	}
}

func handleAnthropicStream(w http.ResponseWriter, upstream *http.Response) {
	log.Printf("  anthropic stream: starting real-time forward")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	emit := func(event string, data any) {
		d, _ := json.Marshal(data)
		w.Write([]byte(fmt.Sprintf("event: %s\n", event)))
		w.Write([]byte(fmt.Sprintf("data: %s\n\n", string(d))))
		flusher.Flush()
	}

	msgID := "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli())
	stopReason := "end_turn"
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          msgID,
			"role":        "assistant",
			"content":     []any{},
			"model":       "",
			"stop_reason": nil,
		},
	})

	textIndex := new(int)
	*textIndex = -1
	hasText := false

	// nextContentIndex 分配下一个 Anthropic 内容块下标。正文与工具块共用同一
	// 编号空间：正文先占了 0，工具块就得从 1 开始，否则下标会撞车。
	nextContentIndex := func() int {
		i := *textIndex + 1
		*textIndex = i
		return i
	}
	pendingTools := map[int]*toolAccumulator{}

	// emitToolStart 打开工具块。input 必须是空对象——Anthropic 规定参数一律由
	// 后续的 input_json_delta 事件累积而成。这里若直接塞进完整 args，官方 SDK
	// 会忽略随后的 delta，而那些 delta 携带的正是真实参数。
	emitToolStart := func(acc *toolAccumulator) {
		if acc.started {
			return
		}
		acc.started = true
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": acc.blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    acc.id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		})
	}

	// emitToolBlock 结束工具块，并在此刻一次性发出参数。
	//
	// 为什么不在收到片段时边收边发：上游的 arguments 分帧不可靠——不少实现会
	// 先发一个占位 "{}"，随后再发真正的完整 JSON。边收边发会把两者拼成
	// "{}{\"city\":...}" 这种非法 JSON，客户端解析必然失败。
	//
	// 因此先把原始分片拼好、在这里一次性校验并发出。代价是参数不再“边收边
	// 解析”，换来的是绝不产出非法 JSON——对工具调用来说后者是致命的，前者只是
	// 延迟。文档化的字段顺序仍与官方一致（start → JSON 分片 → delta → stop）。
	emitToolBlock := func(acc *toolAccumulator) {
		if acc.emitted {
			return
		}
		if !acc.started {
			// 上游一个参数片段都没给：仍要让客户端看到一个完整可解析的空参数调用，
			// 而不是一个从未打开的块。
			emitToolStart(acc)
		}
		acc.emitted = true
		for _, chunk := range toolInputChunks(acc.args) {
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": acc.blockIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": chunk,
				},
			})
		}
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": acc.blockIndex,
		})
	}

	reader := bufio.NewReader(upstream.Body)

	// streamFailed 记录「流是被中断的」。中断后绝不能再走正常收尾：那会发
	// stop_reason=end_turn，把一次被截断的响应伪装成模型主动结束。
	streamFailed := false

	for {
		line, err := readSSELine(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// 上游中途断开（连接重置、单行超长等）：按 Anthropic 的约定发
				// error 事件告知客户端，然后结束整个流。
				log.Printf("  anthropic stream: upstream read error: %v", err)
				emit("error", map[string]any{
					"type": "error",
					"error": map[string]string{
						"type":    "upstream_error",
						"message": "upstream stream failed: " + err.Error(),
					},
				})
				streamFailed = true
			}
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}

		if data, ok := obj["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				obj = d
			}
		}

		// Detect upstream SSE error
		if errPayload, ok := obj["error"]; ok {
			errBody, _ := json.Marshal(errPayload)
			log.Printf("  upstream SSE error: %s", string(errBody))
			emit("error", map[string]any{"type": "error", "error": errPayload})
			// 上游明确报错也算中断：不能再补 end_turn 收尾（旧实现会在发完
			// error 事件后又补一套「正常结束」的事件，自相矛盾）。
			streamFailed = true
			break
		}

		choices, _ := getNested(obj, "choices").([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}

		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			delta = choice
		}

		// Text content delta
		if c, ok := delta["content"].(string); ok && c != "" {
			if !hasText {
				hasText = true
				*textIndex++
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": *textIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": *textIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": sanitizeContent(c),
				},
			})
		}

		// 工具调用：边收边开块并发 input_json_delta，收尾统一放在流末尾。
		if tcRaw, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcRaw {
				tcMap, _ := tc.(map[string]any)
				if tcMap == nil {
					continue
				}
				idx := 0
				if i, ok := tcMap["index"].(float64); ok {
					idx = int(i)
				}
				if len(pendingTools) >= maxPendingToolCalls {
					if _, known := pendingTools[idx]; !known {
						// 上游给了异常多的工具下标：放弃新建，避免无上限内存占用。
						// 宁可少一个工具块，也不要在这里 OOM。
						log.Printf("  anthropic stream: too many tool calls (>%d), dropping index %d",
							maxPendingToolCalls, idx)
						continue
					}
				}
				acc, exists := pendingTools[idx]
				if !exists {
					acc = &toolAccumulator{index: idx}
					acc.blockIndex = nextContentIndex()
					pendingTools[idx] = acc
				}
				if id, ok := tcMap["id"].(string); ok && id != "" {
					acc.id = id
				}
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						acc.name = name
					}
					// 累积参数；如何处理占位 {} 见 appendToolArgs 的说明。
					if args, ok := fn["arguments"].(string); ok {
						acc.args = appendToolArgs(acc.args, args)
					}
				}
				// id 与 name 齐备就先把块打开，让客户端尽早知道要调用哪个工具。
				// 参数不在这里发：要等收尾时统一校验后一次性发出（见 emitToolBlock）。
				if acc.id != "" && acc.name != "" {
					emitToolStart(acc)
				}
			}
		}

		// Finish reason
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}
	// 流被中断：error 事件已经发过了，这里直接结束。
	//
	// 刻意不走下面的正常收尾——那些事件（content_block_stop / message_delta 带
	// stop_reason=end_turn / message_stop）会向客户端宣布「这次回答是完整结束的」，
	// 而实际上正文或工具参数可能已被截断。按 Anthropic 的约定，error 事件之后
	// 流就结束了（客户端读到 EOF，SDK 会把它当错误抛出）。
	if streamFailed {
		log.Printf("  anthropic stream aborted: hasText=%v tools=%d", hasText, len(pendingTools))
		return
	}
	if hasText {
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": *textIndex,
		})
	}

	// 把尚未收尾的工具块按内容下标顺序补齐。
	//
	// 必须排序：Go 的 map 遍历顺序是随机的，多工具调用时各块的 start/stop
	// 会交错乱序，客户端按 index 重建内容数组就会错位。这里直接插入排序，
	// 与 cooldown.go 的做法一致（条目很少，不值得引入 sort）。
	remaining := make([]*toolAccumulator, 0, len(pendingTools))
	for _, acc := range pendingTools {
		if !acc.emitted {
			remaining = append(remaining, acc)
		}
	}
	for i := 1; i < len(remaining); i++ {
		for j := i; j > 0 && remaining[j].blockIndex < remaining[j-1].blockIndex; j-- {
			remaining[j], remaining[j-1] = remaining[j-1], remaining[j]
		}
	}
	for _, acc := range remaining {
		emitToolBlock(acc)
	}

	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": 0,
		},
	})

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s", hasText, len(pendingTools), stopReason)
}

// appendToolArgs 把上游的 arguments 分片拼起来。
//
// 一个坑：部分 OpenAI 兼容实现的第一帧会发一个占位 "{}"，随后再发真正的完整
// 参数。直接字符串拼接会得到 "{}{\"city\":...}" 这种非法 JSON，客户端解析
// 必然失败。这里识别「已收内容是空对象占位、新分片又以 { 开头」的情况，用
// 新分片替换掉占位。
func appendToolArgs(acc, chunk string) string {
	if acc == "" {
		return chunk
	}
	if isPlaceholderToolArgs(acc) && strings.HasPrefix(strings.TrimSpace(chunk), "{") {
		return chunk
	}
	return acc + chunk
}

// isPlaceholderToolArgs 判断已累积的内容是不是「空对象」占位。
func isPlaceholderToolArgs(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" || t == "{}"
}

// toolInputChunks 把累积到的参数切成若干 partial_json 分片。
//
// 正常情况下只有一个分片（整段 JSON）。仅当上游给的参数拼起来不是合法 JSON 时
// 才退化成逐字符下发——这样至少保住「客户端拼出的原文与上游一致」这个性质
// （官方 SDK 会原样保留 partial_json），而不是因为一次解析失败就把整次工具调用
// 变成空参数。
func toolInputChunks(args string) []string {
	if args == "" {
		// 一个参数都没有：给出合法空对象，保证客户端解析不会失败。
		return []string{"{}"}
	}
	if json.Valid([]byte(args)) {
		return []string{args}
	}
	// 上游给的不是合法 JSON：原样分块下发，不猜测、不丢弃内容。
	// 按 rune 切分，保证多字节 UTF-8 不会被从中间截断。
	out := make([]string, 0, len(args))
	for _, r := range args {
		out = append(out, string(r))
	}
	return out
}

func normalizeOpenAIResponse(obj map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range obj {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}

	if choices, ok := out["choices"].([]any); ok {
		normalized := make([]any, 0, len(choices))
		for _, ch := range choices {
			if c, ok := ch.(map[string]any); ok {
				nc := make(map[string]any)
				for k, v := range c {
					if k == "provider_metadata" || k == "proxy_metadata" {
						continue
					}
					nc[k] = v
				}
				if msg, ok := nc["message"].(map[string]any); ok {
					nc["message"] = normalizeMessage(msg)
				}
				if delta, ok := nc["delta"].(map[string]any); ok {
					nd := make(map[string]any)
					for k, v := range delta {
						if k == "provider_metadata" || k == "proxy_metadata" {
							continue
						}
						nd[k] = v
					}
					if tc, ok := nd["tool_calls"].([]any); ok && len(tc) > 0 {
						if nd["content"] == nil {
							nd["content"] = ""
						}
					}
					nc["delta"] = nd
				}
				normalized = append(normalized, nc)
			} else {
				normalized = append(normalized, ch)
			}
		}
		out["choices"] = normalized
	}

	return out
}

func sanitizeContent(s string) string {
	return s
}

func normalizeMessage(msg map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range msg {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}
	if tc, ok := out["tool_calls"].([]any); ok && len(tc) > 0 {
		if out["content"] == nil {
			out["content"] = ""
		}
	}
	if c, ok := out["content"].(string); ok {
		out["content"] = sanitizeContent(c)
	}
	return out
}

// freePort 在启动前腾出端口——但**只清理本代理自己的旧进程**。
//
// 旧实现在 Windows 上会强杀任何占用该端口的进程，这在端口被别的服务占用时等于
// 替用户做决定、直接把对方进程连同未保存的数据一起干掉。端口被非本代理的程序
// 占用时，正确做法是明确告诉用户去处理，而不是替他杀。
//
// 注意这里只实现了 Windows 版：其它平台上原先静默什么都不做，用户看到的是
// ListenAndServe 报「address already in use」后进程退出，因此现在多给一句日志。
func freePort(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return // port is free
	}
	conn.Close()

	if runtime.GOOS != "windows" {
		log.Printf("port %d is occupied by another process; free it and start again"+
			" (this build only knows how to clear the port on Windows)", port)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		log.Printf("port %d is occupied and our own executable path is unknown (%v); not touching it", port, err)
		return
	}

	// 只对「可执行文件路径与自身相同」的进程下手；路径里的单引号按 PowerShell
	// 的字面量规则翻倍转义。
	quoted := strings.ReplaceAll(exe, "'", "''")
	script := fmt.Sprintf(
		`$c=Get-NetTCPConnection -LocalPort %d -State Listen -ErrorAction SilentlyContinue;`+
			`foreach($x in $c){$p=Get-Process -Id $x.OwningProcess -ErrorAction SilentlyContinue;`+
			`if($p -and $p.Path -eq '%s'){Stop-Process -Id $p.Id -Force}}`, port, quoted)
	_ = execCommand("powershell", "-Command", script).Run()
	time.Sleep(500 * time.Millisecond)

	// 再探一次：如果还占着，说明不是我们自己的进程。
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		c.Close()
		log.Printf("port %d is still occupied by a process that is not this proxy;"+
			" refusing to kill it — free the port manually or use -port", port)
	}
}
func getNested(obj map[string]any, keys ...any) any {
	current := any(obj)
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			if m, ok := current.(map[string]any); ok {
				current = m[k]
			} else {
				return nil
			}
		case int:
			if arr, ok := current.([]any); ok && k < len(arr) {
				current = arr[k]
			} else {
				return nil
			}
		default:
			return nil
		}
	}
	return current
}
