package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Checking uses bounded background jobs and never installs a model or saves
// upstream probe state. Each click sends only one text completion request.
var modelCheckJobs = newProbeJobStore(checkModelAvailability)

func handleAdminModelCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		handleProbeStatus(w, r, modelCheckJobs)
		return
	}
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		ModelID string `json:"modelId"`
	}
	if err := readJSONBody(r, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "请求格式不正确"})
		return
	}
	id, err := normalizeModelID(req.ModelID)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	handleProbeStart(w, id, modelCheckJobs)
}

func checkModelAvailability(ctx context.Context, modelID string) (*probeResult, error) {
	upstreamModel := upstreamModelID(modelID)
	body := probeRequestBody(upstreamModel, probeMaxTokens)
	body["messages"] = []any{map[string]any{"role": "user", "content": "Reply with only OK."}}
	applyUpstreamPrefs(body, modelID)
	start := time.Now()
	status, raw, err := upstreamCallContext(ctx, upstreamModel, body, 90*time.Second)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		reason := "上游请求失败"
		switch status {
		case 401, 403:
			reason = "当前账号认证失败或无权访问此模型"
		case 402:
			reason = "当前账号额度或订阅不足"
		case 404:
			reason = "模型或可用渠道不存在"
		case 429:
			reason = "当前账号或模型受到限流，请稍后重试"
		}
		return nil, fmt.Errorf("%s（HTTP %d）", reason, status)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("上游返回 HTTP 200，但响应不是有效的 JSON")
	}
	d := unwrapUpstream(obj)
	message := nestedMap(firstChoiceMap(d), "message")
	content, _ := message["content"].(string)
	// HTTP 200 alone (including empty reasoning output) cannot prove usability.
	if obj["error"] != nil || d["error"] != nil || strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("未获得有效文本回答，暂不能确认可用（可能是上游异常或输出预算不足）")
	}
	return &probeResult{ModelID: modelID, UpstreamModel: upstreamModel, LatencyMS: time.Since(start).Milliseconds()}, nil
}
