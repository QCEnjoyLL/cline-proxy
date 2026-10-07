package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const modelAuditTimeout = 30 * time.Minute
const modelAuditRetention = time.Hour
const modelAuditWorkers = 2

type modelAuditCatalog struct {
	ids      map[string]bool
	complete bool
	warning  string
}

// Free/Pass IDs may only appear in recommended groups. Absence is conclusive
// only when BOTH official lists were refreshed successfully, without stale data.
func fetchModelAuditCatalog(ctx context.Context) modelAuditCatalog {
	recommended := make(chan recommendedResult, 1)
	catalog := make(chan catalogResult, 1)
	go func() { recommended <- recommendedSnapshot(true) }()
	go func() { catalog <- catalogSnapshot(true) }()
	var rec recommendedResult
	var cat catalogResult
	select {
	case rec = <-recommended:
	case <-ctx.Done():
		return modelAuditCatalog{warning: "清单刷新已停止"}
	}
	select {
	case cat = <-catalog:
	case <-ctx.Done():
		return modelAuditCatalog{warning: "清单刷新已停止"}
	}
	return combineModelAuditCatalog(rec, cat)
}

func combineModelAuditCatalog(rec recommendedResult, cat catalogResult) modelAuditCatalog {
	ids := make(map[string]bool)
	warnings := make([]string, 0, 2)
	add := func(models []remoteModel) int {
		count := 0
		for _, model := range models {
			if id, err := normalizeModelID(model.ID); err == nil {
				ids[id] = true
				count++
			}
		}
		return count
	}
	recCount, catCount := 0, 0
	if !rec.Stale && rec.Err == "" {
		for _, group := range rec.Groups {
			recCount += add(group.Models)
		}
	}
	if recCount == 0 {
		warnings = append(warnings, "推荐清单刷新失败或为空")
	}
	if !cat.Stale && cat.Err == "" {
		catCount = add(cat.Models)
	}
	if catCount == 0 {
		warnings = append(warnings, "全部模型清单刷新失败或为空")
	}
	if len(warnings) > 0 {
		warnings = append(warnings, "未找到的模型标为“未确认”，不会据此建议移除")
	}
	return modelAuditCatalog{ids: ids, complete: recCount > 0 && catCount > 0, warning: strings.Join(warnings, "；")}
}

type modelAuditItem struct {
	ModelID       string `json:"modelId"`
	UpstreamModel string `json:"upstreamModel"`
	Catalog       string `json:"catalog"` // present, missing, unknown (uses redirect target)
	Status        string `json:"status"`  // pending, checking, available, unavailable, failed, skipped
	Error         string `json:"error,omitempty"`
	LatencyMS     int64  `json:"latencyMs,omitempty"`
	Suggested     bool   `json:"suggested"`
}

type modelAuditJob struct {
	ID        string           `json:"jobId"`
	Status    string           `json:"status"`
	Stage     string           `json:"stage"`
	Items     []modelAuditItem `json:"items"`
	Completed int              `json:"completed"`
	StartedAt int64            `json:"startedAt"`
	Warning   string           `json:"warning,omitempty"`
	Error     string           `json:"error,omitempty"`
	finished  time.Time
	cancel    context.CancelFunc
}

type modelAuditStore struct {
	mu    sync.Mutex
	job   *modelAuditJob
	fetch func(context.Context) modelAuditCatalog
	check func(context.Context, string) (*probeResult, error)
}

var modelAudits = newModelAuditStore(fetchModelAuditCatalog, checkModelAvailability)

func newModelAuditStore(fetch func(context.Context) modelAuditCatalog, check func(context.Context, string) (*probeResult, error)) *modelAuditStore {
	return &modelAuditStore{fetch: fetch, check: check}
}

func (s *modelAuditStore) snapshotLocked() modelAuditJob {
	copy := *s.job
	copy.Items = append([]modelAuditItem(nil), s.job.Items...)
	copy.cancel = nil
	return copy
}

func enabledModelAuditItems() []modelAuditItem {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	items := make([]modelAuditItem, 0, len(p.CustomModels))
	seen := make(map[string]bool)
	for _, id := range p.CustomModels {
		if seen[id] {
			continue
		}
		seen[id] = true
		target := id
		if cfg := lookupModelUpstreamLocked(p, id); cfg != nil && cfg.Redirect != "" {
			target = cfg.Redirect
		}
		items = append(items, modelAuditItem{ModelID: id, UpstreamModel: target, Catalog: "unknown", Status: "pending"})
	}
	return items
}

// One bounded job per server. Repeated clicks (including from another tab)
// share the running job; the latest results also survive a page reload.
func (s *modelAuditStore) start(items []modelAuditItem) (modelAuditJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.Status == "running" {
		return s.snapshotLocked(), nil
	}
	if len(items) == 0 {
		return modelAuditJob{}, errors.New("请先添加模型")
	}
	if len(items) > maxBatchModelIDs {
		return modelAuditJob{}, fmt.Errorf("一次最多检测 %d 个模型", maxBatchModelIDs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelAuditTimeout)
	job := &modelAuditJob{ID: rand.Text(), Status: "running", Stage: "catalog", Items: append([]modelAuditItem(nil), items...), StartedAt: time.Now().UnixMilli(), cancel: cancel}
	s.job = job
	go s.run(ctx, job)
	return s.snapshotLocked(), nil
}

func (s *modelAuditStore) get(id string) (modelAuditJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil || (id != "" && id != s.job.ID) || (!s.job.finished.IsZero() && time.Since(s.job.finished) > modelAuditRetention) {
		return modelAuditJob{}, false
	}
	return s.snapshotLocked(), true
}

func (s *modelAuditStore) stop(id string) (modelAuditJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil || id != s.job.ID {
		return modelAuditJob{}, false
	}
	if s.job.Status == "running" {
		s.job.cancel()
		s.job.Stage = "stopping"
	}
	return s.snapshotLocked(), true
}

func (s *modelAuditStore) run(ctx context.Context, job *modelAuditJob) {
	defer job.cancel()
	catalog := s.fetch(ctx)
	s.mu.Lock()
	job.Warning = catalog.warning
	for i := range job.Items {
		item := &job.Items[i]
		if catalog.ids[item.UpstreamModel] {
			item.Catalog = "present"
		} else if catalog.complete {
			item.Catalog = "missing"
		}
	}
	if ctx.Err() == nil {
		job.Stage = "checking"
	}
	s.mu.Unlock()

	queue := make(chan int, len(job.Items))
	for i := range job.Items {
		queue <- i
	}
	close(queue)
	var workers sync.WaitGroup
	for w := 0; w < modelAuditWorkers; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range queue {
				if ctx.Err() != nil {
					return
				}
				s.mu.Lock()
				job.Items[i].Status = "checking"
				id := job.Items[i].ModelID
				s.mu.Unlock()
				var result *probeResult
				_, err := normalizeModelID(id)
				if err == nil {
					result, err = s.check(ctx, id)
				} else {
					err = errors.New("历史模型 ID 格式不正确，请检查或移除")
				}
				s.mu.Lock()
				item := &job.Items[i]
				// Routing may have been edited while this model was queued. Use
				// the actual target of the request, including failed requests.
				if result != nil && result.UpstreamModel != "" {
					item.UpstreamModel, item.Catalog = result.UpstreamModel, "unknown"
					if catalog.ids[item.UpstreamModel] {
						item.Catalog = "present"
					} else if catalog.complete {
						item.Catalog = "missing"
					}
				}
				switch {
				case ctx.Err() != nil:
					item.Status, item.Error = "skipped", "检测已停止，尚未确认可用性"
				case err != nil:
					item.Status, item.Error = "failed", err.Error()
					var httpErr *modelCheckHTTPError
					if errors.As(err, &httpErr) && httpErr.status == http.StatusNotFound {
						item.Status = "unavailable"
					}
					job.Completed++
				case result == nil:
					item.Status, item.Error = "failed", "检测没有返回有效结果"
					job.Completed++
				default:
					item.Status, item.LatencyMS = "available", result.LatencyMS
					job.Completed++
				}
				// A working custom alias or an unlisted but working model should
				// remain. Transient failures alone never suggest removal.
				item.Suggested = item.Status == "unavailable" || (item.Catalog == "missing" && item.ModelID == item.UpstreamModel && item.Status == "failed")
				s.mu.Unlock()
			}
		}()
	}
	workers.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	job.Status, job.Stage, job.finished = "done", "done", time.Now()
	if ctx.Err() != nil {
		job.Status = "cancelled"
		job.Error = "检测已停止，保留已完成结果"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			job.Error = "检测达到 30 分钟时限，保留已完成结果；可重新检测"
		}
		for i := range job.Items {
			if job.Items[i].Status == "pending" || job.Items[i].Status == "checking" {
				job.Items[i].Status, job.Items[i].Error = "skipped", "未检测"
			}
		}
	}
}

func handleAdminModelAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		id := r.URL.Query().Get("jobId")
		job, ok := modelAudits.get(id)
		if !ok {
			if id == "" {
				writeAPI(w, http.StatusOK, apiResponse{Success: true})
			} else {
				writeAPI(w, http.StatusNotFound, apiResponse{Error: "检测结果已过期或服务已重启，请重新检测"})
			}
			return
		}
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: job})
	case http.MethodPost:
		job, err := modelAudits.start(enabledModelAuditItems())
		if err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
		writeAPI(w, http.StatusAccepted, apiResponse{Success: true, Data: job})
	default:
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
	}
}

func handleAdminModelAuditStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		ID string `json:"jobId"`
	}
	if readJSONBody(r, &req) != nil || req.ID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "jobId is required"})
		return
	}
	job, ok := modelAudits.stop(req.ID)
	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "检测任务不存在"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: job})
}
