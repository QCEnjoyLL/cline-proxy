package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPerModelConcurrentAccessIsLockSafe 并发压测上游配置的读写路径。
//
// 本机没有 gcc，跑不了 -race（race detector 在 Windows 上要 cgo）。这里退而求其次：
// 用大量并发 goroutine 同时读配置、写配置、跑探测用的解析函数，
// 一旦有「持锁调用 savePool」这类自锁死，测试会以超时失败；一旦有 map 并发写，
// 运行时通常直接 panic（concurrent map writes）。
//
// 注意这**不能**替代 race detector：真正的 data race 未必触发 panic。
// 锁纪律的正确性另由设计保证——所有 PerModel 访问都在 poolMu 内，
// 且 lookupModelUpstreamLocked 返回结构体副本、不返回内部指针。
func TestPerModelConcurrentAccessIsLockSafe(t *testing.T) {
	upstreamTestPool(t)
	pool.PerModel = map[string]ModelUpstream{
		"m1": {Upstreams: []string{"alibaba"}, Pipeline: pipelinePlanner, Available: []string{"alibaba", "baseten"}},
	}

	const goroutines = 24
	const iterations = 120
	done := make(chan struct{})

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				switch i % 3 {
				case 0:
					// 读路径：构造上游 body 会走 modelUpstreamSnapshot + 注入。
					body := buildUpstreamBody(map[string]any{
						"model":    "m1",
						"messages": []any{map[string]any{"role": "user", "content": "hi"}},
					}, i%2 == 0)
					if body["model"] == nil {
						t.Error("body 缺少 model")
						return
					}
				case 1:
					// 直接读写配置快照。
					cfg := modelUpstreamSnapshot("m1")
					if cfg != nil {
						_ = buildUpstreamPrefs(cfg)
						_ = upstreamModelID("m1")
					}
				case 2:
					// 写路径：模拟后台保存（整条替换 + 落盘）。
					p := loadPool()
					poolMu.Lock()
					if p.PerModel == nil {
						p.PerModel = make(map[string]ModelUpstream)
					}
					p.PerModel["m1"] = ModelUpstream{
						Upstreams: []string{"alibaba"},
						Available: []string{"alibaba", "baseten", "novita"},
						Pipeline:  pipelinePlanner,
						UpdatedAt: time.Now().UnixMilli(),
					}
					poolMu.Unlock()
					if err := savePool(); err != nil {
						t.Errorf("savePool: %v", err)
						return
					}
				}
			}
		}(g)
	}

	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("并发访问超时：疑似死锁（例如持 poolMu 调用了 savePool）")
	}
}

// TestPerModelConcurrentProbeStyleParsing 并发跑探测用的纯解析函数，
// 确认它们不依赖全局可变状态（解析函数是纯函数，不该有隐藏共享）。
func TestPerModelConcurrentProbeStyleParsing(t *testing.T) {
	planner := []byte(`{"data":{"choices":[{"message":{"content":"hi","provider_metadata":{"gateway":{"routing":{
		"finalProvider":"alibaba","canonicalSlug":"deepseek/x","fallbacksAvailable":["a","b"]}}}}}]}}`)
	direct := []byte(`{"model":"upstage/solar-pro4","provider":"Upstage","choices":[{"message":{"content":"hi"}}]}`)
	errRaw := []byte(`{"error":"No available providers match the 'only' filter: __probe__. Available providers are: alibaba, baseten, boundless"}`)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if got := parseUpstreamRouting(planner); got.Provider != "alibaba" {
					t.Errorf("planner provider = %q", got.Provider)
					return
				}
				if got := parseUpstreamRouting(direct); got.Pipeline != pipelineDirect {
					t.Errorf("direct pipeline = %q", got.Pipeline)
					return
				}
				if got := parseAvailableProviders(errRaw, pipelinePlanner); len(got) != 3 {
					t.Errorf("providers = %v", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestUpstreamSaveDeleteConcurrentWithExport 让「保存/删除配置」与「导出账号」并发跑。
// 两者都会取 poolMu 与 saveMu，是最容易互相拖死的组合。
func TestUpstreamSaveDeleteConcurrentWithExport(t *testing.T) {
	upstreamTestPool(t)

	done := make(chan struct{})
	var wg sync.WaitGroup

	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				body := `{"modelId":"m` + string(rune('0'+id)) + `","upstreams":["alibaba"],"pinMode":"strict"}`
				rec := httptest.NewRecorder()
				handleAdminUpstreamSave(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/save", strings.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Errorf("save status = %d", rec.Code)
					return
				}
				rec = httptest.NewRecorder()
				handleAdminUpstreamDelete(rec, httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/delete",
					strings.NewReader(`{"modelId":"m`+string(rune('0'+id))+`"}`)))
				if rec.Code != http.StatusOK {
					t.Errorf("delete status = %d", rec.Code)
					return
				}
			}
		}(g)
	}

	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				rec := httptest.NewRecorder()
				handleAdminAccountExport(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/export", nil))
				if rec.Code != http.StatusOK {
					t.Errorf("export status = %d", rec.Code)
					return
				}
				var out []accountTransfer
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Errorf("export decode: %v", err)
					return
				}
			}
		}()
	}

	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("保存/删除/导出并发超时：疑似锁互相拖死")
	}
}
