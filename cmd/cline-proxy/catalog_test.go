package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func resetCatalogCache() {
	catalogMu.Lock()
	catalogCache = nil
	catalogAt = time.Time{}
	catalogMu.Unlock()
}

// sampleCatalogJSON 抄自 https://api.cline.bot/api/v1/ai/cline/models 的真实响应片段，
// 保留那些我们**不**需要的字段（pricing / architecture / supported_parameters…），
// 用来确认解析时能正确忽略它们，也把上游结构固定成文档。
const sampleCatalogJSON = `{
  "data": [
    {
      "id": "stealth/union-alpha",
      "canonical_slug": "stealth/union-alpha",
      "hugging_face_id": "",
      "name": "Union Alpha",
      "created": 1789569723,
      "description": "Union Alpha is a multimodal model built for research, coding, and agentic workflows.",
      "context_length": 262144,
      "architecture": {"modality": "text+image->text", "input_modalities": ["text", "image"]},
      "pricing": {"prompt": "0", "completion": "0"},
      "supported_parameters": ["max_tokens", "temperature", "tools"],
      "expiration_date": "2098-12-31"
    },
    {
      "id": "~deepseek/deepseek-pro-latest",
      "canonical_slug": "~deepseek/deepseek-pro-latest",
      "name": "DeepSeek: DeepSeek Pro Latest",
      "description": "This model always redirects to the latest model in the DeepSeek Pro family.",
      "context_length": 1048576
    }
  ]
}`

func TestParseCatalogModelsReadsUpstreamShape(t *testing.T) {
	models, err := parseCatalogModels([]byte(sampleCatalogJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].ID != "stealth/union-alpha" || models[0].Name != "Union Alpha" {
		t.Errorf("第一个模型解析错误: %+v", models[0])
	}
	// 描述必须原样保留（与推荐清单一样：不做任何翻译）。
	const want = "Union Alpha is a multimodal model built for research, coding, and agentic workflows."
	if models[0].Description != want {
		t.Errorf("描述被改动: %q", models[0].Description)
	}
	// id 里带 ~ 前缀的别名也要照原样保留，不做任何裁剪。
	if models[1].ID != "~deepseek/deepseek-pro-latest" {
		t.Errorf("带 ~ 前缀的 id 被改动了: %q", models[1].ID)
	}
	// 上游存在的其它字段不进我们的结构体：这样转发给面板的数据量能降一个数量级。
	raw, _ := json.Marshal(models[0])
	for _, unwanted := range []string{"pricing", "architecture", "supported_parameters", "context_length"} {
		if strings.Contains(string(raw), unwanted) {
			t.Errorf("不该把上游的 %s 转发给面板: %s", unwanted, raw)
		}
	}
}

func TestParseCatalogModelsRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"非 JSON":     "not json",
		"空 data":     `{"data":[]}`,
		"没有 data 字段": `{}`,
		"data 类型不对":  `{"data":"nope"}`,
		"数组顶层":       `[{"id":"a"}]`,
	}
	for name, body := range cases {
		if _, err := parseCatalogModels([]byte(body)); err == nil {
			t.Errorf("%s: 应当报错，实际成功了", name)
		}
	}
}

// 取数语义：命中缓存不回源、force 回源、回源失败退回过期缓存。
func TestCatalogSnapshotCachingAndStaleFallback(t *testing.T) {
	t.Run("命中缓存不回源", func(t *testing.T) {
		resetCatalogCache()
		t.Cleanup(resetCatalogCache)

		calls := 0
		fetch := func() ([]remoteModel, error) {
			calls++
			return []remoteModel{{ID: "a"}}, nil
		}

		first := catalogSnapshotWith(false, fetch)
		if first.Cached || len(first.Models) != 1 {
			t.Fatalf("首次应回源: %+v", first)
		}
		second := catalogSnapshotWith(false, fetch)
		if !second.Cached {
			t.Errorf("第二次应命中缓存: %+v", second)
		}
		if calls != 1 {
			t.Errorf("回源次数 = %d, want 1（第二次不该再打上游）", calls)
		}
	})

	t.Run("force 会回源", func(t *testing.T) {
		resetCatalogCache()
		t.Cleanup(resetCatalogCache)

		calls := 0
		fetch := func() ([]remoteModel, error) {
			calls++
			return []remoteModel{{ID: "a"}}, nil
		}
		catalogSnapshotWith(false, fetch)
		catalogSnapshotWith(true, fetch)
		if calls != 2 {
			t.Errorf("force 应再回源一次，实际回源 %d 次", calls)
		}
	})

	t.Run("回源失败退回过期缓存", func(t *testing.T) {
		resetCatalogCache()
		t.Cleanup(resetCatalogCache)

		seed := []remoteModel{{ID: "cached"}}
		catalogMu.Lock()
		catalogCache = seed
		catalogAt = time.Now().Add(-2 * catalogCacheTTL) // 已过期，必须回源
		catalogMu.Unlock()

		res := catalogSnapshotWith(false, func() ([]remoteModel, error) {
			return nil, errors.New("upstream down")
		})
		if !res.Stale || !res.Cached {
			t.Errorf("应为「退回旧的缓存」: %+v", res)
		}
		if len(res.Models) != 1 || res.Models[0].ID != "cached" {
			t.Errorf("应保留旧数据: %+v", res.Models)
		}
		if res.Err == "" {
			t.Error("应带上回源失败原因")
		}
	})

	t.Run("无缓存且回源失败返回空", func(t *testing.T) {
		resetCatalogCache()
		t.Cleanup(resetCatalogCache)

		res := catalogSnapshotWith(true, func() ([]remoteModel, error) {
			return nil, errors.New("upstream down")
		})
		if len(res.Models) != 0 || res.Err == "" {
			t.Errorf("应为空 + 错误: %+v", res)
		}
	})
	t.Run("抓取期间缓存被并发刷新则采用更新的那份", func(t *testing.T) {
		resetCatalogCache()
		t.Cleanup(resetCatalogCache)

		// 预置一份过期缓存，迫使走回源分支（此时 at 是「两小时前」）
		catalogMu.Lock()
		catalogCache = []remoteModel{{ID: "old"}}
		catalogAt = time.Now().Add(-2 * catalogCacheTTL)
		catalogMu.Unlock()

		// 在 fetch 执行期间（锁外）模拟另一个并发请求把缓存刷新了
		res := catalogSnapshotWith(false, func() ([]remoteModel, error) {
			catalogMu.Lock()
			catalogCache = []remoteModel{{ID: "newer"}}
			catalogAt = time.Now()
			catalogMu.Unlock()
			return []remoteModel{{ID: "ours"}}, nil
		})

		if len(res.Models) != 1 || res.Models[0].ID != "newer" || !res.Cached {
			t.Errorf("应采用并发刷新的更新结果: %+v", res)
		}
		if len(catalogCache) != 1 || catalogCache[0].ID != "newer" {
			t.Errorf("过期结果不该覆盖更新的缓存: %+v", catalogCache)
		}
	})

}

// handler 的形状：GET 返回 data.models / installed / fetchedAt，非 GET 拒绝。
//
// 这里预置缓存，让非 force 的请求直接命中缓存——测试不打真实网络。
func TestAdminModelCatalogHandlerShape(t *testing.T) {
	resetCatalogCache()
	t.Cleanup(resetCatalogCache)

	catalogMu.Lock()
	catalogCache = []remoteModel{{ID: "vendor/one", Name: "One", Description: "desc"}}
	catalogAt = time.Now()
	catalogMu.Unlock()

	rec := httptest.NewRecorder()
	handleAdminModelCatalog(rec, httptest.NewRequest(http.MethodGet, "/admin/api/model-catalog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200；body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Models    []remoteModel `json:"models"`
			Installed []string      `json:"installed"`
			FetchedAt int64         `json:"fetchedAt"`
			Cached    bool          `json:"cached"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	if !body.Success || len(body.Data.Models) != 1 || body.Data.Models[0].ID != "vendor/one" {
		t.Errorf("响应内容不对: %s", rec.Body.String())
	}
	if body.Data.FetchedAt == 0 {
		t.Error("fetchedAt 不应为 0")
	}
	if !body.Data.Cached {
		t.Error("命中缓存时 cached 应为 true")
	}
	// installed 至少要包含内置模型，否则前端无从判断「已启用」。
	if len(body.Data.Installed) == 0 {
		t.Error("installed 不应为空")
	}

	// 只允许 GET。
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		rec := httptest.NewRecorder()
		handleAdminModelCatalog(rec, httptest.NewRequest(method, "/admin/api/model-catalog", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

// 回归测试：回源抓取期间不得持有缓存锁，否则并发请求会被最长 30 秒地串行阻塞。
func TestCatalogSnapshotDoesNotBlockReadersDuringFetch(t *testing.T) {
	catalogMu.Lock()
	originalCache, originalAt := catalogCache, catalogAt
	catalogMu.Unlock()
	t.Cleanup(func() {
		catalogMu.Lock()
		catalogCache, catalogAt = originalCache, originalAt
		catalogMu.Unlock()
	})

	// 预置过期缓存，使回源必然发生
	catalogMu.Lock()
	catalogCache = []remoteModel{{ID: "seeded"}}
	catalogAt = time.Now().Add(-2 * catalogCacheTTL)
	catalogMu.Unlock()

	// 让抓取阻塞住，模拟慢网络
	release := make(chan struct{})
	started := make(chan struct{})
	fetchDone := make(chan struct{})
	go func() {
		defer close(fetchDone)
		catalogSnapshotWith(false, func() ([]remoteModel, error) {
			close(started)
			<-release
			return []remoteModel{{ID: "fresh"}}, nil
		})
	}()
	<-started

	// 抓取仍在进行时，另一处必须能立刻拿到缓存锁（若抓取持锁则会阻塞）
	acquired := make(chan struct{})
	go func() {
		catalogMu.Lock()
		catalogMu.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
		// 正常：锁没有被抓取过程占着
	case <-time.After(2 * time.Second):
		t.Error("回源期间缓存锁被持有，并发请求会被阻塞")
	}

	close(release)
	<-fetchDone
}

// 无缓存且回源失败时必须回 502，让面板能区分「上游挂了」与「上游返回空」。
// 这里替换 catalogFetch 而不是预置缓存：这条路径本身就是「没有缓存可用」。
func TestAdminModelCatalogNoCacheNoDataReturns502(t *testing.T) {
	resetCatalogCache()
	t.Cleanup(resetCatalogCache)

	original := catalogFetch
	catalogFetch = func() ([]remoteModel, error) { return nil, errors.New("upstream down") }
	t.Cleanup(func() { catalogFetch = original })

	rec := httptest.NewRecorder()
	handleAdminModelCatalog(rec, httptest.NewRequest(http.MethodGet, "/admin/api/model-catalog", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502；body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	if body.Success {
		t.Error("回源失败时 success 应为 false")
	}
	if body.Error == "" {
		t.Error("应带上回源失败原因")
	}
}
