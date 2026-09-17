package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

var execCommand = exec.Command

var httpTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     90 * time.Second,
	DisableCompression:  false,

	// ResponseHeaderTimeout 限制「请求发出 → 收到响应头」的时长。
	//
	// 为什么必须设：没有它时，上游 TCP 连上却不回响应头会让 httpClient.Do
	// 永不返回——该请求的 goroutine 和 TCP 连接就此一直占着（同一个 client 还被
	// auth.go 的凭据刷新/注册复用）。
	//
	// 为什么不用 http.Client.Timeout：那是整个请求（含读完 body）的时限，会把
	// 正常的长时间流式响应一起掐断。这里限的是「首字节」而不是生成时长，所以
	// 对长回答是安全的：SSE 的响应头在生成一开始就发出来了。
	ResponseHeaderTimeout: 120 * time.Second,

	// TLS 握手不限时的话，一个半开的 TLS 连接同样能永久卡住请求。
	TLSHandshakeTimeout: 10 * time.Second,
}

var httpClient = &http.Client{
	Transport: httpTransport,
}

func httpPostForm(rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return httpClient.Do(req)
}

func httpPostJSON(rawURL string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return httpClient.Do(req)
}

func readBody(resp *http.Response) string {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(data)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}
