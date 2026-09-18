package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// 描述必须原样透传上游的英文原文——不再做任何翻译。
//
// 背景：之前这里有一张「英文 → 中文」对照表，但上游新增或改写过的描述命中不了，
// 界面里就变成中英混杂——新增的模型永远显示英文。这条测试锁住「不再翻译」这个决定，
// 避免有人又把对照表加回来，也顺带保证描述内容本身不会被改动。
func TestDescriptionsPassThroughUntranslated(t *testing.T) {
	const newUpstream = "Some brand new description from upstream"
	body := []byte(`{
	  "free": [
	    {"id":"a","name":"a","description":"Top open weights model","tags":[]},
	    {"id":"b","name":"b","description":"` + newUpstream + `","tags":[]}
	  ]
	}`)

	groups, err := parseRecommendedModels(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Models) != 2 {
		t.Fatalf("unexpected groups: %+v", groups)
	}

	// 这句以前会被翻成「顶尖开源权重模型」；现在必须保持英文原文。
	if got := groups[0].Models[0].Description; got != "Top open weights model" {
		t.Errorf("描述被改动了（不应再做翻译）: %q", got)
	}
	// 上游新增的描述本来就命不中对照表，现在也必须原样透传。
	if got := groups[0].Models[1].Description; got != newUpstream {
		t.Errorf("描述未原样透传: %q", got)
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

// 批量添加的核心承诺：整批只落盘一次。
//
// 这条不是锦上添花——原先逐个调用 addCustomModel 时，一个 96 个模型的分组会在持有 poolMu
// 期间做 96 次整池写盘，而 pickAccount（每个代理请求都走）也要这把锁。测试通过统计
// savePoolLocked 的调用次数来钉住这个行为。
func TestAddCustomModelsPersistsOnceForWholeBatch(t *testing.T) {
	useTemporaryPool(t)

	orig := countSavePoolLocked
	countSavePoolLocked = true
	t.Cleanup(func() { countSavePoolLocked = orig; poolSaveCount.Store(0) })
	poolSaveCount.Store(0)

	ids := make([]string, 0, 120)
	for i := 0; i < 120; i++ {
		ids = append(ids, fmt.Sprintf("vendor/model-%d", i))
	}

	added, skipped, failed := addCustomModels(ids)
	if len(added) != 120 || len(skipped) != 0 || len(failed) != 0 {
		t.Fatalf("批量结果不对: added=%d skipped=%d failed=%d", len(added), len(skipped), len(failed))
	}
	if got := poolSaveCount.Load(); got != 1 {
		t.Fatalf("整批落盘次数 = %d, want 1（逐个落盘会让每个代理请求排队等磁盘）", got)
	}

	// 再次提交同一批：全部 skipped，且一个都没变
	poolSaveCount.Store(0)
	added2, skipped2, failed2 := addCustomModels(ids)
	if len(added2) != 0 || len(skipped2) != 120 || len(failed2) != 0 {
		t.Fatalf("重复提交结果不对: added=%d skipped=%d failed=%d", len(added2), len(skipped2), len(failed2))
	}
	if got := poolSaveCount.Load(); got != 0 {
		t.Fatalf("无事可做时不应落盘，实际 %d 次", got)
	}

	// 落盘后的内容必须真的写进了账号池文件（重启后仍在）
	pool := loadPool()
	poolMu.Lock()
	got := len(pool.CustomModels)
	poolMu.Unlock()
	if got != 120 {
		t.Fatalf("CustomModels = %d, want 120", got)
	}
}

// 批量添加的边界：批内重复、非法 id、内置模型重新启用、以及体积上限。
func TestAddCustomModelsEdgeCases(t *testing.T) {
	useTemporaryPool(t)

	t.Run("批内重复按已存在跳过", func(t *testing.T) {
		added, skipped, failed := addCustomModels([]string{"vendor/dup", "vendor/dup", "vendor/dup"})
		if len(added) != 1 || len(skipped) != 2 || len(failed) != 0 {
			t.Fatalf("added=%v skipped=%v failed=%v", added, skipped, failed)
		}
	})

	t.Run("非法 id 进 failed 且不影响同批其它项", func(t *testing.T) {
		added, _, failed := addCustomModels([]string{"vendor/ok", "bad|pipe"})
		if len(failed) != 1 || len(added) != 1 || added[0] != "vendor/ok" {
			t.Fatalf("added=%v failed=%v", added, failed)
		}
	})

	t.Run("内置模型已启用时计入 skipped", func(t *testing.T) {
		added, skipped, failed := addCustomModels([]string{defaultModel})
		if len(added) != 0 || len(skipped) != 1 || len(failed) != 0 {
			t.Fatalf("added=%v skipped=%v failed=%v", added, skipped, failed)
		}
	})

	t.Run("删除过的内置模型可批量重新启用", func(t *testing.T) {
		useTemporaryPool(t)
		if err := deleteCustomModel(defaultModel); err != nil {
			t.Fatalf("先删除内置模型: %v", err)
		}
		added, skipped, failed := addCustomModels([]string{defaultModel, "vendor/x"})
		if len(added) != 2 || len(skipped) != 0 || len(failed) != 0 {
			t.Fatalf("added=%v skipped=%v failed=%v", added, skipped, failed)
		}
		p := loadPool()
		poolMu.Lock()
		disabled := modelDisabledLocked(p, defaultModel)
		poolMu.Unlock()
		if disabled {
			t.Error("批量添加后内置模型仍处于禁用状态")
		}
	})

	t.Run("空批次不做任何事", func(t *testing.T) {
		added, skipped, failed := addCustomModels(nil)
		if len(added) != 0 || len(skipped) != 0 || len(failed) != 0 {
			t.Fatalf("added=%v skipped=%v failed=%v", added, skipped, failed)
		}
	})
}

// 批量接口的条数上限：给出异常巨大的数组时必须拒绝，而不是照单全收。
func TestAdminModelsBatchAddRejectsOversizedBatch(t *testing.T) {
	useTemporaryPool(t)

	ids := make([]string, maxBatchModelIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("vendor/m-%d", i)
	}
	payload, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := httptest.NewRecorder()
	handleAdminModelsBatchAdd(rec, httptest.NewRequest(http.MethodPost,
		"/admin/api/models/batch", bytes.NewReader(payload)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// 刚好等于上限应放行
	ids = ids[:maxBatchModelIDs]
	payload, _ = json.Marshal(map[string]any{"ids": ids})
	rec = httptest.NewRecorder()
	handleAdminModelsBatchAdd(rec, httptest.NewRequest(http.MethodPost,
		"/admin/api/models/batch", bytes.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("上限内应放行: status = %d; body=%s", rec.Code, rec.Body.String())
	}
}

// handler 这一层也不能退化成逐个落盘：直接测 addCustomModels 只能证明那个函数是对的，
// 挡不住「handler 改回循环调用 addCustomModel」。
func TestAdminModelsBatchAddHandlerPersistsOnce(t *testing.T) {
	useTemporaryPool(t)

	orig := countSavePoolLocked
	countSavePoolLocked = true
	t.Cleanup(func() { countSavePoolLocked = orig; poolSaveCount.Store(0) })
	poolSaveCount.Store(0)

	ids := make([]string, 0, 90)
	for i := 0; i < 90; i++ {
		ids = append(ids, fmt.Sprintf("vendor/h-%d", i))
	}
	payload, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := httptest.NewRecorder()
	handleAdminModelsBatchAdd(rec, httptest.NewRequest(http.MethodPost,
		"/admin/api/models/batch", bytes.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data struct {
			Added []string `json:"added"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data.Added) != 90 {
		t.Fatalf("added = %d, want 90", len(body.Data.Added))
	}
	if got := poolSaveCount.Load(); got != 1 {
		t.Fatalf("handler 批量添加落盘 %d 次, want 1", got)
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
