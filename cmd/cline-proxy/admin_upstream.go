package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// 上游渠道配置的后台接口（面板「上游配置」页）。
//
// 存储落在账号池文件里（AccountPool.PerModel），理由：账号池文件已经是
// 「持久化运行时配置」的既有载体，且 writePoolFile 那套原子替换 + bind-mount
// 退化的保护是现成的——另起一个文件要把它整套重做一遍。

// upstreamEntry 是面板看到的一条上游配置：配置内容 + 它对应的模型 ID。
type upstreamEntry struct {
	ModelID string `json:"modelId"`
	ModelUpstream
}

// upstreamModelOption 是「可以配置上游的模型」下拉项。
type upstreamModelOption struct {
	ID       string `json:"id"`
	Cost     string `json:"cost"`     // free / pass / custom，面板据此分组与配色
	Provider string `json:"provider"` // 展示用供应商标识
	Custom   bool   `json:"custom"`
}

// GET /admin/api/upstreams
//
// 一次返回面板需要的全部数据：可配置的模型清单 + 已保存的配置。
// 模型清单只包含用户已添加的模型。
func handleAdminUpstreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	// 已添加的模型：与 /v1/models 用同一个来源。
	options := make([]upstreamModelOption, 0, 16)
	for _, model := range allModels() {
		options = append(options, upstreamModelOption{
			ID:       model.ID,
			Cost:     model.Cost,
			Provider: model.Provider,
			Custom:   model.Custom,
		})
	}

	p := loadPool()
	poolMu.Lock()
	entries := make([]upstreamEntry, 0, len(p.PerModel))
	for modelID, cfg := range p.PerModel {
		entries = append(entries, upstreamEntry{ModelID: modelID, ModelUpstream: cfg})
	}
	poolMu.Unlock()
	// map 迭代顺序随机，排序后前端渲染顺序才稳定。
	sort.Slice(entries, func(i, j int) bool { return entries[i].ModelID < entries[j].ModelID })

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"models":    options,
			"upstreams": entries,
		},
	})
}

// upstreamSaveRequest 是保存接口的请求体。
type upstreamSaveRequest struct {
	ModelID   string   `json:"modelId"`
	Redirect  string   `json:"redirect"`
	Upstreams []string `json:"upstreams"`
	Exclude   []string `json:"exclude"`
	PinMode   string   `json:"pinMode"`
	Aliases   []string `json:"aliases"`
}

// validModelID 挡掉会破坏存储结构或 JSON 的模型 ID。
//
// 账号池文件是 JSON，模型 ID 又是 map 的 key，所以引号/反斜杠这类字符
// 不能放进来。这里比 models.go 的 normalizeModelID 宽松（那个只管新增），
// 因为它面对的是「用户想指向的上游真实 ID」，允许 `/` 与 `:`。
func validModelID(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	return !strings.ContainsAny(s, "|\"'\\")
}

// readJSONBody 读取并解析请求体，限制大小避免后台接口成为内存放大器。
func readJSONBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

// POST /admin/api/upstreams/save  body: { modelId, redirect, upstreams, exclude, pinMode, aliases }
//
// 保存是**整体替换**语义：请求体里没给的渠道即「不配置」。
// 做成部分合并的话，「取消勾选某个渠道」就无法表达。
func handleAdminUpstreamSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	var req upstreamSaveRequest
	if err := readJSONBody(r, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid request: " + err.Error()})
		return
	}

	modelID := strings.TrimSpace(req.ModelID)
	if !validModelID(modelID) {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "modelId is required and must not contain | \" ' \\"})
		return
	}

	entry := ModelUpstream{
		Upstreams: sanitizeUpstreams(req.Upstreams),
		Exclude:   sanitizeUpstreams(req.Exclude),
		Aliases:   sanitizeModelIDs(req.Aliases),
		PinMode:   req.PinMode,
		UpdatedAt: time.Now().UnixMilli(),
	}
	if entry.PinMode != "preferred" {
		entry.PinMode = "strict"
	}

	if redirect := strings.TrimSpace(req.Redirect); redirect != "" {
		if !validModelID(redirect) {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "redirect must not contain | \" ' \\"})
			return
		}
		entry.Redirect = redirect
	}

	p := loadPool()
	poolMu.Lock()
	if p.PerModel == nil {
		p.PerModel = make(map[string]ModelUpstream)
	}
	// 探测缓存必须保留：保存渠道列表不该把刚探到的可用清单和管道归属清掉，
	// 否则「排除渠道」换算白名单会立刻失去依据。
	if prev, ok := p.PerModel[modelID]; ok {
		entry.Pipeline = prev.Pipeline
		entry.Available = prev.Available
		entry.Observed = prev.Observed
		entry.LastProvider = prev.LastProvider
		entry.ProbedAt = prev.ProbedAt
	}
	previous, existed := p.PerModel[modelID]
	p.PerModel[modelID] = entry
	if err := savePoolLocked(); err != nil {
		if existed {
			p.PerModel[modelID] = previous
		} else {
			delete(p.PerModel, modelID)
		}
		poolMu.Unlock()
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}
	poolMu.Unlock()

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: "saved",
		Data:    upstreamEntry{ModelID: modelID, ModelUpstream: entry},
	})
}

// POST /admin/api/upstreams/delete  body: { modelId }
//
// 删除后该模型回到自动模式（不注入任何上游偏好）。
func handleAdminUpstreamDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		ModelID string `json:"modelId"`
	}
	if err := readJSONBody(r, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid request: " + err.Error()})
		return
	}
	modelID := strings.TrimSpace(req.ModelID)
	if modelID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "modelId is required"})
		return
	}

	p := loadPool()
	poolMu.Lock()
	previous, existed := p.PerModel[modelID]
	delete(p.PerModel, modelID)
	if err := savePoolLocked(); err != nil {
		if existed {
			p.PerModel[modelID] = previous
		}
		poolMu.Unlock()
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}
	poolMu.Unlock()
	if !existed {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "no configuration for this model"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "deleted"})
}

// POST /admin/api/upstreams/probe  body: { modelId, async? }
// GET /admin/api/upstreams/probe?jobId=... 查询异步任务。
//
// 探测会真的打上游，但代价可控：第一步是一次极小的真实请求（用来回读管道归属），
// 第二步带假渠道名尝试获取渠道清单；网关若忽略筛选，仍可能生成回答。
func handleAdminUpstreamProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		handleAdminProbeStatus(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		ModelID string `json:"modelId"`
		Async   bool   `json:"async"`
	}
	if err := readJSONBody(r, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid request: " + err.Error()})
		return
	}
	modelID := strings.TrimSpace(req.ModelID)
	if modelID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "modelId is required"})
		return
	}
	if req.Async {
		handleAdminProbeStart(w, modelID)
		return
	}

	probe, err := executeUpstreamProbe(r.Context(), modelID)
	if err != nil {
		// 502：上游或账号不可用属于我们这条链路的问题，不是请求写错了。
		writeAPI(w, http.StatusBadGateway, apiResponse{Error: err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: probe})
}

func executeUpstreamProbe(ctx context.Context, modelID string) (*probeResult, error) {
	probe, err := probeModelUpstreamsContext(ctx, modelID)
	if err != nil {
		return nil, err
	}
	// 探测到的渠道清单与管道归属要落盘，否则「排除渠道」换算白名单时无据可依。
	if probe.Pipeline != "" || len(probe.Available) > 0 {
		if saveErr := saveProbeResult(modelID, probe); saveErr != nil {
			// 落盘失败不影响本次探测结果的价值，告知但不判为请求失败。
			log.Printf("Failed to persist probe result for %s: %v", modelID, saveErr)
			probe.Note = strings.TrimSpace(probe.Note + "（探测结果未能保存：" + saveErr.Error() + "）")
		}
	}
	return probe, nil
}

// sanitizeModelIDs 清洗别名列表：去空白、挡掉破坏格式的字符、去重（保序）。
func sanitizeModelIDs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if !validModelID(s) {
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
