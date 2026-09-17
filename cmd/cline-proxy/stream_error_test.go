package main

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errAfterReader 先吐完 data，再返回一个非 EOF 的错误——用来模拟「上游流中途断开」。
// io.NopCloser(strings.NewReader(...)) 只会给 io.EOF，测不出这个分支。
type errAfterReader struct {
	data string
	err  error
	sent bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	r.sent = true
	return 0, r.err
}

func (r *errAfterReader) Close() error { return nil }

// 超长单行必须报错，而不是让 bufio 内部缓冲无限增长。
func TestReadSSELineBoundsLineLength(t *testing.T) {
	t.Run("正常行原样返回", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("data: hello\nnext"))
		line, err := readSSELine(r)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if line != "data: hello\n" {
			t.Errorf("line = %q", line)
		}
	})

	t.Run("未超限的长行可以读到", func(t *testing.T) {
		// 明显超过 bufio 默认 4KB 缓冲，但仍低于上限：必须完整读出来。
		long := "data: " + strings.Repeat("a", 256<<10) + "\n"
		r := bufio.NewReader(strings.NewReader(long))
		line, err := readSSELine(r)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if line != long {
			t.Errorf("长行被截断：got %d bytes, want %d", len(line), len(long))
		}
	})

	t.Run("超过上限时报错", func(t *testing.T) {
		// 一条不带换行的超长数据：旧实现（ReadString）会一直吃内存。
		huge := strings.Repeat("x", maxSSELineBytes+4096)
		r := bufio.NewReader(strings.NewReader(huge))
		if _, err := readSSELine(r); !errors.Is(err, errSSELineTooLong) {
			t.Fatalf("err = %v, want errSSELineTooLong", err)
		}
	})

	t.Run("EOF 时返回已读到的内容", func(t *testing.T) {
		// 与 ReadString 语义一致：返回残行 + io.EOF。
		r := bufio.NewReader(strings.NewReader("data: tail"))
		line, err := readSSELine(r)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
		if line != "data: tail" {
			t.Errorf("line = %q", line)
		}
	})
}

// 上游流中途断掉时，Anthropic 端点必须发 error 事件，并且**不能**再补一套
// 「正常结束」的收尾事件——否则客户端会把被截断的回答当成模型主动结束。
func TestAnthropicStreamSignalsMidStreamFailure(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"半句话\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"再来半句\"}}]}\n"
	body := &errAfterReader{data: sse, err: errors.New("connection reset by peer")}

	rec := httptest.NewRecorder()
	handleAnthropicStream(rec, &http.Response{StatusCode: 200, Body: body})
	out := rec.Body.String()

	if !strings.Contains(out, "event: error") {
		t.Errorf("缺少 error 事件；实际输出：\n%s", out)
	}
	if strings.Contains(out, "upstream stream failed") == false {
		t.Errorf("error 事件没带上原因；实际输出：\n%s", out)
	}
	// 关键：不能把截断伪装成正常完成。
	if strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("中断后不该发 stop_reason=end_turn；实际输出：\n%s", out)
	}
	if strings.Contains(out, "event: message_stop") {
		t.Errorf("中断后不该发 message_stop；实际输出：\n%s", out)
	}
	// 正常情况下这些事件是必须有的，这里顺带确认测试真的走到了中断分支。
	if !strings.Contains(out, "event: message_start") {
		t.Errorf("缺少 message_start；实际输出：\n%s", out)
	}
}

// 上游明确返回 error 字段时同理：只发 error，不再补正常收尾。
func TestAnthropicStreamUpstreamErrorEventDoesNotFakeEndTurn(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n"
	rec := httptest.NewRecorder()
	handleAnthropicStream(rec, &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(sse)),
	})
	out := rec.Body.String()

	if !strings.Contains(out, "event: error") {
		t.Errorf("缺少 error 事件；实际输出：\n%s", out)
	}
	if strings.Contains(out, `"stop_reason":"end_turn"`) || strings.Contains(out, "event: message_stop") {
		t.Errorf("上游报错后不该再发正常收尾事件；实际输出：\n%s", out)
	}
}

// 干净的流仍必须完整收尾（确认上面的改动没有把正常路径也一起关掉）。
func TestAnthropicStreamCleanEndStillEmitsClosingEvents(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
		"data: [DONE]\n"
	rec := httptest.NewRecorder()
	handleAnthropicStream(rec, &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(sse)),
	})
	out := rec.Body.String()

	for _, want := range []string{
		"event: message_start",
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("正常结束的流缺少 %q；实际输出：\n%s", want, out)
		}
	}
	if strings.Contains(out, "event: error") {
		t.Errorf("正常结束的流不该有 error 事件；实际输出：\n%s", out)
	}
}

// OpenAI 端点：上游中途断掉时必须发一个 error chunk，而不是静默 break。
func TestOpenAIStreamSignalsMidStreamFailure(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"半句\"}}]}\n"
	body := &errAfterReader{data: sse, err: errors.New("unexpected EOF")}

	rec := httptest.NewRecorder()
	handleStreamResponse(rec, &http.Response{StatusCode: 200, Body: body})
	out := rec.Body.String()

	if !strings.Contains(out, `"type":"upstream_error"`) {
		t.Errorf("缺少 error chunk；实际输出：\n%s", out)
	}
	if !strings.Contains(out, "data: ") {
		t.Errorf("没有按 SSE 格式输出；实际输出：\n%s", out)
	}
}
