package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// anthropicStreamEvents 跑一次 handleAnthropicStream，返回 (event, data) 序列。
func anthropicStreamEvents(t *testing.T, openAISSE string) []struct{ Event, Data string } {
	t.Helper()
	rec := httptest.NewRecorder()
	up := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(openAISSE)),
	}
	handleAnthropicStream(rec, up)

	var out []struct{ Event, Data string }
	var curEvent string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			curEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, struct{ Event, Data string }{curEvent, strings.TrimPrefix(line, "data: ")})
		}
	}
	return out
}

// rebuildToolInput 按 Anthropic 语义重建工具参数：content_block_start 给空 input，
// 后续 input_json_delta 的 partial_json 逐段拼接。这正是官方 SDK 的做法。
func rebuildToolInput(t *testing.T, events []struct{ Event, Data string }) map[string]string {
	t.Helper()
	inputs := map[string]string{}
	names := map[string]string{}

	for _, e := range events {
		var payload map[string]any
		if json.Unmarshal([]byte(e.Data), &payload) != nil {
			continue
		}
		switch e.Event {
		case "content_block_start":
			cb, _ := payload["content_block"].(map[string]any)
			if cb == nil || cb["type"] != "tool_use" {
				continue
			}
			name, _ := cb["name"].(string)
			// 关键约定：input 必须为空对象，真实参数只能来自 input_json_delta。
			if in, ok := cb["input"].(map[string]any); !ok || len(in) != 0 {
				t.Errorf("content_block_start 的 tool_use.input 应为空对象，实际为 %v", cb["input"])
			}
			names[name] = name
			inputs[name] = ""
		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]any)
			if delta == nil || delta["type"] != "input_json_delta" {
				continue
			}
			partial, _ := delta["partial_json"].(string)
			// 把片段挂到最近打开的工具块上（单工具场景够用）。
			for k := range inputs {
				inputs[k] += partial
			}
		}
	}
	return inputs
}

// 回归：工具调用参数必须完整抵达客户端。
//
// 背景：旧实现在收到第一个 arguments 片段时就发出 content_block_start 并带上
// 当时的（往往不完整的）input，之后完全不再发参数——客户端最终只拿到 {}
// 或残缺 JSON，工具调用等于失效。这里锁住“重建后的参数必须等于上游给的原始
// JSON”，并覆盖首帧 arguments 为空串 / 为 "{}" 两种常见形态。
func TestAnthropicStreamPreservesToolArguments(t *testing.T) {
	// want 是期望重建出的参数（按 JSON 语义比较，键序无关）。
	cases := []struct {
		name string
		want string
		sse  []string
	}{
		{
			// OpenAI 官方常见形态：首帧先给 id/name，arguments 为空串。
			name: "首帧 arguments 为空串",
			want: `{"city":"San Francisco","unit":"celsius"}`,
			sse: []string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"San Franc"}}]}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"isco\",\"unit\":\"celsius\"}"}}]}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			},
		},
		{
			// 部分兼容实现首帧就带 "{}"——旧代码会立刻开块锁定 {}，
			// 拼错则会得到 "{}{\"city\":...}" 这种非法 JSON。
			name: "首帧 arguments 为 {} 占位",
			want: `{"city":"San Francisco","unit":"celsius"}`,
			sse: []string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"San Franc"}}]}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"isco\",\"unit\":\"celsius\"}"}}]}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			},
		},
		{
			// 正文 + 工具混合：正文占内容块下标 0，工具块必须从 1 开始。
			name: "正文与工具块混排",
			want: `{"city":"北京"}`,
			sse: []string{
				`data: {"choices":[{"delta":{"content":"让我查一下"}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"北京\"}"}}]}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			},
		},
		{
			// 上游一个参数片段都没给：也必须给出合法空对象而不是空串。
			name: "完全没有参数帧",
			want: `{}`,
			sse: []string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"get_weather"}}]}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := anthropicStreamEvents(t, strings.Join(tc.sse, "\n"))

			inputs := rebuildToolInput(t, events)
			got, ok := inputs["get_weather"]
			if !ok {
				t.Fatalf("没有收到 get_weather 工具块；事件序列=%v", events)
			}
			// 按 JSON 语义比较：键序不影响等价性。
			var gotObj, wantObj any
			if err := json.Unmarshal([]byte(got), &gotObj); err != nil {
				t.Fatalf("重建出的参数不是合法 JSON：%q (%v)", got, err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantObj); err != nil {
				t.Fatalf("用例期望值不是合法 JSON：%q", tc.want)
			}
			if !reflect.DeepEqual(gotObj, wantObj) {
				t.Errorf("重建后的工具参数 = %s, want %s", got, tc.want)
			}

			// 每个工具块都必须有 start / stop 配对，且 stop_reason 为 tool_use。
			var starts, stops int
			for _, e := range events {
				switch e.Event {
				case "content_block_start":
					if strings.Contains(e.Data, `"tool_use"`) {
						starts++
					}
				case "content_block_stop":
					stops++
				}
			}
			if starts != 1 || stops < 1 {
				t.Errorf("工具块 start/stop 不配对: starts=%d stops=%d", starts, stops)
			}
			if !strings.Contains(strings.Join([]string{events[len(events)-2].Data}, ""), "tool_use") {
				t.Errorf("message_delta 的 stop_reason 应为 tool_use，实际事件=%v", events[len(events)-2])
			}
		})
	}
}

// 工具块下标必须与正文块错开：不能出现两个 content_block_start 用同一 index。
func TestAnthropicStreamBlockIndexesAreUnique(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"先说一句"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"b","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")

	events := anthropicStreamEvents(t, sse)
	seen := map[float64]string{}
	for _, e := range events {
		if e.Event != "content_block_start" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(e.Data), &payload) != nil {
			continue
		}
		idx, _ := payload["index"].(float64)
		cb, _ := payload["content_block"].(map[string]any)
		kind, _ := cb["type"].(string)
		if prev, dup := seen[idx]; dup {
			t.Errorf("content_block_start 下标 %v 重复使用：先 %s 后 %s", idx, prev, kind)
		}
		seen[idx] = kind
	}
	// 两个工具块 + 一个正文块。
	if len(seen) != 3 {
		t.Errorf("期望 3 个不同的内容块下标，实际 %d 个: %v", len(seen), seen)
	}
}
