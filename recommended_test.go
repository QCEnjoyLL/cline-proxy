package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 与线上结构一致的最小样本，含一个未知分组以验证兜底排序。
const sampleRecommendedJSON = `{
  "recommended": [
    {"id":"openai/gpt-6-astra","name":"gpt-6-astra","description":"","tags":["NEW"]},
    {"id":"moonshotai/kimi-k3","name":"kimi-k3","description":"Moonshot flagship","tags":["NEW"]}
  ],
  "free": [
    {"id":"cline-free/deepseek-v4.1-flash","name":"Deepseek-v4.1-Flash","description":"Fast","tags":[]}
  ],
  "clinePass": [
    {"id":"cline-pass/glm-5.2","name":"cline-pass/glm-5.2","description":"","tags":[]}
  ],
  "extraGroup": [
    {"id":"extra/one","name":"extra/one","description":"","tags":[]}
  ]
}`

func TestParseRecommendedModelsOrdersGroupsAndKeepsUnknown(t *testing.T) {
	groups, err := parseRecommendedModels([]byte(sampleRecommendedJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	wantOrder := []string{"recommended", "free", "clinePass", "extraGroup"}
	if len(groups) != len(wantOrder) {
		t.Fatalf("got %d groups, want %d", len(groups), len(wantOrder))
	}
	for i, want := range wantOrder {
		if groups[i].Key != want {
			t.Fatalf("group[%d] = %q, want %q", i, groups[i].Key, want)
		}
	}
	// clineCloud 在上游样本里缺失，不应凭空出现
	for _, g := range groups {
		if g.Key == "clineCloud" {
			t.Fatal("absent group clineCloud should not be synthesized")
		}
	}
	if groups[0].Models[0].ID != "openai/gpt-6-astra" {
		t.Fatalf("unexpected first model: %+v", groups[0].Models[0])
	}
	if groups[0].Models[0].Tags[0] != "NEW" {
		t.Fatalf("tags not decoded: %+v", groups[0].Models[0].Tags)
	}
}

// 中文翻译：命中映射时替换，未命中时保留原文，空描述不动。
func TestLocalizeDescription(t *testing.T) {
	// 命中映射
	if got := localizeDescription("Top open weights model"); got != "顶尖开源权重模型" {
		t.Fatalf("命中映射未翻译: %q", got)
	}
	// 首尾空白也应能命中
	if got := localizeDescription("  Top open weights model \n"); got != "顶尖开源权重模型" {
		t.Fatalf("带空白未翻译: %q", got)
	}
	// 标点差异（上游同一描述在两组中一个有句点一个没有）都必须命中
	for _, variant := range []string{
		"Latest natively multimodal model in the GLM-5 series",
		"Latest natively multimodal model in the GLM-5 series.",
	} {
		if got := localizeDescription(variant); got != "GLM-5 系列中最新的原生多模态模型" {
			t.Fatalf("标点差异未翻译 %q: %q", variant, got)
		}
	}
	// 未命中：保留原文，不丢内容
	const upstream = "Some brand new description from upstream"
	if got := localizeDescription(upstream); got != upstream {
		t.Fatalf("未命中的描述应保留原文, got %q", got)
	}
	// 空描述保持为空
	if got := localizeDescription("   "); got != "   " {
		t.Fatalf("空描述不应被改动, got %q", got)
	}
}

// 解析阶段必须应用翻译，否则前端拿到的仍是英文。
func TestParseRecommendedModelsAppliesChineseDescriptions(t *testing.T) {
	body := []byte(`{
	  "free": [
	    {"id":"a","name":"a","description":"Top open weights model","tags":[]},
	    {"id":"b","name":"b","description":"Unknown en text","tags":[]}
	  ]
	}`)
	groups, err := parseRecommendedModels(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Models) != 2 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
	if got := groups[0].Models[0].Description; got != "顶尖开源权重模型" {
		t.Fatalf("已映射的描述未翻译: %q", got)
	}
	if got := groups[0].Models[1].Description; got != "Unknown en text" {
		t.Fatalf("未映射的描述应保留原文: %q", got)
	}
}

// 翻译表必须完整覆盖线上描述，避免漏翻。
func TestDescriptionZhCoversKnownUpstreamText(t *testing.T) {
	known := []string{
		"Kimi K3 is Moonshot AI’s new flagship MoE model for agentic coding",
		"SpaceXAI's smartest model with frontier performance on coding",
		"Fast and efficient with 1M context window",
		"Meta’s multimodal reasoning model for experimentation, learning, and early-stage agentic, multi-agent, and coding workflows.",
		"Latest natively multimodal model in the GLM-5 series.",
		"Strong model for office productivity, document-intensive work, and coding.",
		"Latest coding agent model from Poolside",
		"Leading open weights model (reliability might be unstable and will consume usage faster than others)",
		"Top open weights model",
		"Qwen's New SOTA coding model",
		"Frontier reasoning and coding with 1M context window",
		"Smarter and more efficient, with 1M context window",
		"Strong multimodal model for long-horizon agent tasks",
		"Flagship agent model with 1M context window",
		"Frontier coding and agent model with 1M context window",
		"Latest Kimi model specialized for agentic coding",
		"Z-AI's new top open-weights model",
		"Fast multimodal agent model with vision and video input",
		"Top open model for long autonomous coding runs",
		"Fast and efficient MiMo for everyday coding",
	}
	for _, en := range known {
		zh, ok := descriptionZh[en]
		if !ok {
			t.Errorf("线上描述缺少翻译: %q", en)
			continue
		}
		if zh == "" || zh == en {
			t.Errorf("翻译为空或未变更: %q", en)
		}
		// 翻译结果应当是中文（至少含一个中日韩统一表意文字）
		hasHan := false
		for _, r := range zh {
			if r >= 0x4E00 && r <= 0x9FFF {
				hasHan = true
				break
			}
		}
		if !hasHan {
			t.Errorf("译文不含中文: %q -> %q", en, zh)
		}
	}
}

func TestParseRecommendedModelsRejectsBadInput(t *testing.T) {
	if _, err := parseRecommendedModels([]byte("not json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if _, err := parseRecommendedModels([]byte(`{"free":[]}`)); err == nil {
		t.Fatal("expected error for empty model list")
	}
}

// fetchRecommendedModels 必须能处理上游的真实响应格式；
// 这里用本地 httptest 服务替身不方便（URL 是常量），因此直接验证网络路径失败时
// recommendedSnapshot 会退回过期缓存而不是清空数据。
func TestRecommendedSnapshotFallsBackToStaleCache(t *testing.T) {
	recommendedMu.Lock()
	originalCache, originalAt := recommendedCache, recommendedAt
	recommendedMu.Unlock()
	t.Cleanup(func() {
		recommendedMu.Lock()
		recommendedCache, recommendedAt = originalCache, originalAt
		recommendedMu.Unlock()
	})

	// 预置一份"过期"缓存，并让 TTL 视为已过期
	seeded := []modelGroup{{Key: "free", Models: []remoteModel{{ID: "cline-free/x"}}}}
	recommendedMu.Lock()
	recommendedCache = seeded
	recommendedAt = time.Now().Add(-2 * recommendedCacheTTL)
	recommendedMu.Unlock()

	// 用一个必然失败的地址回源，观察是否退回缓存
	recommendedMu.Lock()
	recommendedCache, recommendedAt = seeded, time.Now().Add(-2*recommendedCacheTTL)
	recommendedMu.Unlock()

	res := recommendedSnapshotFunc(true, func() ([]modelGroup, error) {
		return nil, errFakeUpstream
	})

	if len(res.Groups) != 1 || res.Groups[0].Models[0].ID != "cline-free/x" {
		t.Fatalf("stale cache not returned: %+v", res.Groups)
	}
	if !res.Stale || res.Err == "" {
		t.Fatalf("expected stale flag and error, got stale=%v err=%q", res.Stale, res.Err)
	}
}

func TestRecommendedSnapshotNoCacheNoData(t *testing.T) {
	recommendedMu.Lock()
	originalCache, originalAt := recommendedCache, recommendedAt
	recommendedCache, recommendedAt = nil, time.Time{}
	recommendedMu.Unlock()
	t.Cleanup(func() {
		recommendedMu.Lock()
		recommendedCache, recommendedAt = originalCache, originalAt
		recommendedMu.Unlock()
	})

	res := recommendedSnapshotFunc(true, func() ([]modelGroup, error) {
		return nil, errFakeUpstream
	})
	if len(res.Groups) != 0 {
		t.Fatalf("expected no data, got %+v", res.Groups)
	}
	if res.Err == "" {
		t.Fatal("expected error to be reported")
	}
}

func TestAdminModelsBatchAddSkipsDuplicates(t *testing.T) {
	useTemporaryPool(t)

	addRequest := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handleAdminModelsBatchAdd(rec, httptest.NewRequest(http.MethodPost,
			"/admin/api/models/batch", strings.NewReader(body)))
		return rec
	}

	res := addRequest(`{"ids":["provider/a","provider/b"]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", res.Code, http.StatusOK, res.Body.String())
	}

	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Added   []string          `json:"added"`
			Skipped []string          `json:"skipped"`
			Failed  map[string]string `json:"failed"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data.Added) != 2 || len(body.Data.Skipped) != 0 || len(body.Data.Failed) != 0 {
		t.Fatalf("unexpected batch result: %+v", body.Data)
	}

	// 重复提交同一批：应全部进入 skipped，而不是报错
	res2 := addRequest(`{"ids":["provider/a","provider/b"]}`)
	if res2.Code != http.StatusOK {
		t.Fatalf("duplicate batch status = %d, want %d", res2.Code, http.StatusOK)
	}
	var body2 struct {
		Data struct {
			Added   []string `json:"added"`
			Skipped []string `json:"skipped"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res2.Body).Decode(&body2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body2.Data.Added) != 0 || len(body2.Data.Skipped) != 2 {
		t.Fatalf("duplicates were not skipped: %+v", body2.Data)
	}

	// 确认没有产生重复条目
	count := 0
	for _, m := range allModels() {
		if m.ID == "provider/a" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("provider/a appears %d times, want 1", count)
	}
}

func TestAdminModelsBatchAddRejectsBadRequests(t *testing.T) {
	useTemporaryPool(t)

	for name, tc := range map[string]struct {
		method string
		body   string
		want   int
	}{
		"wrong method": {http.MethodGet, `{"ids":["a"]}`, http.StatusMethodNotAllowed},
		"invalid json": {http.MethodPost, `{`, http.StatusBadRequest},
		"empty ids":    {http.MethodPost, `{"ids":[]}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleAdminModelsBatchAdd(rec, httptest.NewRequest(tc.method,
				"/admin/api/models/batch", strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestAdminRecommendedModelsHandlerMethodAndShape(t *testing.T) {
	recommendedMu.Lock()
	originalCache, originalAt := recommendedCache, recommendedAt
	recommendedCache = []modelGroup{{Key: "free", Models: []remoteModel{{ID: "cline-free/x"}}}}
	recommendedAt = time.Now()
	recommendedMu.Unlock()
	t.Cleanup(func() {
		recommendedMu.Lock()
		recommendedCache, recommendedAt = originalCache, originalAt
		recommendedMu.Unlock()
	})

	rec := httptest.NewRecorder()
	handleAdminRecommendedModels(rec, httptest.NewRequest(http.MethodPost, "/admin/api/recommended-models", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}

	// 命中新鲜缓存：不应回源，直接返回数据
	rec2 := httptest.NewRecorder()
	handleAdminRecommendedModels(rec2, httptest.NewRequest(http.MethodGet, "/admin/api/recommended-models", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", rec2.Code, http.StatusOK, rec2.Body.String())
	}

	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Groups    []modelGroup `json:"groups"`
			Installed []string     `json:"installed"`
			FetchedAt int64        `json:"fetchedAt"`
			Cached    bool         `json:"cached"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Success || len(body.Data.Groups) != 1 {
		t.Fatalf("unexpected payload: %+v", body)
	}
	if body.Data.FetchedAt == 0 {
		t.Fatal("fetchedAt should be populated from cache time")
	}
	if !body.Data.Cached {
		t.Fatal("expected cached=true when serving from fresh cache")
	}
	if body.Data.Installed == nil {
		t.Fatal("installed should be an empty slice, not null")
	}
}

// 回归测试：回源抓取期间不得持有缓存锁，否则并发请求会被最长 15 秒地串行阻塞。
func TestRecommendedSnapshotDoesNotBlockReadersDuringFetch(t *testing.T) {
	recommendedMu.Lock()
	originalCache, originalAt := recommendedCache, recommendedAt
	recommendedMu.Unlock()
	t.Cleanup(func() {
		recommendedMu.Lock()
		recommendedCache, recommendedAt = originalCache, originalAt
		recommendedMu.Unlock()
	})

	// 预置过期缓存，使 force=true 必然回源
	recommendedMu.Lock()
	recommendedCache = []modelGroup{{Key: "free", Models: []remoteModel{{ID: "seeded"}}}}
	recommendedAt = time.Now().Add(-2 * recommendedCacheTTL)
	recommendedMu.Unlock()

	// 让上游抓取阻塞住，模拟慢网络
	release := make(chan struct{})
	started := make(chan struct{})
	fetchDone := make(chan struct{})
	go func() {
		defer close(fetchDone)
		recommendedSnapshotFunc(true, func() ([]modelGroup, error) {
			close(started)
			<-release
			return []modelGroup{{Key: "fresh", Models: []remoteModel{{ID: "fresh"}}}}, nil
		})
	}()
	<-started

	// 抓取仍在进行时，另一处必须能立刻拿到缓存锁（若抓取持锁则会阻塞）。
	acquired := make(chan struct{})
	go func() {
		recommendedMu.Lock()
		recommendedMu.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
		// 正常：锁未被抓取过程长期占用
	case <-time.After(2 * time.Second):
		close(release)
		<-fetchDone
		t.Fatal("回源抓取期间持有了 recommendedMu，会阻塞并发请求")
	}

	close(release)
	<-fetchDone
}
