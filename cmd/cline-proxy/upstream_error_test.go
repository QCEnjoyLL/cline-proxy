package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 上游状态码转达规则必须锁死：这是客户端判断「该退避重试」还是「该改请求」
// 的唯一依据。旧实现把一切都压成 500，客户端只能盲目重试，限流时反而加剧限流。
func TestClientStatusForUpstream(t *testing.T) {
	cases := []struct {
		upstream int
		want     int
		why      string
	}{
		{http.StatusTooManyRequests, http.StatusTooManyRequests,
			"429 必须原样转达：客户端需要知道被限流了"},
		{http.StatusUnauthorized, http.StatusBadGateway,
			"上游 401 是我方账号凭据失效，不能回 401 让客户端以为自己的 Key 有问题"},
		{http.StatusForbidden, http.StatusBadGateway,
			"同上：403 也属于我方账号问题"},
		{http.StatusNotFound, http.StatusNotFound,
			"404 多半是模型名写错，客户端需要看到真实原因"},
		{http.StatusBadRequest, http.StatusBadRequest,
			"400 是客户端请求本身有问题，原样转达"},
		{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity,
			"422 同上"},
		{http.StatusInternalServerError, http.StatusBadGateway,
			"上游 5xx 是上游故障，转 502 而不是让客户端背锅"},
		{http.StatusBadGateway, http.StatusBadGateway, "上游 502 → 502"},
		{http.StatusServiceUnavailable, http.StatusBadGateway, "上游 503 → 502"},
		{http.StatusOK, http.StatusBadGateway, "非错误码走到这里属异常，按 502 处理"},
		{http.StatusFound, http.StatusBadGateway, "重定向同上"},
	}
	for _, tc := range cases {
		if got := clientStatusForUpstream(tc.upstream); got != tc.want {
			t.Errorf("clientStatusForUpstream(%d) = %d, want %d (%s)",
				tc.upstream, got, tc.want, tc.why)
		}
	}
}

// writeUpstreamError 要按错误的语义选状态码，而不是一律 500。
func TestWriteUpstreamErrorMapsStatusAndType(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{
			name:       "带状态的限流错误",
			err:        newUpstreamError(http.StatusTooManyRequests, "upstream_error", "API 429: free limit reached"),
			wantStatus: http.StatusTooManyRequests,
			wantType:   "upstream_error",
		},
		{
			name:       "池子空了",
			err:        newUpstreamError(http.StatusServiceUnavailable, "no_account", "no active accounts available"),
			wantStatus: http.StatusServiceUnavailable,
			wantType:   "no_account",
		},
		{
			name:       "包了一层也要识别出来",
			err:        fmt.Errorf("outer: %w", newUpstreamError(http.StatusNotFound, "upstream_error", "API 404")),
			wantStatus: http.StatusNotFound,
			wantType:   "upstream_error",
		},
		{
			// 没有状态的裸错误（网络错误等）：不能回 500 让客户端以为请求有问题。
			name:       "普通错误回 502",
			err:        errors.New("connection reset by peer"),
			wantStatus: http.StatusBadGateway,
			wantType:   "api_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeUpstreamError(rec, tc.err)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"type":"`+tc.wantType+`"`) {
				t.Errorf("body 缺少 type=%s：%s", tc.wantType, body)
			}
			if !strings.Contains(body, `"message"`) {
				t.Errorf("body 缺少 message：%s", body)
			}
			// 关键回归点：任何情况下都不该再出现 500。
			if rec.Code == http.StatusInternalServerError {
				t.Error("不该再返回 500（旧实现的问题就是一律 500）")
			}
		})
	}
}

// 代理端不该再有 500：500 意味着「服务端自己出错了」，但我们的失败路径都能
// 说清是谁的问题（上游故障→502、我方没账号→503、上游限流→429、请求本身
// 有问题→4xx）。留下 500 只会让客户端无法判断该重试还是该改请求。
//
// 背景：这正是这次修复要根除的问题——旧实现把一切都压成 500。
// 这里直接读 proxy.go 源码做静态检查，因为「新增一个 500 返回点」是最容易
// 复发、又最不容易被行为测试覆盖的回归。
func TestProxySourceNeverReturns500(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("读取 proxy.go: %v", err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "StatusInternalServerError") {
			t.Errorf("proxy.go:%d 不该再返回 500，改用可表达语义的状态码：\n  %s",
				i+1, strings.TrimSpace(line))
		}
	}
}

// 状态码越界必须兜底成 502，而不是让 net/http 的 WriteHeader panic。
// 同样地，err 为 nil 也不能 panic。
func TestWriteUpstreamErrorHardensBadInput(t *testing.T) {
	t.Run("非法状态码兜底", func(t *testing.T) {
		rec := httptest.NewRecorder()
		// 0 与 999+ 都是 WriteHeader 会 panic 的值。
		writeUpstreamError(rec, &upstreamError{Status: 0, Type: "x", Message: "boom"})
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", rec.Code)
		}
	})

	t.Run("nil 错误不 panic", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeUpstreamError(rec, nil) // 不得 panic
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", rec.Code)
		}
	})

	t.Run("空 Type 回退为 api_error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeUpstreamError(rec, &upstreamError{Status: http.StatusBadGateway, Message: "x"})
		if body := rec.Body.String(); !strings.Contains(body, `"type":"api_error"`) {
			t.Errorf("body 应回退 type=api_error：%s", body)
		}
	})
}

// account_error 的 message 会原样回给持有 API Key 的调用者，不能泄露完整邮箱。
func TestUpstreamErrorMessagesDoNotLeakFullEmail(t *testing.T) {
	const full = "someone@example.com"
	for _, line := range []string{
		"account " + truncateEmail(full) + " token failed",
		"account " + truncateEmail(full) + " token expired permanently",
	} {
		if strings.Contains(line, full) {
			t.Errorf("回给客户端的 message 泄露了完整邮箱：%s", line)
		}
	}
	// 顺带确认 truncateEmail 确实会脱敏（否则上面的断言毫无意义）。
	if truncateEmail(full) == full {
		t.Errorf("truncateEmail(%q) 未脱敏，断言形同虚设", full)
	}
}
