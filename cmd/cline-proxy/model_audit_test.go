package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func awaitModelAudit(t *testing.T, s *modelAuditStore, id string) modelAuditJob {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		job, ok := s.get(id)
		if !ok {
			t.Fatal("audit disappeared")
		}
		if job.Status != "running" {
			return job
		}
		select {
		case <-deadline:
			t.Fatal("audit did not finish")
		case <-tick.C:
		}
	}
}

func auditItems(ids ...string) []modelAuditItem {
	items := make([]modelAuditItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, modelAuditItem{ModelID: id, UpstreamModel: id, Catalog: "unknown", Status: "pending"})
	}
	return items
}

func TestAuditCatalogCombinesFreePassAndFullListWithoutStaleAbsence(t *testing.T) {
	rec := recommendedResult{Groups: []modelGroup{{Key: "free", Models: []remoteModel{{ID: "cline-free/free"}, {ID: "cline-pass/pass"}}}}}
	cat := catalogResult{Models: []remoteModel{{ID: "vendor/normal"}}}
	combined := combineModelAuditCatalog(rec, cat)
	if !combined.complete || len(combined.ids) != 3 || !combined.ids["cline-free/free"] || !combined.ids["cline-pass/pass"] {
		t.Fatalf("incomplete union: %+v", combined)
	}
	for _, invalid := range []catalogResult{{}, {Models: cat.Models, Stale: true}, {Models: cat.Models, Err: "timeout"}, {Models: []remoteModel{{ID: ""}}}} {
		combined = combineModelAuditCatalog(rec, invalid)
		if combined.complete || combined.warning == "" || combined.ids["vendor/normal"] {
			t.Fatalf("bad catalog used as evidence: %+v", combined)
		}
	}
	rec.Stale = true
	combined = combineModelAuditCatalog(rec, cat)
	if combined.complete || combined.ids["cline-free/free"] || !combined.ids["vendor/normal"] {
		t.Fatalf("stale recommendations used: %+v", combined)
	}
}

func TestAuditCatalogForcesBothOfficialSourcesPastFreshCaches(t *testing.T) {
	recommendedMu.Lock()
	originalRec, originalRecAt := recommendedCache, recommendedAt
	recommendedCache, recommendedAt = []modelGroup{{Key: "old", Models: []remoteModel{{ID: "old"}}}}, time.Now()
	recommendedMu.Unlock()
	catalogMu.Lock()
	originalCat, originalCatAt := catalogCache, catalogAt
	catalogCache, catalogAt = []remoteModel{{ID: "old"}}, time.Now()
	catalogMu.Unlock()
	t.Cleanup(func() {
		recommendedMu.Lock()
		recommendedCache, recommendedAt = originalRec, originalRecAt
		recommendedMu.Unlock()
		catalogMu.Lock()
		catalogCache, catalogAt = originalCat, originalCatAt
		catalogMu.Unlock()
	})
	var calls atomic.Int32
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch r.URL.String() {
		case recommendedModelsURL:
			return reliabilityResponse(200, `{"free":[{"id":"cline-free/new"}]}`), nil
		case catalogModelsURL:
			return reliabilityResponse(200, `{"data":[{"id":"vendor/new"}]}`), nil
		default:
			return nil, errors.New("unexpected URL")
		}
	})
	result := fetchModelAuditCatalog(context.Background())
	if calls.Load() != 2 || !result.complete || result.ids["old"] || !result.ids["cline-free/new"] || !result.ids["vendor/new"] {
		t.Fatalf("fresh cache hid changes: %+v calls=%d", result, calls.Load())
	}
}

func TestAuditClassifiesMissingAliasesAndTransientFailures(t *testing.T) {
	items := auditItems("normal", "unlisted-working", "removed", "quota", "auth", "rate", "server", "network", "alias", "missing-alias", "not-found")
	items[8].UpstreamModel = "normal"
	items[9].UpstreamModel = "removed"
	s := newModelAuditStore(func(context.Context) modelAuditCatalog {
		return modelAuditCatalog{complete: true, ids: map[string]bool{"normal": true, "quota": true, "auth": true, "rate": true, "server": true, "network": true, "not-found": true}}
	}, func(_ context.Context, id string) (*probeResult, error) {
		switch id {
		case "normal", "unlisted-working", "alias":
			return &probeResult{LatencyMS: 75}, nil
		case "removed", "not-found":
			return nil, &modelCheckHTTPError{status: 404, message: "missing"}
		case "quota":
			return nil, &modelCheckHTTPError{status: 402, message: "quota"}
		case "auth":
			return nil, &modelCheckHTTPError{status: 403, message: "auth"}
		case "rate":
			return nil, &modelCheckHTTPError{status: 429, message: "rate"}
		case "server":
			return nil, &modelCheckHTTPError{status: 503, message: "server"}
		default:
			return nil, errors.New("timeout")
		}
	})
	started, err := s.start(items)
	if err != nil {
		t.Fatal(err)
	}
	job := awaitModelAudit(t, s, started.ID)
	if job.Completed != len(items) || job.Status != "done" {
		t.Fatalf("incomplete: %+v", job)
	}
	for _, item := range job.Items {
		wantSuggested := item.ModelID == "removed" || item.ModelID == "not-found"
		if item.Suggested != wantSuggested {
			t.Errorf("bad removal suggestion: %+v", item)
		}
	}
	if job.Items[1].Catalog != "missing" || job.Items[1].Status != "available" || job.Items[8].Catalog != "present" || job.Items[9].Catalog != "missing" {
		t.Fatalf("membership confused with usability or alias: %+v", job.Items)
	}
	job.Items[0].Status = "mutated"
	copy, _ := s.get(started.ID)
	if copy.Items[0].Status == "mutated" {
		t.Fatal("API snapshot shares mutable items")
	}
}

func TestAuditIncompleteCatalogNeverSuggestsMissingOnProbeFailure(t *testing.T) {
	s := newModelAuditStore(func(context.Context) modelAuditCatalog {
		return modelAuditCatalog{ids: map[string]bool{"normal": true}, warning: "refresh failed"}
	}, func(context.Context, string) (*probeResult, error) { return nil, errors.New("network timeout") })
	start, _ := s.start(auditItems("unknown"))
	job := awaitModelAudit(t, s, start.ID)
	if job.Warning == "" || job.Items[0].Catalog != "unknown" || job.Items[0].Suggested {
		t.Fatalf("false removal from stale/missing data: %+v", job)
	}
}

func TestAuditUsesRealAvailabilityCheckerAndActualRedirectTarget(t *testing.T) {
	p := seedReliabilityPool(t)
	p.Accounts[0].AccessToken = "workos:cached"
	p.Accounts[0].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	p.CustomModels = []string{"cline-free/ok", "alias", "removed", "quota"}
	p.PerModel["alias"] = ModelUpstream{Redirect: "vendor/real"}
	items := enabledModelAuditItems()
	// A redirect edited after start must not be marked against the old target.
	p.PerModel["removed"] = ModelUpstream{Redirect: "vendor/new-target"}
	before, _ := os.ReadFile(poolPath)
	var calls atomic.Int32
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			return nil, errors.New("bad request")
		}
		switch body["model"] {
		case "cline-free/ok", "vendor/real":
			return reliabilityResponse(200, `{"choices":[{"message":{"content":"OK"}}]}`), nil
		case "vendor/new-target":
			return reliabilityResponse(404, `{}`), nil
		case "quota":
			return reliabilityResponse(402, `{}`), nil
		default:
			return nil, errors.New("wrong model sent")
		}
	})
	s := newModelAuditStore(func(context.Context) modelAuditCatalog {
		return modelAuditCatalog{complete: true, ids: map[string]bool{"cline-free/ok": true, "vendor/real": true, "vendor/new-target": true, "quota": true}}
	}, checkModelAvailability)
	started, _ := s.start(items)
	job := awaitModelAudit(t, s, started.ID)
	if calls.Load() != 4 || job.Items[0].Status != "available" || job.Items[1].Status != "available" || job.Items[1].Suggested || job.Items[3].Suggested {
		t.Fatalf("incorrect real check results: %+v calls=%d", job.Items, calls.Load())
	}
	if item := job.Items[2]; item.UpstreamModel != "vendor/new-target" || item.Catalog != "present" || item.Status != "unavailable" || !item.Suggested {
		t.Fatalf("failed request lost actual routing: %+v", item)
	}
	after, _ := os.ReadFile(poolPath)
	if !reflect.DeepEqual(before, after) || len(p.CustomModels) != 4 {
		t.Fatal("checking modified enabled models")
	}
}

func TestAuditLimitsConcurrencyReusesActiveJobAndStops(t *testing.T) {
	var calls, running, peak atomic.Int32
	entered := make(chan struct{}, modelAuditWorkers)
	s := newModelAuditStore(func(context.Context) modelAuditCatalog { return modelAuditCatalog{} }, func(ctx context.Context, _ string) (*probeResult, error) {
		calls.Add(1)
		current := running.Add(1)
		defer running.Add(-1)
		for previous := peak.Load(); current > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, current) {
				break
			}
		}
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	items := auditItems("a", "b", "c", "d", "e")
	job, _ := s.start(items)
	for i := 0; i < modelAuditWorkers; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	shared, _ := s.start(auditItems("different"))
	if job.ID != shared.ID {
		t.Fatal("duplicate click started another batch")
	}
	if _, ok := s.stop("wrong-id"); ok {
		t.Fatal("wrong job stopped active batch")
	}
	s.stop(job.ID)
	done := awaitModelAudit(t, s, job.ID)
	if peak.Load() != modelAuditWorkers || calls.Load() != modelAuditWorkers || done.Status != "cancelled" || done.Completed != 0 {
		t.Fatalf("bad cancellation/concurrency: %+v calls=%d peak=%d", done, calls.Load(), peak.Load())
	}
	for _, item := range done.Items {
		if item.Status != "skipped" || item.Suggested {
			t.Fatalf("cancelled check suggested removal: %+v", item)
		}
	}
	s.mu.Lock()
	s.job.finished = time.Now().Add(-2 * modelAuditRetention)
	s.mu.Unlock()
	if _, ok := s.get(""); ok {
		t.Fatal("expired results remained visible")
	}
}

func TestAuditCapturesConfiguredRedirectsWithoutChangingEnabledModels(t *testing.T) {
	p := seedReliabilityPool(t)
	p.CustomModels = []string{"alias", "shared", "vendor/model", "alias"}
	p.PerModel["alias"] = ModelUpstream{Redirect: "vendor/real", Aliases: []string{"shared"}}
	before, _ := os.ReadFile(poolPath)
	items := enabledModelAuditItems()
	if len(items) != 3 || items[0].UpstreamModel != "vendor/real" || items[1].UpstreamModel != "vendor/real" || items[2].UpstreamModel != "vendor/model" {
		t.Fatalf("bad routing snapshot: %+v", items)
	}
	after, _ := os.ReadFile(poolPath)
	if !reflect.DeepEqual(before, after) || len(p.CustomModels) != 4 {
		t.Fatal("audit changed enabled model configuration")
	}
}

func TestBatchModelRemovalPersistsAndRollsBackDefaultAndHistoricalIDs(t *testing.T) {
	p := seedReliabilityPool(t)
	p.CustomModels = []string{"vendor/model", "legacy|id", "keep", "remove"}
	removed, skipped, err := deleteCustomModels([]string{"vendor/model", "legacy|id", "remove", "remove", "absent"})
	if err != nil || !reflect.DeepEqual(removed, []string{"vendor/model", "legacy|id", "remove"}) || !reflect.DeepEqual(skipped, []string{"absent"}) || getDefaultModel() != "keep" {
		t.Fatalf("batch deletion: removed=%v skipped=%v error=%v default=%s", removed, skipped, err, getDefaultModel())
	}
	pool = nil
	p = loadPool()
	if !reflect.DeepEqual(p.CustomModels, []string{"keep"}) || len(p.PerModel) != 1 || len(p.Accounts) != 1 || len(p.Keys) != 1 {
		t.Fatalf("wrong persisted configuration: %+v", p)
	}
	if err := setDefaultModel("keep"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(poolPath)
	blockPoolWrites(t)
	if _, _, err := deleteCustomModels([]string{"keep"}); !errors.Is(err, errModelStorage) {
		t.Fatalf("write error lost: %v", err)
	}
	after, _ := os.ReadFile(poolPath)
	if !reflect.DeepEqual(before, after) || p.DefaultModel != "keep" || !reflect.DeepEqual(p.CustomModels, []string{"keep"}) {
		t.Fatal("partial deletion survived failed persistence")
	}
}

func TestModelAuditAndRemovalRoutesValidationAndAuth(t *testing.T) {
	seedReliabilityPool(t)
	old := modelAudits
	modelAudits = newModelAuditStore(func(context.Context) modelAuditCatalog { return modelAuditCatalog{} }, func(context.Context, string) (*probeResult, error) { return &probeResult{LatencyMS: 1}, nil })
	t.Cleanup(func() { modelAudits = old })
	for _, items := range [][]modelAuditItem{nil, make([]modelAuditItem, maxBatchModelIDs+1)} {
		if _, err := modelAudits.start(items); err == nil {
			t.Fatal("invalid batch size accepted")
		}
	}
	for _, tc := range []struct {
		method, path, body string
		handler            http.HandlerFunc
		want               int
	}{
		{"GET", "/models/audit", "", handleAdminModelAudit, 200},
		{"GET", "/models/audit?jobId=absent", "", handleAdminModelAudit, 404},
		{"PUT", "/models/audit", "", handleAdminModelAudit, 405},
		{"POST", "/models/audit/stop", `{}`, handleAdminModelAuditStop, 400},
		{"POST", "/models/audit/stop", `{"jobId":"absent"}`, handleAdminModelAuditStop, 404},
		{"POST", "/models/delete-batch", `{}`, handleAdminModelsBatchDelete, 400},
		{"POST", "/models/delete-batch", `{"ids":[]}`, handleAdminModelsBatchDelete, 400},
		{"POST", "/models/delete-batch", `{`, handleAdminModelsBatchDelete, 400},
		{"GET", "/models/delete-batch", "", handleAdminModelsBatchDelete, 405},
	} {
		w := httptest.NewRecorder()
		tc.handler(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handleAdminModelAudit(w, httptest.NewRequest("POST", "/models/audit", nil))
	var response struct {
		Data modelAuditJob `json:"data"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Data.ID == "" {
		t.Fatalf("start response: %d %s", w.Code, w.Body.String())
	}
	awaitModelAudit(t, modelAudits, response.Data.ID)
	w = httptest.NewRecorder()
	handleAdminModelsBatchDelete(w, httptest.NewRequest("POST", "/models/delete-batch", strings.NewReader(`{"ids":["vendor/model"]}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"removed":["vendor/model"]`) || getDefaultModel() != "" {
		t.Fatalf("delete all models: %d %s", w.Code, w.Body.String())
	}
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	for _, path := range []string{"/models/audit", "/models/audit/stop", "/models/audit/recheck", "/models/delete-batch"} {
		for _, method := range []string{"GET", "POST"} {
			w = httptest.NewRecorder()
			requireAdminAuth(mux).ServeHTTP(w, httptest.NewRequest(method, "/admin/api"+path, nil))
			if w.Code != 401 {
				t.Fatal(fmt.Sprintf("unprotected %s %s: %d", method, path, w.Code))
			}
		}
	}
}

func TestAuditRecheckUpdatesOnlyOneRowAndSurvivesReload(t *testing.T) {
	var checks, fetches atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	s := newModelAuditStore(func(context.Context) modelAuditCatalog {
		fetches.Add(1)
		return modelAuditCatalog{complete: true, ids: map[string]bool{"other": true, "retry": true}}
	}, func(ctx context.Context, id string) (*probeResult, error) {
		call := checks.Add(1)
		if id == "retry" && call <= 2 {
			return nil, &modelCheckHTTPError{status: 404, message: "model unavailable"}
		}
		if call > 2 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &probeResult{ModelID: id, LatencyMS: 123}, nil
	})
	start, _ := s.start(auditItems("retry", "other"))
	before := awaitModelAudit(t, s, start.ID)
	if !before.Items[0].Suggested || before.Completed != 2 {
		t.Fatalf("bad initial results: %+v", before)
	}
	started, status, err := s.recheck(start.ID, "retry")
	if err != nil || status != 202 || started.ID != start.ID || started.Completed != 1 || started.Items[0].Status != "pending" || started.Items[0].Suggested {
		t.Fatalf("bad restart: %+v status=%d err=%v", started, status, err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("retry did not start")
	}
	if _, status, _ := s.recheck(start.ID, "retry"); status != 409 {
		t.Fatal("concurrent retry was not rejected")
	}
	close(release)
	after := awaitModelAudit(t, s, start.ID)
	if checks.Load() != 3 || fetches.Load() != 1 || after.Completed != 2 || after.Items[0].Status != "available" || after.Items[0].Error != "" || after.Items[0].Suggested {
		t.Fatalf("retry did not replace failure: %+v calls=%d fetches=%d", after, checks.Load(), fetches.Load())
	}
	if !reflect.DeepEqual(before.Items[1], after.Items[1]) {
		t.Fatal("retry changed another model's result")
	}
	reloaded, _ := s.get("")
	if reloaded.Items[0].Status != "available" {
		t.Fatal("refresh restored the stale failed result")
	}
	if _, status, _ := s.recheck(start.ID, "absent"); status != 404 {
		t.Fatal("unknown model accepted")
	}
	if _, status, _ := s.recheck("old-job", "retry"); status != 404 {
		t.Fatal("old job accepted")
	}
}

func TestAuditRecheckHTTPValidationAndCancelledRowCount(t *testing.T) {
	old := modelAudits
	s := newModelAuditStore(func(context.Context) modelAuditCatalog { return modelAuditCatalog{} }, func(context.Context, string) (*probeResult, error) {
		return &probeResult{LatencyMS: 1}, nil
	})
	modelAudits = s
	t.Cleanup(func() { modelAudits = old })
	job, _ := s.start(auditItems("retry", "other"))
	awaitModelAudit(t, s, job.ID)
	s.mu.Lock()
	s.job.Items[0].Status, s.job.Completed, s.job.Status = "skipped", 0, "cancelled"
	s.job.Items[1].Status = "skipped"
	s.mu.Unlock()
	for _, body := range []string{`{}`, `{"jobId":"x"}`, `{`} {
		w := httptest.NewRecorder()
		handleAdminModelAuditRecheck(w, httptest.NewRequest("POST", "/models/audit/recheck", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("invalid body accepted: %s", body)
		}
	}
	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"jobId": job.ID, "modelId": "retry"})
	handleAdminModelAuditRecheck(w, httptest.NewRequest("POST", "/models/audit/recheck", strings.NewReader(string(body))))
	if w.Code != 202 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("restart failed: %d %s", w.Code, w.Body.String())
	}
	after := awaitModelAudit(t, s, job.ID)
	if after.Completed != 1 || after.Items[0].Status != "available" || after.Items[1].Status != "skipped" || after.Status != "cancelled" {
		t.Fatalf("skipped row was counted twice: %+v", after)
	}
	if _, _, err := s.recheck(job.ID, "other"); err != nil {
		t.Fatal(err)
	}
	after = awaitModelAudit(t, s, job.ID)
	if after.Completed != 2 || after.Status != "done" {
		t.Fatalf("remaining row did not complete: %+v", after)
	}
}
