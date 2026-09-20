package main

import "strings"

// Version 是本项目的版本号，也是唯一的版本来源。
//
// 面板左下角、/v1/health、/admin/api/stats、/admin/api/config 都用它，
// 所以升级版本只需要改这一行。
//
// 版本号规则（务必遵守）：
//
//	每次修复/改动都必须递增版本号，递增幅度与工程量成正比：
//	  - 1.3.0 → 1.3.1  小改动：改个文案、调个样式、修一处小 bug
//	  - 1.3.0 → 1.4.0  功能量级：新增接口/新面板/新机制，或一次较大的重构
//	  - 1.3.0 → 2.0.0  不兼容变更：破坏既有配置格式或对外接口
//
// 也就是说：不允许「只改代码不升版本」。判据是工程量，不是提交次数——
// 一次提交里塞了很多活就多升一点，一行小修就只动最后一位。
//
// 发布镜像/二进制时可以由构建参数覆盖（见 Dockerfile 与
// .github/workflows/docker-build.yml），打 tag 发布时会自动写入 tag 名：
//
//	go build -ldflags "-X main.Version=1.2.3" ./cmd/cline-proxy
var Version = "1.7.8"

// versionLabel 返回展示用的版本号，统一带一个 v 前缀
// （避免传入 "v1.2.3" 时显示成 "vv1.2.3"）。
func versionLabel() string {
	return "v" + strings.TrimPrefix(Version, "v")
}

// versionPlaceholder 是页面里的版本号占位符。
//
// 页面是 go:embed 进来的静态 HTML，没法用模板变量，所以在返回给浏览器之前
// 做一次字符串替换（见 adminStaticHandler / handleAdminLogin）。
// 直接把版本号写死在 HTML 里会立刻和 Version 脱钩。
const versionPlaceholder = "__CLINE_PROXY_VERSION__"
