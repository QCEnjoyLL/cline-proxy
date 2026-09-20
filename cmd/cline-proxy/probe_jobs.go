package main

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"sync"
	"time"
)

const probeJobTimeout = 5 * time.Minute
const probeJobRetention = 15 * time.Minute
const maxProbeJobs = 32
const maxRunningProbes = 4

var errProbeBusy = errors.New("探测任务较多，请稍后重试")

type probeJob struct {
	ID        string       `json:"jobId"`
	ModelID   string       `json:"modelId"`
	Status    string       `json:"status"`
	Result    *probeResult `json:"result,omitempty"`
	Error     string       `json:"error,omitempty"`
	createdAt time.Time
}

type probeJobStore struct {
	mu   sync.Mutex
	jobs map[string]*probeJob
	run  func(context.Context, string) (*probeResult, error)
}

var upstreamProbeJobs = newProbeJobStore(executeUpstreamProbe)

func newProbeJobStore(run func(context.Context, string) (*probeResult, error)) *probeJobStore {
	return &probeJobStore{jobs: make(map[string]*probeJob), run: run}
}

func (s *probeJobStore) pruneLocked(now time.Time) {
	for id, job := range s.jobs {
		if job.Status != "running" && now.Sub(job.createdAt) > probeJobRetention {
			delete(s.jobs, id)
		}
	}
}

// Return immediately; only short status polls traverse a reverse proxy. A
// repeated click for the same running model shares the job and upstream calls.
func (s *probeJobStore) start(modelID string) (probeJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	running := 0
	for _, job := range s.jobs {
		if job.Status == "running" {
			if job.ModelID == modelID {
				return *job, nil
			}
			running++
		}
	}
	if running >= maxRunningProbes {
		return probeJob{}, errProbeBusy
	}
	if len(s.jobs) >= maxProbeJobs {
		var oldest *probeJob
		for _, job := range s.jobs {
			if job.Status != "running" && (oldest == nil || job.createdAt.Before(oldest.createdAt)) {
				oldest = job
			}
		}
		if oldest == nil {
			return probeJob{}, errProbeBusy
		}
		delete(s.jobs, oldest.ID)
	}
	job := &probeJob{ID: rand.Text(), ModelID: modelID, Status: "running", createdAt: time.Now()}
	s.jobs[job.ID] = job
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeJobTimeout)
		defer cancel()
		result, err := s.run(ctx, modelID)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			job.Status, job.Error = "failed", err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				job.Error = "上游响应超时，请稍后重试"
			}
		} else {
			job.Status, job.Result = "done", result
		}
	}()
	return *job, nil
}

func (s *probeJobStore) get(id string) (probeJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	job, ok := s.jobs[id]
	if !ok {
		return probeJob{}, false
	}
	return *job, true
}

func handleAdminProbeStart(w http.ResponseWriter, modelID string) {
	handleProbeStart(w, modelID, upstreamProbeJobs)
}

func handleProbeStart(w http.ResponseWriter, modelID string, jobs *probeJobStore) {
	job, err := jobs.start(modelID)
	if err != nil {
		w.Header().Set("Retry-After", "2")
		writeAPI(w, http.StatusTooManyRequests, apiResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeAPI(w, http.StatusAccepted, apiResponse{Success: true, Data: job})
}

func handleAdminProbeStatus(w http.ResponseWriter, r *http.Request) {
	handleProbeStatus(w, r, upstreamProbeJobs)
}

func handleProbeStatus(w http.ResponseWriter, r *http.Request, jobs *probeJobStore) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.URL.Query().Get("jobId")
	if id == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "jobId is required"})
		return
	}
	job, ok := jobs.get(id)
	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "探测任务已过期或服务已重启，请重新探测"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: job})
}
