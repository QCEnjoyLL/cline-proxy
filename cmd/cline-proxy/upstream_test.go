package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 纯函数：归一化、清洗、偏好构造
// ---------------------------------------------------------------------------

func TestNormalizeProviderSlugMatchesDisplayNames(t *testing.T) {
	// 这些配对全部来自真实探测响应：direct 管道回的是显示名，而渠道清单里是 slug。
	pairs := [][2]string{
		{"OpenInference", "open-inference"},
		{"DeepInfra", "deepinfra"},
		{"Upstage", "upstage"},
		{"Poolside", "poolside"},
		{"GMICloud", "gmicloud"},
		{"near-ai", "nearai"},
	}
	for _, p := range pairs {
		if got := normalizeProviderSlug(p[0]); got != normalizeProviderSlug(p[1]) {
			t.Errorf("normalizeProviderSlug(%q)=%q 与 %q 不匹配", p[0], got, p[1])
		}
	}
}
func TestSanitizeUpstreamsRejectsAndDedupes(t *testing.T) {
	// __probe__ 是我们自己的探测哨兵，不是真实渠道，不该能被存成用户配置。
	got := sanitizeUpstreams([]string{"alibaba", " alibaba ", "baseten", "", "Bad Slug", "x\"y", "__probe__"})
	want := []string{"alibaba", "baseten"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sanitizeUpstreams = %v, want %v", got, want)
	}
}
func TestBuildUpstreamPrefsAutoModeInjectsNothing(t *testing.T) {
	// 自动模式（无钉住、无排除）= 不注入，由网关自己决定。这是默认行为。
	if prefs := buildUpstreamPrefs(&ModelUpstream{}); prefs != nil {
		t.Fatalf("自动模式应返回 nil，得到 %v", prefs)
	}
	if prefs := buildUpstreamPrefs(&ModelUpstream{PinMode: "strict"}); prefs != nil {
		t.Fatalf("空上游列表应返回 nil，得到 %v", prefs)
	}
}

func TestBuildUpstreamPrefsStrictUsesOnly(t *testing.T) {
	prefs := buildUpstreamPrefs(&ModelUpstream{Upstreams: []string{"alibaba", "baseten"}})
	if prefs == nil {
		t.Fatal("严格模式应产生偏好")
	}
	if _, hasOrder := prefs["order"]; hasOrder {
		t.Fatal("严格模式不该产生 order")
	}
	only, ok := prefs["only"].([]any)
	if !ok || len(only) != 2 || only[0] != "alibaba" {
		t.Fatalf("only = %v", prefs["only"])
	}
}

func TestBuildUpstreamPrefsPreferredUsesOrder(t *testing.T) {
	prefs := buildUpstreamPrefs(&ModelUpstream{Upstreams: []string{"alibaba", "baseten"}, PinMode: "preferred"})
	order, ok := prefs["order"].([]any)
	if !ok || len(order) != 2 || order[0] != "alibaba" || order[1] != "baseten" {
		t.Fatalf("preferred 的 order = %v", prefs["order"])
	}
}

func TestBuildUpstreamPrefsExcludeWinsOverPin(t *testing.T) {
	// 被排除的渠道即使也在钉住列表里也不能生效，否则「排除」形同虚设。
	prefs := buildUpstreamPrefs(&ModelUpstream{
		Upstreams: []string{"alibaba", "baseten"},
		Exclude:   []string{"alibaba"},
		Available: []string{"alibaba", "baseten", "novita"},
	})
	only, ok := prefs["only"].([]any)
	if !ok {
		t.Fatalf("应产生 only，得到 %v", prefs)
	}
	for _, v := range only {
		if v == "alibaba" {
			t.Fatal("被排除的渠道不能出现在 only 里")
		}
	}
	// 严格钉住模式下 only 就是钉住列表本身（此时已剔除排除项），
	// 这样「勾了 A、同时也排除了 A」不会退化成把整个白名单放开。
	if len(only) != 1 || only[0] != "baseten" {
		t.Fatalf("严格模式下 only = %v, want [baseten]", only)
	}
}

func TestApplyUpstreamPrefsInjectsBothPipelinesWhenUnknown(t *testing.T) {
	useTemporaryPool(t)
	// 管道未知时两种形式同时注入：实测 planner 模型上多一个顶层 provider.only
	// 不会报错也不会干扰 providerOptions.gateway，所以这是安全兜底。
	pool = &AccountPool{PerModel: map[string]ModelUpstream{
		"m1": {Upstreams: []string{"alibaba"}},
	}}
	body := map[string]any{"model": "m1"}

	applyUpstreamPrefs(body, "m1")

	gw, ok := body["providerOptions"].(map[string]any)
	if !ok {
		t.Fatalf("应注入 providerOptions，body=%v", body)
	}
	gateway, ok := gw["gateway"].(map[string]any)
	if !ok || gateway["only"] == nil {
		t.Fatalf("应注入 providerOptions.gateway.only，body=%v", body)
	}
	pr, ok := body["provider"].(map[string]any)
	if !ok || pr["only"] == nil {
		t.Fatalf("应同时注入顶层 provider.only，body=%v", body)
	}
}

func TestApplyUpstreamPrefsHonorsKnownPipeline(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{PerModel: map[string]ModelUpstream{
		"planner-model": {Upstreams: []string{"alibaba"}, Pipeline: pipelinePlanner},
		"direct-model":  {Upstreams: []string{"upstage"}, Pipeline: pipelineDirect},
		"auto-model":    {},
	}}

	plannerBody := map[string]any{"model": "planner-model"}
	applyUpstreamPrefs(plannerBody, "planner-model")
	if _, has := plannerBody["provider"]; has {
		t.Error("已知 planner 管道时不该再注入顶层 provider")
	}
	if _, has := plannerBody["providerOptions"]; !has {
		t.Error("已知 planner 管道时应注入 providerOptions")
	}

	directBody := map[string]any{"model": "direct-model"}
	applyUpstreamPrefs(directBody, "direct-model")
	if _, has := directBody["providerOptions"]; has {
		t.Error("已知 direct 管道时不该再注入 providerOptions")
	}
	if _, has := directBody["provider"]; !has {
		t.Error("已知 direct 管道时应注入顶层 provider")
	}

	autoBody := map[string]any{"model": "auto-model"}
	applyUpstreamPrefs(autoBody, "auto-model")
	if _, has := autoBody["provider"]; has {
		t.Error("自动模式不该注入 provider")
	}
	if _, has := autoBody["providerOptions"]; has {
		t.Error("自动模式不该注入 providerOptions")
	}
}

func TestApplyUpstreamPrefsRedirectRewritesModel(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{PerModel: map[string]ModelUpstream{
		"alias": {Redirect: "deepseek/deepseek-v4-flash"},
	}}
	body := map[string]any{"model": "alias"}
	applyUpstreamPrefs(body, "alias")
	if body["model"] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("重定向未生效，model=%v", body["model"])
	}
}

func TestApplyUpstreamPrefsMatchesAliases(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{PerModel: map[string]ModelUpstream{
		"real-id": {Upstreams: []string{"alibaba"}, Aliases: []string{"[cline]-fast"}},
	}}

	// 别名请求应命中原记录。
	aliasBody := map[string]any{"model": "[cline]-fast"}
	applyUpstreamPrefs(aliasBody, "[cline]-fast")
	if _, has := aliasBody["providerOptions"]; !has {
		t.Fatalf("别名应命中原配置，body=%v", aliasBody)
	}

	// 完全未知的模型不注入。
	otherBody := map[string]any{"model": "nope"}
	applyUpstreamPrefs(otherBody, "nope")
	if _, has := otherBody["providerOptions"]; has {
		t.Fatalf("未知模型不该被注入，body=%v", otherBody)
	}
}

func TestUpstreamModelIDAppliesRedirect(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{PerModel: map[string]ModelUpstream{
		"alias": {Redirect: "vendor/real"},
		"plain": {},
	}}
	if got := upstreamModelID("alias"); got != "vendor/real" {
		t.Fatalf("upstreamModelID(alias) = %q", got)
	}
	if got := upstreamModelID("plain"); got != "plain" {
		t.Fatalf("upstreamModelID(plain) = %q", got)
	}
	if got := upstreamModelID("missing"); got != "missing" {
		t.Fatalf("upstreamModelID(missing) = %q", got)
	}
}

// ---------------------------------------------------------------------------
// 解析：管道归属与渠道清单
// ---------------------------------------------------------------------------

func TestParseUpstreamRoutingPlanner(t *testing.T) {
	raw := []byte(`{"data":{"choices":[{"message":{"content":"hi","provider_metadata":{"gateway":{"routing":{
		"canonicalSlug":"deepseek/deepseek-v4.1-flash",
		"finalProvider":"alibaba",
		"fallbacksAvailable":["fireworks","alibaba"],
		"planningReasoning":"System credentials planned for: alibaba"}}}}}]}}`)
	got := parseUpstreamRouting(raw)
	if got.Pipeline != pipelinePlanner {
		t.Fatalf("pipeline = %q", got.Pipeline)
	}
	if got.Provider != "alibaba" || got.CanonicalSlug != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("provider/slug = %q/%q", got.Provider, got.CanonicalSlug)
	}
	if len(got.Fallbacks) != 2 {
		t.Fatalf("fallbacks = %v", got.Fallbacks)
	}
}

func TestParseUpstreamRoutingDirect(t *testing.T) {
	// direct 管道没有 provider_metadata，靠顶层 provider（显示名）+ model（真实 ID）判定。
	raw := []byte(`{"id":"x","object":"chat.completion","model":"upstage/solar-pro4","provider":"Upstage",
		"choices":[{"message":{"content":"hi"}}]}`)
	got := parseUpstreamRouting(raw)
	if got.Pipeline != pipelineDirect {
		t.Fatalf("pipeline = %q, want direct", got.Pipeline)
	}
	if got.Provider != "Upstage" {
		t.Fatalf("provider = %q", got.Provider)
	}
	if got.CanonicalSlug != "upstage/solar-pro4" {
		t.Fatalf("canonicalSlug = %q", got.CanonicalSlug)
	}
}

func TestParseUpstreamRoutingStreamDelta(t *testing.T) {
	// 流式把元数据放在 choices[0].delta 下，不是 message。
	raw := []byte(`{"data":{"choices":[{"delta":{"provider_metadata":{"gateway":{"routing":{
		"finalProvider":"fireworks","canonicalSlug":"deepseek/deepseek-v4.1-flash"}}}}}]}}`)
	got := parseUpstreamRouting(raw)
	if got.Pipeline != pipelinePlanner || got.Provider != "fireworks" {
		t.Fatalf("流式解析失败: %+v", got)
	}
}

func TestParseUpstreamRoutingUnknownShape(t *testing.T) {
	got := parseUpstreamRouting([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	if got.Pipeline != "" {
		t.Fatalf("不应判定出管道，得到 %q", got.Pipeline)
	}
	if got := parseUpstreamRouting([]byte("not json")); got.Pipeline != "" {
		t.Fatalf("非法 JSON 不应 panic 或误判，得到 %+v", got)
	}
}

func TestParseAvailableProvidersPlannerText(t *testing.T) {
	raw := []byte(`{"error":"inference request failed: ... {\"error\":{\"message\":\"No available providers match the 'only' filter: __probe__. Available providers are: alibaba, baseten, boundless\"}}"}`)
	got := parseAvailableProviders(raw, pipelinePlanner)
	if len(got) != 3 || got[0] != "alibaba" {
		t.Fatalf("parsed = %v", got)
	}
}

func TestParseAvailableProvidersDirectJSON(t *testing.T) {
	raw := []byte(`{"error":"failed from Openrouter: request failed with status 404: {\"error\":{\"message\":\"no providers\",\"metadata\":{\"available_providers\":[\"upstage\",\"poolside\"]}}}"}`)
	got := parseAvailableProviders(raw, pipelineDirect)
	// JSON 数组路径保序（不像文本路径那样排序），顺序应原样保留。
	if len(got) != 2 || got[0] != "upstage" || got[1] != "poolside" {
		t.Fatalf("parsed = %v, want [upstage poolside]", got)
	}
}

func TestParseAvailableProvidersFiltersJSONNoise(t *testing.T) {
	// 错误文本里混着 JSON 残片，必须按 slug 形态过滤，否则会把它们当渠道名。
	raw := []byte(`{"error":"Available providers are: alibaba, baseten\",\"type\":\"invalid_request_error\"}"}`)
	got := parseAvailableProviders(raw, pipelinePlanner)
	for _, v := range got {
		if !validUpstreamSlug(v) {
			t.Fatalf("混入非法渠道名 %q（全部=%v）", v, got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("parsed = %v, want 2 个合法渠道", got)
	}
}

func TestParseAvailableProvidersNoList(t *testing.T) {
	if got := parseAvailableProviders([]byte(`{"error":"something else"}`), pipelinePlanner); got != nil {
		t.Fatalf("应返回 nil，得到 %v", got)
	}
	if got := parseAvailableProviders([]byte("garbage"), pipelineDirect); got != nil {
		t.Fatalf("非 JSON 应返回 nil，得到 %v", got)
	}
}

// ---------------------------------------------------------------------------
// 后台接口
// ---------------------------------------------------------------------------

// upstreamTestPool 装一个带账号的临时池。
func upstreamTestPool(t *testing.T) {
	t.Helper()
	useTemporaryPool(t)
	pool = &AccountPool{
		Accounts: []*Account{{
			AccountID: "acc_1", Email: "user@example.com",
			RefreshToken: "rt-secret-value", AccessToken: "workos:at-secret-value",
			Status: "active", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		}},
	}
}

func TestAdminUpstreamsListShape(t *testing.T) {
	upstreamTestPool(t)
	pool.PerModel = map[string]ModelUpstream{
		"cline-free/deepseek-v4.1-flash": {Upstreams: []string{"alibaba"}, Pipeline: pipelinePlanner},
	}

	rec := httptest.NewRecorder()
	handleAdminUpstreams(rec, httptest.NewRequest(http.MethodGet, "/admin/api/upstreams", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Models    []upstreamModelOption `json:"models"`
			Upstreams []upstreamEntry       `json:"upstreams"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || len(resp.Data.Models) == 0 {
		t.Fatalf("模型清单为空: %s", rec.Body.String())
	}
	if len(resp.Data.Upstreams) != 1 || resp.Data.Upstreams[0].ModelID != "cline-free/deepseek-v4.1-flash" {
		t.Fatalf("upstreams = %+v", resp.Data.Upstreams)
	}
}

func TestAdminUpstreamsRejectsWrongMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	handleAdminUpstreams(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAdminUpstreamSavePersistsAndKeepsProbeCache(t *testing.T) {
	upstreamTestPool(t)
	// 预置一条带探测缓存的记录，验证保存不会把缓存清掉——
	// 否则「排除渠道」换算白名单会立刻失去依据。
	pool.PerModel = map[string]ModelUpstream{
		"m1": {Pipeline: pipelinePlanner, Available: []string{"alibaba", "baseten"}, ProbedAt: 123},
	}

	body := `{"modelId":"m1","upstreams":["alibaba"],"exclude":["baseten"],"pinMode":"preferred","aliases":["[cline]-fast"],"redirect":"vendor/real"}`
	rec := httptest.NewRecorder()
	handleAdminUpstreamSave(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/save", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	poolMu.Lock()
	got := pool.PerModel["m1"]
	poolMu.Unlock()
	if len(got.Upstreams) != 1 || got.Upstreams[0] != "alibaba" {
		t.Fatalf("upstreams = %v", got.Upstreams)
	}
	if got.PinMode != "preferred" || got.Redirect != "vendor/real" {
		t.Fatalf("pinMode/redirect = %q/%q", got.PinMode, got.Redirect)
	}
	if len(got.Aliases) != 1 || got.Aliases[0] != "[cline]-fast" {
		t.Fatalf("aliases = %v", got.Aliases)
	}
	if got.Pipeline != pipelinePlanner || len(got.Available) != 2 || got.ProbedAt != 123 {
		t.Fatalf("探测缓存被清掉了: %+v", got)
	}
}

func TestAdminUpstreamSaveValidates(t *testing.T) {
	upstreamTestPool(t)
	cases := []struct {
		name string
		body string
	}{
		{"缺 modelId", `{"upstreams":["alibaba"]}`},
		{"modelId 带引号", `{"modelId":"bad\"id","upstreams":[]}`},
		{"redirect 带反斜杠", `{"modelId":"m1","redirect":"a\\b"}`},
		{"非法 JSON", `{`},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		handleAdminUpstreamSave(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/save", strings.NewReader(c.body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body=%s)", c.name, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminUpstreamSaveNormalizesPinMode(t *testing.T) {
	upstreamTestPool(t)
	rec := httptest.NewRecorder()
	handleAdminUpstreamSave(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/save",
		strings.NewReader(`{"modelId":"m1","upstreams":[],"pinMode":"nonsense"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	poolMu.Lock()
	got := pool.PerModel["m1"].PinMode
	poolMu.Unlock()
	if got != "strict" {
		t.Fatalf("pinMode = %q, want strict", got)
	}
}

func TestAdminUpstreamDelete(t *testing.T) {
	upstreamTestPool(t)
	pool.PerModel = map[string]ModelUpstream{"m1": {Upstreams: []string{"alibaba"}}}

	rec := httptest.NewRecorder()
	handleAdminUpstreamDelete(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/delete", strings.NewReader(`{"modelId":"m1"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	poolMu.Lock()
	_, still := pool.PerModel["m1"]
	poolMu.Unlock()
	if still {
		t.Fatal("配置未被删除")
	}

	// 重复删除不报错（幂等），否则前端重试会看到假失败。
	rec = httptest.NewRecorder()
	handleAdminUpstreamDelete(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/delete", strings.NewReader(`{"modelId":"m1"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("重复删除 status = %d", rec.Code)
	}
}

func TestAdminUpstreamProbeRejectsBadInput(t *testing.T) {
	upstreamTestPool(t)
	rec := httptest.NewRecorder()
	handleAdminUpstreamProbe(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/probe", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 modelId status = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	handleAdminUpstreamProbe(rec, httptest.NewRequest(http.MethodGet, "/admin/api/upstreams/probe", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestUpstreamRoutesRegistered(t *testing.T) {
	// 路由必须挂在 /admin/api/ 下才会自动受 requireAdminAuth 保护。
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	for _, path := range []string{
		"/admin/api/upstreams",
		"/admin/api/upstreams/save",
		"/admin/api/upstreams/delete",
		"/admin/api/upstreams/probe",
	} {
		rec := httptest.NewRecorder()
		requireAdminAuth(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 未受后台鉴权保护，status = %d", path, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// 导出筛选与 token 脱敏
// ---------------------------------------------------------------------------

func TestAdminAccountExportFiltersByIDs(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{Accounts: []*Account{
		{AccountID: "acc_1", Email: "one@example.com", RefreshToken: "rt-1"},
		{AccountID: "acc_2", Email: "two@example.com", RefreshToken: "rt-2"},
	}}

	rec := httptest.NewRecorder()
	handleAdminAccountExport(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/export?ids=acc_2,,", nil))
	var got []accountTransfer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].RefreshToken != "rt-2" {
		t.Fatalf("按 ids 过滤失败: %+v", got)
	}

	// 不带 ids = 全部导出（默认行为不能变）。
	rec = httptest.NewRecorder()
	handleAdminAccountExport(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/export", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("默认应导出全部，得到 %d 条", len(got))
	}
}

func TestAdminAccountExportUnknownIDYieldsEmptyList(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{Accounts: []*Account{{AccountID: "acc_1", Email: "one@example.com", RefreshToken: "rt-1"}}}

	rec := httptest.NewRecorder()
	handleAdminAccountExport(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/export?ids=nope", nil))
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("期望空数组，得到 %q", rec.Body.String())
	}
}

func TestMaskCredential(t *testing.T) {
	if got := maskCredential(""); got != "" {
		t.Fatalf("空值应返回空，得到 %q", got)
	}
	long := maskCredential("PcYPh1BAIeervF5RFLJlLq3Rb")
	if strings.Contains(long, "IeervF5RFLJl") {
		t.Fatalf("中间部分未打码: %q", long)
	}
	if !strings.HasPrefix(long, "PcYPh1") || !strings.HasSuffix(long, "q3Rb") {
		t.Fatalf("首尾片段不对: %q", long)
	}
	// 短值必须整体打码，否则「前6后4」几乎等于明文。
	if got := maskCredential("short"); got != "*****" {
		t.Fatalf("短值应全部打码，得到 %q", got)
	}
}

func TestAccountDetailMasksTokensByDefault(t *testing.T) {
	upstreamTestPool(t)

	rec := httptest.NewRecorder()
	handleAdminAccountDetail(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/detail?accountId=acc_1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Data accountDetailData `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.RefreshToken != "" || resp.Data.AccessToken != "" {
		t.Fatal("默认响应不该带完整 token")
	}
	if resp.Data.RefreshTokenMasked == "" || resp.Data.AccessTokenMasked == "" {
		t.Fatal("应提供脱敏片段供辨认")
	}
	if strings.Contains(rec.Body.String(), "rt-secret-value") {
		t.Fatalf("响应泄露了 refreshToken: %s", rec.Body.String())
	}
}

func TestAccountDetailRevealReturnsTokens(t *testing.T) {
	upstreamTestPool(t)

	rec := httptest.NewRecorder()
	handleAdminAccountDetail(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/detail?accountId=acc_1&reveal=1", nil))
	var resp struct {
		Data accountDetailData `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.RefreshToken != "rt-secret-value" {
		t.Fatalf("reveal=1 应返回完整 refreshToken，得到 %q", resp.Data.RefreshToken)
	}
	if resp.Data.AccessToken != "workos:at-secret-value" {
		t.Fatalf("reveal=1 应返回完整 accessToken，得到 %q", resp.Data.AccessToken)
	}
}

// ---------------------------------------------------------------------------
// 代理链路：客户端偏好优先级的收口
// ---------------------------------------------------------------------------

func TestBuildUpstreamBodyAppliesPanelConfigAfterClientFields(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{
		DefaultModel: "cline-free/glm-5.2",
		PerModel: map[string]ModelUpstream{
			"panel-model": {Upstreams: []string{"alibaba"}},
		},
	}

	// 客户端也传了 provider，但不能覆盖面板配置——否则任何持有 API Key 的人
	// 都能改上游路由。
	params := map[string]any{
		"model":    "panel-model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"provider": map[string]any{"only": []any{"attacker-choice"}},
	}
	body := buildUpstreamBody(params, false)

	gw, ok := body["providerOptions"].(map[string]any)
	if !ok {
		t.Fatalf("面板配置未生效: %v", body)
	}
	only := gw["gateway"].(map[string]any)["only"].([]any)
	if len(only) != 1 || only[0] != "alibaba" {
		t.Fatalf("面板配置应覆盖客户端值，得到 %v", only)
	}
}

func TestBuildUpstreamBodyLeavesUnconfiguredModelsAlone(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{DefaultModel: "cline-free/glm-5.2"}

	params := map[string]any{
		"model":    "not-configured",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	body := buildUpstreamBody(params, false)
	if _, has := body["provider"]; has {
		t.Fatalf("未配置的模型不该被注入: %v", body)
	}
	if _, has := body["providerOptions"]; has {
		t.Fatalf("未配置的模型不该被注入: %v", body)
	}
	if body["model"] != "not-configured" {
		t.Fatalf("model = %v", body["model"])
	}
}

// ---------------------------------------------------------------------------
// 探测必须真的带上钉住配置
// ---------------------------------------------------------------------------

// TestProbeBaselineRequestCarriesPinnedUpstreams 守护一个真实缺陷：
// 探测的基线请求最初没带钉住配置，于是 providerMatch 反映的是「网关自己选了什么」，
// 而不是「我钉的渠道有没有生效」——用户据此会得到误导性的结论。
func TestProbeBaselineRequestCarriesPinnedUpstreams(t *testing.T) {
	upstreamTestPool(t)
	pool.PerModel = map[string]ModelUpstream{
		"m1": {Upstreams: []string{"alibaba"}, Pipeline: pipelinePlanner},
	}

	var captured []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured = append(captured, body)

		// 第一次（基线）回一个带 provider_metadata 的成功响应；第二次（枚举）回错误。
		if len(captured) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"choices":[{"message":{"content":"hi","provider_metadata":{"gateway":{"routing":{"finalProvider":"alibaba","canonicalSlug":"deepseek/x"}}}}}]}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Available providers are: alibaba, baseten"}`))
	}))
	defer server.Close()

	originalBase := clineAPIBase
	clineAPIBase = server.URL
	t.Cleanup(func() { clineAPIBase = originalBase })

	probe, err := probeModelUpstreams("m1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(captured) < 1 {
		t.Fatal("没有捕获到探测请求")
	}

	// 基线请求必须带上钉住配置，否则 providerMatch 毫无意义。
	gw, ok := captured[0]["providerOptions"].(map[string]any)
	if !ok {
		t.Fatalf("基线请求缺少 providerOptions，实际=%v", captured[0])
	}
	gateway, ok := gw["gateway"].(map[string]any)
	if !ok {
		t.Fatalf("基线请求缺少 providerOptions.gateway，实际=%v", gw)
	}
	only, ok := gateway["only"].([]any)
	if !ok || len(only) != 1 || only[0] != "alibaba" {
		t.Fatalf("基线请求未带上钉住的渠道，only=%v", gateway["only"])
	}

	if !probe.ProviderMatch {
		t.Fatalf("实际命中 alibaba 与钉住一致，providerMatch 应为 true；note=%q", probe.Note)
	}
	if len(probe.Available) != 2 {
		t.Fatalf("渠道枚举应得到 2 个，得到 %v", probe.Available)
	}
}

// TestProbeReportsPinNotTakingEffect 反向验证：实际命中的渠道与钉住不符时，
// 探测必须明确报告「未生效」，而不是把 providerMatch 留在 false 让人猜。
func TestProbeReportsPinNotTakingEffect(t *testing.T) {
	upstreamTestPool(t)
	pool.PerModel = map[string]ModelUpstream{
		"m1": {Upstreams: []string{"alibaba"}, Pipeline: pipelinePlanner},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"choices":[{"message":{"content":"hi","provider_metadata":{"gateway":{"routing":{"finalProvider":"fireworks"}}}}}]}}`))
	}))
	defer server.Close()

	originalBase := clineAPIBase
	clineAPIBase = server.URL
	t.Cleanup(func() { clineAPIBase = originalBase })

	probe, err := probeModelUpstreams("m1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.ProviderMatch {
		t.Fatal("命中 fireworks 而钉的是 alibaba，providerMatch 不该为 true")
	}
	if !strings.Contains(probe.Note, "未生效") {
		t.Fatalf("应明确报告钉住未生效，note=%q", probe.Note)
	}
}

// TestApplyUpstreamPrefsDropsClientSuppliedOtherKeys 守护一个真实缺陷：
// 早期实现把面板偏好「合并」进客户端的 provider/providerOptions，于是客户端
// 传的其它键（尤其 gateway.sort）会存活下来一起发往上游——而实测 gateway.sort
// 会让上游直接 500。面板配置存在时，这块应当整体覆盖。
func TestApplyUpstreamPrefsDropsClientSuppliedOtherKeys(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{PerModel: map[string]ModelUpstream{
		"m1": {Upstreams: []string{"alibaba"}, Pipeline: pipelinePlanner},
	}}

	body := map[string]any{
		"model": "m1",
		"providerOptions": map[string]any{
			"gateway": map[string]any{
				"only": []any{"attacker"},
				"sort": "cost",
			},
			"otherKey": "should-not-matter",
		},
		"provider": map[string]any{"only": []any{"attacker"}, "sort": "price"},
	}

	applyUpstreamPrefs(body, "m1")

	gateway := body["providerOptions"].(map[string]any)["gateway"].(map[string]any)
	if _, has := gateway["sort"]; has {
		t.Fatalf("客户端传的 sort 不该存活（实测会让上游 500），got %v", gateway)
	}
	only := gateway["only"].([]any)
	if len(only) != 1 || only[0] != "alibaba" {
		t.Fatalf("面板的 only 应整体覆盖客户端值，got %v", only)
	}

	// planner 管道已知时不该注入顶层 provider，客户端那一段也不该被顺手留下。
	if _, has := body["provider"]; has {
		// 允许客户端自带，但必须不是我们自己拼的合并结果；
		// 这里已知 pipeline=planner，所以实现不应触碰它。
		pr := body["provider"].(map[string]any)
		if len(pr) != 2 {
			t.Fatalf("实现不该改写客户端 provider，got %v", pr)
		}
	}
}

// TestApplyUpstreamPrefsUnknownPipelineOverridesBothForms 管道未知时两种形式都要
// 整体覆盖，不能一半覆盖一半合并。
func TestApplyUpstreamPrefsUnknownPipelineOverridesBothForms(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{PerModel: map[string]ModelUpstream{
		"m1": {Upstreams: []string{"deepinfra"}},
	}}

	body := map[string]any{
		"model":           "m1",
		"providerOptions": map[string]any{"gateway": map[string]any{"sort": "tps", "only": []any{"x"}}},
		"provider":        map[string]any{"sort": "price", "only": []any{"y"}},
	}
	applyUpstreamPrefs(body, "m1")

	gateway := body["providerOptions"].(map[string]any)["gateway"].(map[string]any)
	if _, has := gateway["sort"]; has {
		t.Fatalf("planner 形式应整体覆盖，got %v", gateway)
	}
	top := body["provider"].(map[string]any)
	if _, has := top["sort"]; has {
		t.Fatalf("direct 形式应整体覆盖，got %v", top)
	}
	if only := top["only"].([]any); len(only) != 1 || only[0] != "deepinfra" {
		t.Fatalf("direct only 应被面板覆盖，got %v", only)
	}
}
