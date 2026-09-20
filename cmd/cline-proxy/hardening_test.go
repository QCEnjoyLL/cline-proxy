package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// 超过上限的请求体必须被拒，而且要用 413 而不是笼统的 400。
//
// 背景：这个端口在 docker-compose 里直接对外发布，未配置 API Key 时鉴权完全放行，
// 所以「任何人都能 POST 任意大的 body」是可达的远程内存耗尽路径。
func TestProxyRejectsOversizedRequestBody(t *testing.T) {
	// 比上限多 1 字节即可触发 MaxBytesReader。
	oversized := bytes.NewReader(make([]byte, maxRequestBodyBytes+1))

	rec := httptest.NewRecorder()
	handleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", oversized))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413；body=%s", rec.Code, rec.Body.String())
	}
}

// 上限之内但内容非法的请求体仍按 400 处理（不能因为加了上限就把两者混为一谈）。
func TestProxyStillRejectsMalformedBodyWith400(t *testing.T) {
	rec := httptest.NewRecorder()
	handleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewReader([]byte("{not json"))))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400；body=%s", rec.Code, rec.Body.String())
	}
}

// bodyTooLargeStatus 的映射：MaxBytesError → 413，其它读取错误 → 400。
func TestBodyTooLargeStatusMapping(t *testing.T) {
	if got := bodyTooLargeStatus(&http.MaxBytesError{Limit: 1}); got != http.StatusRequestEntityTooLarge {
		t.Errorf("MaxBytesError → %d, want 413", got)
	}
	if got := bodyTooLargeStatus(os.ErrPermission); got != http.StatusBadRequest {
		t.Errorf("其它错误 → %d, want 400", got)
	}
}

// Picking only advances the in-memory cursor; the next normal save includes it.
func TestPickAccountDefersCursorPersistence(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	for _, id := range []string{"a1", "a2"} {
		addAccount(&Account{
			AccountID:    id,
			Email:        id + "@example.com",
			Status:       "active",
			RefreshToken: "rt",
			AccessToken:  "workos:a",
			ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		})
	}

	before, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatal(err)
	}
	if acc := pickAccount("vendor/m"); acc == nil {
		t.Fatal("pickAccount 返回 nil")
	}
	after, err := os.ReadFile(poolPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("picking an account should not write the pool")
	}
	if err := savePool(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("落盘内容不是完整 JSON: %v (content=%q)", err, data)
	}
	// 两个账号、round_robin：第一次取走后索引应推进到 1。
	if p.CurrentIdx != 1 {
		t.Errorf("落盘的 currentIdx = %d, want 1", p.CurrentIdx)
	}
}

// override.md 的状态只该在「首次读取」或「内容变化」时记日志。
//
// 之前每个请求都会打一行（"not found" 或 "using override.md"），把日志淹掉。
// 这条测试对「文件存在 / 不存在」两种环境都成立：第二次调用不该再产生日志。
func TestLoadOverrideContentLogsAtMostOncePerState(t *testing.T) {
	originalOut := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(originalOut) })

	loadOverrideContent()
	afterFirst := buf.Len()

	loadOverrideContent()
	if buf.Len() != afterFirst {
		t.Errorf("第二次调用不该再记日志，新增输出：%q", buf.String()[afterFirst:])
	}
}
