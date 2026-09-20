package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func awaitProbeJob(t *testing.T, s *probeJobStore, id string) probeJob {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		job, ok := s.get(id)
		if !ok {
			t.Fatal("job disappeared")
		}
		if job.Status != "running" {
			return job
		}
		select {
		case <-deadline:
			t.Fatal("job did not finish")
		case <-tick.C:
		}
	}
}

func TestAsyncProbeSurvivesRequestAndPollsResult(t *testing.T) {
	release := make(chan struct{})
	s := newProbeJobStore(func(ctx context.Context, model string) (*probeResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &probeResult{ModelID: model, Pipeline: "direct", Note: "partial result"}, nil
		}
	})
	previous := upstreamProbeJobs
	upstreamProbeJobs = s
	t.Cleanup(func() { upstreamProbeJobs = previous })
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/admin/api/upstreams/probe", strings.NewReader(`{"modelId":"test/model","async":true}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	handleAdminUpstreamProbe(w, r)
	cancel()
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Data probeJob `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	job := response.Data
	if job.ID == "" || job.Status != "running" {
		t.Fatalf("bad start: %+v", job)
	}
	w = httptest.NewRecorder()
	handleAdminUpstreamProbe(w, httptest.NewRequest("GET", "/admin/api/upstreams/probe?jobId="+job.ID, nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("poll: %d", w.Code)
	}
	release <- struct{}{}
	done := awaitProbeJob(t, s, job.ID)
	if done.Status != "done" || done.Result.ModelID != "test/model" || done.Result.Note != "partial result" {
		t.Fatalf("result: %+v", done)
	}
	w = httptest.NewRecorder()
	handleAdminUpstreamProbe(w, httptest.NewRequest("GET", "/admin/api/upstreams/probe?jobId=unknown", nil))
	if w.Code != 404 {
		t.Fatalf("unknown job: %d", w.Code)
	}
}

func TestProbeJobsDeduplicateAndBoundConcurrency(t *testing.T) {
	release := make(chan struct{})
	s := newProbeJobStore(func(context.Context, string) (*probeResult, error) {
		<-release
		return nil, errors.New("mock upstream unavailable")
	})
	defer close(release)
	ids := make([]string, 0, maxRunningProbes)
	for i := 0; i < maxRunningProbes; i++ {
		job, err := s.start(fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, job.ID)
	}
	duplicate, err := s.start("0")
	if err != nil || duplicate.ID != ids[0] {
		t.Fatal("same model was not deduplicated")
	}
	if _, err := s.start("overflow"); !errors.Is(err, errProbeBusy) {
		t.Fatalf("concurrency: %v", err)
	}
	for range ids {
		release <- struct{}{}
	}
	for _, id := range ids {
		job := awaitProbeJob(t, s, id)
		if job.Status != "failed" || job.Error != "mock upstream unavailable" {
			t.Fatalf("failure: %+v", job)
		}
	}
}

func TestProbeJobsExpireAndEvictCompletedResults(t *testing.T) {
	s := newProbeJobStore(func(context.Context, string) (*probeResult, error) { return &probeResult{}, nil })
	s.jobs["expired"] = &probeJob{ID: "expired", Status: "done", createdAt: time.Now().Add(-probeJobRetention - time.Second)}
	if _, ok := s.get("expired"); ok {
		t.Fatal("expired result retained")
	}
	for i := 0; i < maxProbeJobs; i++ {
		id := fmt.Sprint(i)
		s.jobs[id] = &probeJob{ID: id, Status: "done", createdAt: time.Now().Add(time.Duration(i-maxProbeJobs) * time.Second)}
	}
	job, err := s.start("new")
	if err != nil {
		t.Fatal(err)
	}
	awaitProbeJob(t, s, job.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) != maxProbeJobs || s.jobs["0"] != nil {
		t.Fatal("cache did not evict oldest completed result")
	}
}

func TestProbeCancellationStopsHTTP(t *testing.T) {
	p := seedReliabilityPool(t)
	p.Accounts[0].AccessToken = "workos:cached"
	p.Accounts[0].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	_, err := probeModelUpstreamsContext(ctx, "vendor/model")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestPartialProbeCacheAndPersistenceRollback(t *testing.T) {
	p := seedReliabilityPool(t)
	p.PerModel["vendor/model"] = ModelUpstream{Pipeline: "direct", Available: []string{"provider"}, Upstreams: []string{"provider"}}
	if err := saveProbeResult("vendor/model", &probeResult{Pipeline: "direct"}); err != nil {
		t.Fatal(err)
	}
	if len(p.PerModel["vendor/model"].Available) != 1 {
		t.Fatal("partial probe erased cached providers")
	}
	if err := saveProbeResult("vendor/model", &probeResult{Pipeline: "planner"}); err != nil {
		t.Fatal(err)
	}
	if len(p.PerModel["vendor/model"].Available) != 0 {
		t.Fatal("pipeline change retained incompatible providers")
	}
	before := p.PerModel["vendor/model"]
	blockPoolWrites(t)
	if err := saveProbeResult("vendor/model", &probeResult{Pipeline: "direct", Available: []string{"new"}}); err == nil {
		t.Fatal("expected storage failure")
	}
	if !reflect.DeepEqual(before, p.PerModel["vendor/model"]) {
		t.Fatal("failed write changed memory")
	}
}
