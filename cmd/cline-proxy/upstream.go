package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 上游渠道钉住（upstream pinning）。
//
// 背景：Cline 网关后面是两条完全不同的路由管道，钉住上游的写法互不通用——
//
//	direct（OpenRouter）：响应顶层带 provider（显示名）与 model（真实上游 ID），
//	    钉住写进顶层 provider.{only,order}，实测会被透传到 OpenRouter。
//	planner（Vercel AI Gateway）：响应带 provider_metadata.gateway.routing，
//	    顶层 provider.* 会被 Cline 丢弃，必须写 providerOptions.gateway.{only,order}。
//
// 管道归属不是固定的：同一个模型在不同时间可能落在这两条之一（实测 glm-5.3-flash
// 前后两次探测就从 planner 变成 direct），所以只能运行时探测 + 缓存，不能硬编码名单。
//
// 「是否免费」与管道无关（免费的 poolside/laguna-s-2.1:free 走 direct，
// 同样免费的 cline-free/deepseek-v4.1-flash 走 planner），因此这里不按 cost 分支。
const (
	pipelineDirect  = "direct"
	pipelinePlanner = "planner"
)

// probeSentinel 是探测用的假上游名。故意带一个不存在的渠道，让网关在**路由层**
// 就报错并列出可用渠道清单，从而不消耗任何 token。
const probeSentinel = "__probe__"

// probeMaxTokens 是探测请求的 max_tokens。
//
// 这里刻意不传 reasoning_effort：推理模型的思考过程会先用掉 token 预算，
// 小 max_tokens 下模型还没输出正文就被截断，上游回 500「empty response content」。
// 实测把它当成「渠道坏了」会误判——所以探测一律用足够大的预算且不带 effort。
const probeMaxTokens = 512

// upstreamSlugPattern 是合法的渠道 slug 形态（网关侧全部是小写字母/数字/连字符）。
var upstreamSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// providerListPatternDirect / providerListPatternPlanner 从错误文本里兜底抽取渠道清单。
//
// 边界刻意写成「slug 的逗号列表」而不是更省事的 [^.]+：错误文本尾部通常还跟着
// JSON 残片（wafer","type":"invalid_request_error"...），用 [^.]+ 会把它们一起吃进去，
// 结果最后一个渠道名被污染后过滤掉——实测中表现为「渠道数偶尔少一个」。
// 这里只在 slug 形态上继续匹配，遇到引号/括号自然停住。
var providerListPatternDirect = regexp.MustCompile(`Providers serving [^:]+:\s*([a-z0-9][a-z0-9-]*(?:\s*,\s*[a-z0-9][a-z0-9-]*)*)`)

var providerListPatternPlanner = regexp.MustCompile(`Available providers are:\s*([a-z0-9][a-z0-9-]*(?:\s*,\s*[a-z0-9][a-z0-9-]*)*)`)

// ModelUpstream 是「按模型配置上游渠道」的一条记录。
//
// Upstreams 为空 = 不注入任何偏好（自动模式）：由网关自己挑渠道并自带故障转移。
// 这是默认行为，也是推荐行为——实测网关在一次请求里会依次尝试 baseten(503)
// → fireworks(503) → alibaba(200)，比在代理层再叠一层重试更有效，也不会放大请求量。
type ModelUpstream struct {
	// Upstreams 是要钉住的渠道，按顺序写入 only（strict）或 order（preferred）。
	Upstreams []string `json:"upstreams,omitempty"`
	// Exclude 是永不使用的渠道。网关不支持 exclude/ignore 字段（实测被静默忽略），
	// 因此统一换算成 only 白名单：only = 已知渠道 - Exclude。
	Exclude []string `json:"exclude,omitempty"`
	// PinMode 为 "preferred" 时用 order 表达优先级（首位优先、其余回退）；
	// 空或其它值为 "strict"，只用 only 严格钉住。
	PinMode string `json:"pinMode,omitempty"`
	// Redirect 是把对外模型 ID 转发上游时替换成的真实模型 ID。
	// 用途：客户端统一使用稳定的别名，上游改名/换 ID 时只改这里。
	Redirect string `json:"redirect,omitempty"`
	// Aliases 是同样适用本记录的其它对外模型 ID。
	Aliases []string `json:"aliases,omitempty"`
	// Pipeline / Available / ProbedAt 是探测结果的缓存，由探测接口写入。
	Pipeline  string   `json:"pipeline,omitempty"`
	Available []string `json:"available,omitempty"`
	ProbedAt  int64    `json:"probedAt,omitempty"`
	UpdatedAt int64    `json:"updatedAt,omitempty"`
}

// isPinPreferred 判断是否使用 order 表达优先级。
func (m *ModelUpstream) isPinPreferred() bool {
	return strings.EqualFold(m.PinMode, "preferred")
}

// normalizeProviderSlug 把渠道名归一化：只保留小写字母与数字。
//
// 必要性来自实测：direct 管道的响应顶层 provider 是**显示名**（"OpenInference"、
// "DeepInfra"、"Upstage"），而探出来的 slug 是 "open-inference"、"deepinfra"。
// 不做归一化就无法判断「钉住的渠道」与「实际命中的渠道」是否同一个。
func normalizeProviderSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validUpstreamSlug 校验渠道 slug 形态，挡住把任意字符串注入请求体的可能。
func validUpstreamSlug(s string) bool {
	return len(s) <= 64 && upstreamSlugPattern.MatchString(s)
}

// sanitizeUpstreams 清洗一组渠道名：去空白、跳过非法项、去重（保序）。
func sanitizeUpstreams(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if !validUpstreamSlug(s) {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// lookupModelUpstreamLocked 找到 modelID 对应的上游配置。
//
// modelID 可能是某个记录的别名，所以除直接命中外还要扫一遍 Aliases。
// 找到时返回副本：调用方会在锁外继续使用，直接返回内部指针会与并发写竞争。
// Callers must hold poolMu.
func lookupModelUpstreamLocked(p *AccountPool, modelID string) *ModelUpstream {
	if p == nil || modelID == "" || len(p.PerModel) == 0 {
		return nil
	}
	if cfg, ok := p.PerModel[modelID]; ok {
		c := cfg
		return &c
	}
	// 别名扫描按 key 排序，保证同一次输入总是得到同一个结果。
	keys := make([]string, 0, len(p.PerModel))
	for k := range p.PerModel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cfg := p.PerModel[k]
		for _, alias := range cfg.Aliases {
			if alias == modelID {
				c := cfg
				return &c
			}
		}
	}
	return nil
}

// modelUpstreamSnapshot 取某个模型的上游配置副本（自行取锁）。
func modelUpstreamSnapshot(modelID string) *ModelUpstream {
	p := loadPool()
	poolMu.Lock()
	cfg := lookupModelUpstreamLocked(p, modelID)
	poolMu.Unlock()
	return cfg
}

// upstreamModelID 把对外模型 ID 解析成真正发给上游的模型 ID（应用 Redirect）。
func upstreamModelID(modelID string) string {
	cfg := modelUpstreamSnapshot(modelID)
	if cfg != nil && cfg.Redirect != "" {
		return cfg.Redirect
	}
	return modelID
}

// 用「整体替换」而不是与已有内容合并：客户端也可能自己传 providerOptions，
// 合并会让它的其它键（例如 sort）存活下来并一起发往上游——实测 gateway.sort
// 会让上游直接 500，等于把客户端的错误参数放大成一次失败请求。
// 面板配置存在时，这一块就该由面板说了算。
func setGatewayPrefs(body map[string]any, prefs map[string]any) {
	po, ok := body["providerOptions"].(map[string]any)
	if !ok || po == nil {
		po = map[string]any{}
	}
	po["gateway"] = prefs
	body["providerOptions"] = po
}

// setProviderPrefs 把偏好整体写入顶层 provider，理由同 setGatewayPrefs。
func setProviderPrefs(body map[string]any, prefs map[string]any) {
	body["provider"] = prefs
}

// buildUpstreamPrefs 把一条配置换算成网关认识的偏好键。
//
// 返回 nil 表示这条配置不需要注入任何东西（自动模式且没有排除项）。
func buildUpstreamPrefs(cfg *ModelUpstream) map[string]any {
	excluded := make(map[string]struct{}, len(cfg.Exclude))
	for _, u := range cfg.Exclude {
		excluded[u] = struct{}{}
	}

	// 钉住列表里剔除被排除的渠道：排除的优先级高于勾选。
	pinned := make([]string, 0, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		if _, bad := excluded[u]; !bad {
			pinned = append(pinned, u)
		}
	}

	// 排除换算成 only 白名单，需要已知渠道清单（来自探测缓存）。
	allowList := make([]string, 0, len(cfg.Available))
	if len(cfg.Exclude) > 0 && len(cfg.Available) > 0 {
		for _, u := range cfg.Available {
			if _, bad := excluded[u]; !bad {
				allowList = append(allowList, u)
			}
		}
	}

	prefs := map[string]any{}
	switch {
	case len(pinned) > 0 && cfg.isPinPreferred():
		order := make([]any, 0, len(pinned))
		for _, u := range pinned {
			order = append(order, u)
		}
		prefs["order"] = order
		// preferred 模式也要限制回退范围，否则网关可能回退到被排除的渠道。
		if len(allowList) > 0 {
			only := make([]any, 0, len(allowList))
			for _, u := range allowList {
				only = append(only, u)
			}
			prefs["only"] = only
		}
	case len(pinned) > 0:
		only := make([]any, 0, len(pinned))
		for _, u := range pinned {
			only = append(only, u)
		}
		prefs["only"] = only
	case len(allowList) > 0:
		only := make([]any, 0, len(allowList))
		for _, u := range allowList {
			only = append(only, u)
		}
		prefs["only"] = only
	default:
		return nil
	}
	return prefs
}

// applyUpstreamPrefs 就地把上游偏好与模型重定向写进已构造好的请求体。
//
// 管道未知时两种形式**同时**注入：实测在 planner 模型上额外加一个顶层 provider.only
// 不会报错、也不会干扰 providerOptions.gateway（顶层被网关忽略），所以这是安全的兜底，
// 省掉了「必须先探测成功才能钉住」的强依赖。
func applyUpstreamPrefs(body map[string]any, modelID string) {
	cfg := modelUpstreamSnapshot(modelID)
	if cfg == nil {
		return
	}
	if cfg.Redirect != "" {
		body["model"] = cfg.Redirect
	}

	prefs := buildUpstreamPrefs(cfg)
	if prefs == nil {
		return
	}

	usePlanner := cfg.Pipeline == pipelinePlanner || cfg.Pipeline == ""
	useDirect := cfg.Pipeline == pipelineDirect || cfg.Pipeline == ""
	if usePlanner {
		setGatewayPrefs(body, prefs)
	}
	if useDirect {
		setProviderPrefs(body, prefs)
	}
}

// ---------------------------------------------------------------------------
// 探测
// ---------------------------------------------------------------------------

// upstreamRouting 是一次响应里回读出来的路由信息。
type upstreamRouting struct {
	Pipeline      string
	Provider      string
	CanonicalSlug string
	Fallbacks     []string
	Plan          string
}

// unwrapUpstream 把上游的 {"data":{...}} 包装拆掉，返回真正含 choices 的那层。
func unwrapUpstream(obj map[string]any) map[string]any {
	if data, ok := obj["data"].(map[string]any); ok {
		if _, hasChoices := data["choices"]; hasChoices {
			return data
		}
	}
	return obj
}

// nestedMap 沿路径取嵌套 map，任一层缺失就返回 nil。
func nestedMap(obj map[string]any, path ...string) map[string]any {
	cur := obj
	for _, key := range path {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// firstChoiceMap 返回 choices[0] 这一层。
func firstChoiceMap(d map[string]any) map[string]any {
	choices, ok := d["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil
	}
	return choice
}

// stringSlice 把 []any 转成 []string，跳过非字符串项。
func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseUpstreamRouting 从响应体里回读管道归属与实际上游。
//
// 非流式元数据在 choices[0].message.provider_metadata；流式在 choices[0].delta 下，
// 这里两者都认，所以同一个函数既能解析探测响应也能解析流式分片。
func parseUpstreamRouting(raw []byte) upstreamRouting {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return upstreamRouting{}
	}
	d := unwrapUpstream(obj)

	var routing map[string]any
	if choice := firstChoiceMap(d); choice != nil {
		if msg, ok := choice["message"].(map[string]any); ok {
			routing = nestedMap(msg, "provider_metadata", "gateway", "routing")
		}
		if routing == nil {
			if delta, ok := choice["delta"].(map[string]any); ok {
				routing = nestedMap(delta, "provider_metadata", "gateway", "routing")
			}
		}
	}
	if routing == nil {
		routing = nestedMap(d, "provider_metadata", "gateway", "routing")
	}

	out := upstreamRouting{}
	if finalProvider, ok := routing["finalProvider"].(string); ok {
		out.Pipeline = pipelinePlanner
		out.Provider = finalProvider
	}
	if slug, ok := routing["canonicalSlug"].(string); ok {
		out.CanonicalSlug = slug
	}
	if plan, ok := routing["planningReasoning"].(string); ok {
		out.Plan = plan
	}
	out.Fallbacks = stringSlice(routing["fallbacksAvailable"])

	// direct 管道没有 routing，改看顶层 provider（显示名）与 model（真实上游 ID）。
	if topProvider, ok := d["provider"].(string); ok && topProvider != "" {
		if out.Pipeline == "" {
			out.Pipeline = pipelineDirect
			out.Provider = topProvider
		}
	}
	if out.CanonicalSlug == "" {
		if model, ok := d["model"].(string); ok && strings.Contains(model, "/") {
			out.CanonicalSlug = model
		}
	}
	return out
}

// parseAvailableProviders 从假上游探测的**错误响应**里抽出渠道清单。
//
// 两条管道的错误形态不同，因此先按管道解析，再退回到通用正则：
//   - direct：错误文本里嵌一段 JSON，metadata.available_providers 是数组
//   - planner：错误文本里是 "Available providers are: a, b, c"
func parseAvailableProviders(raw []byte, pipeline string) []string {
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)

	errText := ""
	if errValue, ok := obj["error"]; ok {
		switch v := errValue.(type) {
		case string:
			errText = v
		case map[string]any:
			// 有些错误是对象形态，直接找 available_providers。
			if list := extractProviderList(v); len(list) > 0 {
				return list
			}
			if msg, ok := v["message"].(string); ok {
				errText = msg
			}
		}
	}

	// 优先：错误文本里嵌的 JSON 片段。
	if idx := strings.Index(errText, "{"); idx >= 0 {
		var embedded map[string]any
		if json.Unmarshal([]byte(errText[idx:]), &embedded) == nil {
			if list := extractProviderList(embedded); len(list) > 0 {
				return list
			}
		}
	}

	// 兜底：两条管道各自的文本形态都试一遍。
	for _, pattern := range []*regexp.Regexp{providerListPatternPlanner, providerListPatternDirect} {
		if list := splitProviderTokens(pattern, errText); len(list) > 0 {
			return list
		}
	}
	return nil
}

// extractProviderList 在任意嵌套结构里找 available_providers 数组。
func extractProviderList(obj map[string]any) []string {
	// error.metadata.available_providers
	if errObj, ok := obj["error"].(map[string]any); ok {
		if meta := nestedMap(errObj, "metadata"); meta != nil {
			if list := stringSlice(meta["available_providers"]); len(list) > 0 {
				return list
			}
		}
	}
	// 顶层 metadata.available_providers
	if meta := nestedMap(obj, "metadata"); meta != nil {
		if list := stringSlice(meta["available_providers"]); len(list) > 0 {
			return list
		}
	}
	return nil
}

// splitProviderTokens 用正则抓一段再用 slug 形态过滤。
//
// 过滤是必需的：错误文本里可能混有 JSON 残片（如 ","type":"invalid_request_error"），
// 不过滤会把它们当成渠道名。
func splitProviderTokens(pattern *regexp.Regexp, text string) []string {
	if text == "" {
		return nil
	}
	m := pattern.FindStringSubmatch(text)
	if len(m) < 2 {
		return nil
	}
	out := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	for _, token := range strings.Split(m[1], ",") {
		s := strings.TrimSpace(token)
		s = strings.Trim(s, `"'`)
		if !validUpstreamSlug(s) {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// upstreamCall 是一次探测用的原始上游调用。
//
// 刻意不复用 callClineAPI：探测要读**非 200 响应体**（渠道清单就藏在错误里），
// 而 callClineAPI 会把非 200 转成 error 并丢掉响应体；探测也不该计入使用量、
// 不该触发冷却——那都是真实请求才有的副作用。
func upstreamCall(modelID string, body map[string]any, timeout time.Duration) (int, []byte, error) {
	account := pickAccount(modelID)
	if account == nil {
		return 0, nil, fmt.Errorf("no active accounts available")
	}
	token, err := ensureAccountToken(account)
	if err != nil {
		return 0, nil, fmt.Errorf("account token failed: %v", err)
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal body: %w", err)
	}

	sessionID, _ := body["session_id"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, clineAPIBase+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header = clineHeaders(token, sessionID)

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read upstream response: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// probeRequestBody 构造一次探测请求。
func probeRequestBody(modelID string, maxTokens int) map[string]any {
	return map[string]any{
		"model":      modelID,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": maxTokens,
		"session_id": fmt.Sprintf("sess_%d", time.Now().UnixMilli()),
	}
}

// probeResult 是探测接口返回给面板的结果。
type probeResult struct {
	ModelID       string   `json:"modelId"`
	UpstreamModel string   `json:"upstreamModel"`
	Pipeline      string   `json:"pipeline"`
	Provider      string   `json:"provider"`
	ProviderMatch bool     `json:"providerMatch"`
	Available     []string `json:"available"`
	Fallbacks     []string `json:"fallbacks"`
	LatencyMS     int64    `json:"latencyMs"`
	Note          string   `json:"note,omitempty"`
}

// appendProbeNote 追加一条探测说明（分号分隔）。
//
// 探测是多步的，每步都可能产生「值得告诉用户」的信息；直接赋值会让后一步
// 悄悄盖掉前一步的结论（比如把「钉住未生效」盖成「渠道清单解析失败」）。
func appendProbeNote(result *probeResult, note string) {
	if note == "" {
		return
	}
	if result.Note == "" {
		result.Note = note
		return
	}
	result.Note += "；" + note
}

// probeModelUpstreams 探测一个模型的管道归属与可用渠道清单。
//
// 共三步：
//  1. 发一次**带上当前钉住配置**的真实请求（有配置时就注入，没有就是自动模式），
//     既回读管道归属，也顺带回答「我钉的渠道到底生效了没有」。
//  2. 若管道仍未知，用顶层 provider 形式再试一次拿管道（两种形式同时注入是安全的）。
//  3. 带假上游名让网关在**路由层**报错并列出渠道清单（不产生 token 消耗）。
//
// 第 1 步必须真的注入钉住配置：否则「实际命中的渠道」与「用户钉的渠道」无关，
// providerMatch 就永远只是个偶然巧合，而不是在验证钉住是否生效。
func probeModelUpstreams(modelID string) (*probeResult, error) {
	upstreamModel := upstreamModelID(modelID)
	cfg := modelUpstreamSnapshot(modelID)
	result := &probeResult{ModelID: modelID, UpstreamModel: upstreamModel}

	// 与真实代理链路完全一致地构造请求体，这样探测结果才代表线上行为。
	probeBody := probeRequestBody(upstreamModel, probeMaxTokens)
	if cfg != nil {
		probeBody["model"] = upstreamModel
		applyUpstreamPrefs(probeBody, modelID)
	}

	start := time.Now()
	status, raw, err := upstreamCall(upstreamModel, probeBody, 180*time.Second)
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}

	routing := parseUpstreamRouting(raw)
	result.Pipeline = routing.Pipeline
	result.Provider = routing.Provider
	result.Fallbacks = routing.Fallbacks

	if status != http.StatusOK {
		appendProbeNote(result, fmt.Sprintf("基线请求返回 HTTP %d：%s", status, truncate(string(raw), 300)))
		return result, nil
	}
	if routing.Pipeline == "" {
		appendProbeNote(result, "响应里既没有 provider_metadata.gateway.routing，也没有顶层 provider，无法判定管道")
		return result, nil
	}

	// 钉住生效确认：实际命中的渠道是否就是钉住列表里的某一个（按归一化名比对，
	// 因为 direct 管道回的显示名是 "DeepInfra"，而清单里是 "deepinfra"）。
	if cfg != nil && len(cfg.Upstreams) > 0 {
		actual := normalizeProviderSlug(routing.Provider)
		for _, pinned := range cfg.Upstreams {
			if normalizeProviderSlug(pinned) == actual {
				result.ProviderMatch = true
				break
			}
		}
		if !result.ProviderMatch {
			appendProbeNote(result, fmt.Sprintf("钉住 %v 未生效：实际命中 %s", cfg.Upstreams, routing.Provider))
		}
	}

	// 渠道枚举：故意带一个不存在的渠道名，让网关在路由层拒绝并回吐可用清单。
	// 这一步不产生 token 消耗，也不会被计入使用量。
	enumBody := probeRequestBody(upstreamModel, 16)
	if routing.Pipeline == pipelinePlanner {
		setGatewayPrefs(enumBody, map[string]any{"only": []any{probeSentinel}})
	} else {
		setProviderPrefs(enumBody, map[string]any{"only": []any{probeSentinel}})
	}

	_, probeRaw, probeErr := upstreamCall(upstreamModel, enumBody, 60*time.Second)
	if probeErr != nil {
		appendProbeNote(result, "渠道枚举请求失败："+probeErr.Error())
		return result, nil
	}
	result.Available = parseAvailableProviders(probeRaw, routing.Pipeline)
	if len(result.Available) == 0 {
		// 用追加而非覆盖：上面可能已经有「钉住未生效」这类更要紧的结论，
		// 覆盖掉会让用户看不到它。
		appendProbeNote(result, "未能从网关错误里解析出渠道清单："+truncate(string(probeRaw), 300))
	}
	return result, nil
}

// saveProbeResult 把探测结果写进模型的上游配置缓存。
func saveProbeResult(modelID string, probe *probeResult) error {
	p := loadPool()
	poolMu.Lock()
	if p.PerModel == nil {
		p.PerModel = make(map[string]ModelUpstream)
	}
	entry, ok := p.PerModel[modelID]
	if !ok {
		entry = ModelUpstream{}
	}
	entry.Pipeline = probe.Pipeline
	entry.Available = probe.Available
	entry.ProbedAt = time.Now().UnixMilli()
	p.PerModel[modelID] = entry
	poolMu.Unlock()
	return savePool()
}
