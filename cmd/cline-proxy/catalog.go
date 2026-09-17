package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// catalogModelsURL 是 Cline 的「全部模型」清单接口。
//
// 与 recommendedModelsURL 的区别：那个返回按用途分好的几组（推荐 / free / Pass /
// Cloud，几十条），这个返回上游全部可选模型的平铺列表（实测 400+ 条、约 500 KB）。
// 两个接口都只对 cline 自家域名放行 CORS，所以统一由本进程抓取后转发给管理面板。
const catalogModelsURL = "https://api.cline.bot/api/v1/ai/cline/models"

// catalogCacheTTL 与推荐清单取同一个值：同一个上游、同一类使用节奏，没有理由不同。
const catalogCacheTTL = 30 * time.Minute

// catalogFetchTimeout 比推荐清单的 15 秒宽一倍。
//
// 那份只有几十 KB，这里是约 500 KB 的响应体，io.ReadAll 必须在时限内读完；而超时是硬
// 失败，面板只能报错，代价比多等几秒大得多。下面还有 4 MiB 的读取上限兜底，放宽时限
// 不会让内存无界增长。
const catalogFetchTimeout = 30 * time.Second

// catalogResponse 是该接口的响应外壳：{"data":[{...}]}。
//
// 单个条目里还有 pricing / architecture / supported_parameters 等一大堆字段。这里复用的是
// 与推荐清单共用的 remoteModel，它除了 id / name / description 还带一个 tags（推荐清单拿它
// 渲染「NEW」角标）；这份上游数据里 tags 恒为空，会序列化成 "tags":null，444 条加起来约
// 5 KB，不值得为省这点流量单独定义一个结构体。其余字段在解析时自动忽略——完整响应约
// 500 KB，只保留这几个字段已经能把面板拿到的数据量降一个数量级。
type catalogResponse struct {
	Data []remoteModel `json:"data"`
}

// parseCatalogModels 把上游 JSON 解析成模型清单。
// 单独抽出来是为了能在不联网的情况下做单元测试。
func parseCatalogModels(body []byte) ([]remoteModel, error) {
	var raw catalogResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析上游 JSON: %w", err)
	}
	if len(raw.Data) == 0 {
		return nil, errors.New("上游返回的模型列表为空")
	}
	return raw.Data, nil
}

// fetchCatalogModels 抓取「全部模型」清单。
//
// 与 fetchRecommendedModels 是两条平行的抓取（不同接口、不同解析），刻意不合并成
// 一个通用函数：那条路径已经被测试覆盖，为省几行代码去改动它的收益不抵风险。
func fetchCatalogModels() ([]remoteModel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogModelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cline-go-proxy")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	// 限制读取量：上游正常返回约 500 KB，4 MiB 足够，也挡住异常的超大响应。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	return parseCatalogModels(body)
}

var (
	catalogMu    sync.Mutex
	catalogCache []remoteModel
	catalogAt    time.Time
)

// catalogResult 描述一次取数结果，供 handler 原样转发给前端。
type catalogResult struct {
	Models    []remoteModel
	FetchedAt time.Time
	Cached    bool   // 直接命中缓存，未回源
	Stale     bool   // 回源失败，退回了过期缓存
	Err       string // 非致命错误（回源失败原因）
}

// catalogFetch 是回源实现，抽成变量是为了让测试能注入失败（不打真实网络）。
// 生产代码只在包初始化时读它，没有其它写入点。
var catalogFetch = fetchCatalogModels

// catalogSnapshot 返回当前模型清单，回源抓取走真实网络。
func catalogSnapshot(force bool) catalogResult {
	return catalogSnapshotWith(force, catalogFetch)
}

// catalogSnapshotWith 是取数的共用实现，fetch 可注入以便测试。
//
// 语义与 recommendedSnapshotWith 完全一致（那里有更详细的说明）：网络请求放在锁外，
// 回源失败时退回过期缓存而不是让面板空白，并在提交前复核缓存是否已被并发请求刷新。
func catalogSnapshotWith(force bool, fetch func() ([]remoteModel, error)) catalogResult {
	catalogMu.Lock()
	cache, at := catalogCache, catalogAt
	catalogMu.Unlock()

	fresh := cache != nil && time.Since(at) < catalogCacheTTL
	if !force && fresh {
		return catalogResult{Models: cache, FetchedAt: at, Cached: true}
	}

	models, err := fetch()

	catalogMu.Lock()
	defer catalogMu.Unlock()

	if err != nil {
		if catalogCache != nil {
			return catalogResult{
				Models:    catalogCache,
				FetchedAt: catalogAt,
				Cached:    true,
				Stale:     true,
				Err:       err.Error(),
			}
		}
		return catalogResult{Err: err.Error()}
	}

	// 若在我们抓取期间缓存已被刷新得更新，则采用更新的那份，避免覆盖。
	if catalogCache != nil && catalogAt.After(at) && !force {
		return catalogResult{Models: catalogCache, FetchedAt: catalogAt, Cached: true}
	}

	catalogCache = models
	catalogAt = time.Now()
	return catalogResult{Models: catalogCache, FetchedAt: catalogAt}
}

// GET /admin/api/model-catalog
//
// 查询参数 refresh=1 表示强制回源刷新（忽略缓存）。
// 面板只在用户展开「全部模型」折叠块时才调用它：这份清单有 400+ 条、上游响应约
// 500 KB，没必要在页面加载时一起拉下来。
func handleAdminModelCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	res := catalogSnapshot(r.URL.Query().Get("refresh") == "1")

	// installed 是当前已启用的模型 ID，面板靠它标记「已启用」。
	// 注意这份清单与内置模型是两个命名空间（清单里全是 vendor/model 形式的 slug，实测
	// 444 条里没有一条 cline-free/* 或 cline-pass/*），所以它一般只对「用户从这份清单里
	// 加过的模型」生效——但仍然必须带上：否则前端没法把已启用的卡片标出来。
	installed := make([]string, 0)
	for _, m := range allModels() {
		installed = append(installed, m.ID)
	}

	var fetchedAt int64
	if !res.FetchedAt.IsZero() {
		fetchedAt = res.FetchedAt.UnixMilli()
	}

	// 完全没有数据（无缓存且回源失败）时用 502，让前端能明确区分。
	status := http.StatusOK
	if len(res.Models) == 0 {
		status = http.StatusBadGateway
	}

	writeAPI(w, status, apiResponse{
		Success: len(res.Models) > 0,
		Error:   res.Err,
		Data: map[string]any{
			"models":    res.Models,
			"installed": installed,
			"fetchedAt": fetchedAt,
			"cached":    res.Cached,
			"stale":     res.Stale,
		},
	})
}
