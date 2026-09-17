package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func resetOAuthSessions() {
	oauthSessionsMu.Lock()
	oauthSessions = make(map[string]*oauthSessionState)
	oauthSessionsMu.Unlock()
}

// 过期会话必须被清掉：这个 map 没有别的地方会删除条目，否则会随
// 「点击开始 OAuth 登录」的次数单调增长（每条还对应一个后台 goroutine）。
func TestPruneOAuthSessionsDropsExpired(t *testing.T) {
	resetOAuthSessions()
	t.Cleanup(resetOAuthSessions)

	now := time.Now()
	oauthSessionsMu.Lock()
	oauthSessions["fresh"] = &oauthSessionState{CreatedAt: now.Add(-time.Minute)}
	// 边界：刚好还没到期，必须留下。
	oauthSessions["edge"] = &oauthSessionState{CreatedAt: now.Add(-oauthSessionTTL + time.Minute)}
	oauthSessions["stale"] = &oauthSessionState{CreatedAt: now.Add(-oauthSessionTTL - time.Minute)}
	// 零值 CreatedAt 不该永远赖着（正常路径不会产生，但清掉更安全）。
	oauthSessions["zero"] = &oauthSessionState{}
	oauthSessionsMu.Unlock()

	oauthSessionsMu.Lock()
	removed := pruneOAuthSessionsLocked(now)
	_, freshOK := oauthSessions["fresh"]
	_, edgeOK := oauthSessions["edge"]
	_, staleOK := oauthSessions["stale"]
	_, zeroOK := oauthSessions["zero"]
	remaining := len(oauthSessions)
	oauthSessionsMu.Unlock()

	if removed != 2 {
		t.Errorf("清理条数 = %d, want 2（stale + zero）", removed)
	}
	if !freshOK || !edgeOK {
		t.Error("未过期的会话被误删")
	}
	if staleOK || zeroOK {
		t.Error("过期/零值会话没被清掉")
	}
	if remaining != 2 {
		t.Errorf("剩余 %d 条, want 2", remaining)
	}
}

// 走真实写入口（registerOAuthSession）：反复登记不能让会话表无限增长。
func TestRegisterOAuthSessionKeepsMapBounded(t *testing.T) {
	resetOAuthSessions()
	t.Cleanup(resetOAuthSessions)

	// 模拟「很久以前堆积了很多会话」。
	oauthSessionsMu.Lock()
	for i := 0; i < 100; i++ {
		oauthSessions[fmt.Sprintf("old_%d", i)] = &oauthSessionState{
			CreatedAt: time.Now().Add(-time.Hour),
		}
	}
	oauthSessionsMu.Unlock()

	// 新登记一次：旧的应被顺带清掉。
	registerOAuthSession("oauth_new", &oauthSessionState{CreatedAt: time.Now()})

	oauthSessionsMu.Lock()
	n := len(oauthSessions)
	_, hasNew := oauthSessions["oauth_new"]
	oauthSessionsMu.Unlock()

	if !hasNew {
		t.Error("新会话没登记上")
	}
	if n != 1 {
		t.Errorf("登记后应有 1 条，实际 %d 条——会话表在无限增长", n)
	}
}

// handleOAuthStatus 必须返回会话的真实状态（并且字段是在锁内读出来的）。
func TestHandleOAuthStatusReportsState(t *testing.T) {
	resetOAuthSessions()
	t.Cleanup(resetOAuthSessions)

	registerOAuthSession("s_done", &oauthSessionState{
		CreatedAt: time.Now(),
		Done:      true,
		Success:   true,
		Email:     "done@example.com",
	})
	registerOAuthSession("s_failed", &oauthSessionState{
		CreatedAt: time.Now(),
		Done:      true,
		Success:   false,
		Error:     "boom",
	})
	registerOAuthSession("s_pending", &oauthSessionState{CreatedAt: time.Now()})

	cases := []struct {
		id       string
		wantCode int
		check    func(t *testing.T, data map[string]any)
	}{
		{"s_done", http.StatusOK, func(t *testing.T, d map[string]any) {
			if d["done"] != true || d["success"] != true {
				t.Errorf("done/success = %v/%v", d["done"], d["success"])
			}
			if d["email"] != "done@example.com" {
				t.Errorf("email = %v", d["email"])
			}
		}},
		{"s_failed", http.StatusOK, func(t *testing.T, d map[string]any) {
			if d["success"] != false {
				t.Error("success 应为 false")
			}
			if d["error"] != "boom" {
				t.Errorf("error = %v", d["error"])
			}
		}},
		{"s_pending", http.StatusOK, func(t *testing.T, d map[string]any) {
			// 未完成时不该带上 email/error。
			if _, ok := d["email"]; ok {
				t.Error("未完成的会话不该返回 email")
			}
			if _, ok := d["error"]; ok {
				t.Error("未完成的会话不该返回 error")
			}
		}},
		{"missing", http.StatusNotFound, nil},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleOAuthStatus(rec, httptest.NewRequest(http.MethodGet,
				"/admin/api/oauth/status?sessionId="+tc.id, nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if tc.check == nil {
				return
			}
			var body struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
			}
			tc.check(t, body.Data)
		})
	}
}

// 后台 goroutine 改写 state 的同时轮询状态，不得触发数据竞争。
//
// 背景：handleOAuthStatus 之前只把 state 指针取到锁外，再在锁外读
// state.Done/Success/Email/Error，而写方是持 oauthSessionsMu 改这些字段的——
// 这是真实的数据竞争。本测试需要 `go test -race` 才能可靠地暴露它（本机没有
// C 编译器，无法运行 -race），但即便不开 -race，也能覆盖「读到不一致的组合」
// 之外的基本正确性。
func TestHandleOAuthStatusConcurrentWithStateUpdates(t *testing.T) {
	resetOAuthSessions()
	t.Cleanup(resetOAuthSessions)

	state := &oauthSessionState{CreatedAt: time.Now()}
	registerOAuthSession("s_race", state)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 与真实写方（handleOAuthStart 的后台 goroutine）一样持锁改字段。
			oauthSessionsMu.Lock()
			state.Done = true
			state.Success = true
			state.Email = "race@example.com"
			oauthSessionsMu.Unlock()
		}
	}()

	for i := 0; i < 300; i++ {
		rec := httptest.NewRecorder()
		handleOAuthStatus(rec, httptest.NewRequest(http.MethodGet,
			"/admin/api/oauth/status?sessionId=s_race", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		// 字段是「一次性拷出来」的，所以 done 为 true 时 email 必然已在同一个
		// 快照里可见——不该出现 done=true 却完全没有 email 的半更新状态。
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析响应: %v", err)
		}
		if body.Data["done"] == true {
			if body.Data["email"] != "race@example.com" {
				t.Fatalf("done=true 时 email = %v，说明读到了半更新的状态", body.Data["email"])
			}
		}
	}
	close(stop)
	wg.Wait()
}
