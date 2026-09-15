package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// recommendedModelsURL 是 Cline 官方的推荐模型清单接口。
//
// 该接口只对 cline 自家域名放行 CORS，浏览器无法直接读取，
// 因此统一由本进程在服务端抓取后再转发给管理面板。
const recommendedModelsURL = "https://api.cline.bot/api/v1/ai/cline/recommended-models"

// recommendedFetchTimeout 限制单次上游抓取耗时，避免面板请求被挂死。
const recommendedFetchTimeout = 15 * time.Second

// recommendedCacheTTL 是自动刷新场景下允许复用缓存的最长时间；
// 超过这个时间后页面加载会主动回源一次。
const recommendedCacheTTL = 30 * time.Minute

// remoteModel 是上游返回的单个模型条目。
type remoteModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// modelGroup 是按上游分组键聚合后的模型集合。
// 用切片而非 map，保证前端渲染顺序稳定。
type modelGroup struct {
	Key    string        `json:"key"`
	Models []remoteModel `json:"models"`
}

// recommendedGroupOrder 固定分组的展示顺序；上游新增的分组会按字典序排在末尾。
var recommendedGroupOrder = []string{"recommended", "free", "clinePass", "clineCloud"}

// descriptionZh 是上游英文描述的官方中文对照。
//
// 为什么在服务端做翻译而不是前端：描述是上游固定文案，服务端翻译一次即可，
// 前端浏览器直连不到该接口（CORS），也不该为此额外请求翻译服务。
// 上游若新增或改写描述，未命中映射时按原文返回，不会丢内容。
var descriptionZh = map[string]string{
	"Kimi K3 is Moonshot AI’s new flagship MoE model for agentic coding":                                                           "Kimi K3 是 Moonshot AI 面向智能体编码的新旗舰 MoE 模型",
	"SpaceXAI's smartest model with frontier performance on coding":                                                                "SpaceXAI 最智能的模型，编码能力达到前沿水平",
	"Fast and efficient with 1M context window":                                                                                    "快速高效，支持 100 万 token 上下文",
	"Meta’s multimodal reasoning model for experimentation, learning, and early-stage agentic, multi-agent, and coding workflows.": "Meta 的多模态推理模型，适用于实验、学习，以及早期的智能体、多智能体与编码工作流",
	"Latest natively multimodal model in the GLM-5 series.":                                                                        "GLM-5 系列中最新的原生多模态模型",
	"Strong model for office productivity, document-intensive work, and coding.":                                                   "适合办公生产力、文档密集型任务与编码的强劲模型",
	"Latest coding agent model from Poolside":                                                                                      "Poolside 最新的编码智能体模型",
	"Leading open weights model (reliability might be unstable and will consume usage faster than others)":                         "领先的开源权重模型（稳定性可能欠佳，且额度消耗快于其他模型）",
	"Top open weights model":                                  "顶尖开源权重模型",
	"Qwen's New SOTA coding model":                            "Qwen 全新的 SOTA 编码模型",
	"Frontier reasoning and coding with 1M context window":    "前沿推理与编码能力，支持 100 万 token 上下文",
	"Smarter and more efficient, with 1M context window":      "更智能、更高效，支持 100 万 token 上下文",
	"Strong multimodal model for long-horizon agent tasks":    "面向长周期智能体任务的强劲多模态模型",
	"Flagship agent model with 1M context window":             "旗舰级智能体模型，支持 100 万 token 上下文",
	"Frontier coding and agent model with 1M context window":  "前沿编码与智能体模型，支持 100 万 token 上下文",
	"Latest Kimi model specialized for agentic coding":        "Kimi 最新专为智能体编码打造的模型",
	"Z-AI's new top open-weights model":                       "Z-AI 全新的顶级开源权重模型",
	"Fast multimodal agent model with vision and video input": "快速多模态智能体模型，支持图像与视频输入",
	"Top open model for long autonomous coding runs":          "顶尖开源模型，适合长时间自主编码任务",
	"Fast and efficient MiMo for everyday coding":             "快速高效的 MiMo，胜任日常编码",
}

// localizeDescription 返回描述的中文版本；无对应翻译时回退为原文。
//
// 上游同一句描述在不同分组里可能带/不带结尾句点
// （例如 "Latest natively multimodal model in the GLM-5 series" 与 "...series."），
// 因此这里对结尾标点做容错查找，避免因一个句点漏翻。
func localizeDescription(desc string) string {
	trimmed := strings.TrimSpace(desc)
	if trimmed == "" {
		return desc
	}
	for _, candidate := range []string{
		trimmed,
		strings.TrimSuffix(trimmed, "."),
		trimmed + ".",
	} {
		if zh, ok := descriptionZh[candidate]; ok {
			return zh
		}
	}
	return desc
}

// localizeModels 就地替换一组模型的描述为中文。
func localizeModels(models []remoteModel) {
	for i := range models {
		models[i].Description = localizeDescription(models[i].Description)
	}
}

var (
	recommendedMu    sync.Mutex
	recommendedCache []modelGroup
	recommendedAt    time.Time
)

// recommendedResult 描述一次取数结果，供 handler 原样转发给前端。
type recommendedResult struct {
	Groups    []modelGroup
	FetchedAt time.Time
	Cached    bool   // 直接命中缓存，未回源
	Stale     bool   // 回源失败，退回了过期缓存
	Err       string // 非致命错误（回源失败原因）
}

// parseRecommendedModels 把上游 JSON 解析成有序分组。
// 单独抽出来是为了能在不联网的情况下做单元测试。
func parseRecommendedModels(body []byte) ([]modelGroup, error) {
	var raw map[string][]remoteModel
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析上游 JSON: %w", err)
	}

	groups := make([]modelGroup, 0, len(raw))
	for _, key := range recommendedGroupOrder {
		models, ok := raw[key]
		if !ok {
			continue
		}
		localizeModels(models)
		groups = append(groups, modelGroup{Key: key, Models: models})
		delete(raw, key)
	}

	// 上游若有新增分组，按字典序追加，避免静默丢弃。
	rest := make([]string, 0, len(raw))
	for key := range raw {
		rest = append(rest, key)
	}
	sort.Strings(rest)
	for _, key := range rest {
		localizeModels(raw[key])
		groups = append(groups, modelGroup{Key: key, Models: raw[key]})
	}

	total := 0
	for _, g := range groups {
		total += len(g.Models)
	}
	if total == 0 {
		return nil, errors.New("上游返回的模型列表为空")
	}
	return groups, nil
}

// fetchRecommendedModels 从 Cline 上游抓取并解析模型清单。
func fetchRecommendedModels() ([]modelGroup, error) {
	ctx, cancel := context.WithTimeout(context.Background(), recommendedFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, recommendedModelsURL, nil)
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	return parseRecommendedModels(body)
}

// recommendedSnapshot 返回当前分组数据，回源抓取走真实网络。
// force 为真或缓存已过期时回源；回源失败会退回过期缓存而不是让页面空白。
func recommendedSnapshot(force bool) recommendedResult {
	return recommendedSnapshotWith(force, fetchRecommendedModels)
}

// recommendedSnapshotWith 是取数的共用实现，fetch 可注入以便测试。
//
// 网络请求刻意放在锁外：抓取最长可能耗时 15 秒，持锁会阻塞所有并发请求。
// 提交前会复核缓存是否已被其他并发请求刷新，避免用旧结果覆盖新结果。
func recommendedSnapshotWith(force bool, fetch func() ([]modelGroup, error)) recommendedResult {
	recommendedMu.Lock()
	cache, at := recommendedCache, recommendedAt
	recommendedMu.Unlock()

	fresh := cache != nil && time.Since(at) < recommendedCacheTTL
	if !force && fresh {
		return recommendedResult{Groups: cache, FetchedAt: at, Cached: true}
	}

	groups, err := fetch()

	recommendedMu.Lock()
	defer recommendedMu.Unlock()

	if err != nil {
		// 退回到锁内最新的缓存（可能已被其他并发请求刷新成功）
		if recommendedCache != nil {
			return recommendedResult{
				Groups:    recommendedCache,
				FetchedAt: recommendedAt,
				Cached:    true,
				Stale:     true,
				Err:       err.Error(),
			}
		}
		return recommendedResult{Err: err.Error()}
	}

	// 若在我们抓取期间缓存已被刷新得更新，则采用更新的那份，避免覆盖。
	if recommendedCache != nil && recommendedAt.After(at) && !force {
		return recommendedResult{Groups: recommendedCache, FetchedAt: recommendedAt, Cached: true}
	}

	recommendedCache = groups
	recommendedAt = time.Now()
	return recommendedResult{Groups: recommendedCache, FetchedAt: recommendedAt}
}

// GET /admin/api/recommended-models
//
// 查询参数 refresh=1 表示强制回源刷新（忽略缓存）。
// 返回分组数据、当前已安装的模型 ID，以及取数元信息。
func handleAdminRecommendedModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	res := recommendedSnapshot(r.URL.Query().Get("refresh") == "1")

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
	if len(res.Groups) == 0 {
		status = http.StatusBadGateway
	}

	writeAPI(w, status, apiResponse{
		Success: len(res.Groups) > 0,
		Error:   res.Err,
		Data: map[string]any{
			"groups":    res.Groups,
			"installed": installed,
			"fetchedAt": fetchedAt,
			"cached":    res.Cached,
			"stale":     res.Stale,
		},
	})
}

// POST /admin/api/models/batch
//
// 请求体 {"ids":["a","b"]}，逐个加入自定义模型。
// 已存在的 ID 计入 skipped 而不是报错，因此可安全地整组重复点击。
func handleAdminModelsBatchAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "ids is required"})
		return
	}

	added := make([]string, 0, len(req.IDs))
	skipped := make([]string, 0, len(req.IDs))
	failed := make(map[string]string)

	for _, id := range req.IDs {
		if _, err := addCustomModel(id); err != nil {
			if errors.Is(err, errModelExists) {
				skipped = append(skipped, id)
				continue
			}
			failed[id] = err.Error()
			continue
		}
		added = append(added, id)
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"added":   added,
		"skipped": skipped,
		"failed":  failed,
	}})
}

// errFakeUpstream 供测试注入回源失败。
var errFakeUpstream = errors.New("fake upstream failure")

// recommendedSnapshotFunc 允许注入取数函数（测试用），其余逻辑与 recommendedSnapshot 完全一致，
// 两者共用 recommendedSnapshotWith，避免实现漂移。
func recommendedSnapshotFunc(force bool, fetch func() ([]modelGroup, error)) recommendedResult {
	return recommendedSnapshotWith(force, fetch)
}
