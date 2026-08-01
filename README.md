# Cline Go Proxy

Cline API 的反向代理服务，支持多账号轮询、OpenAI 和 Anthropic Messages API 双协议、API Key 鉴权，内置中文管理后台。

## 功能

- **双协议兼容**：同时支持 `/v1/chat/completions`（OpenAI）和 `/v1/messages`（Anthropic Messages API）
- **多账号轮询**：自动在多个 Cline 账号间切换负载（支持 `round_robin` / `fill` / `random` 策略）
- **中文管理后台**：浏览器访问 `/admin/` 即可管理账号、API Key、模型配置、请求头、代理设置
- **API Key 鉴权**：保护代理端点，支持生成/删除多个 API Key
- **System Prompt 覆盖**：项目目录下放 `override.md` 则自动替换系统提示词，不存在则使用客户端自带
- **账号导入**：支持 OAuth 浏览器登录、手动 Token 输入、批量文件导入
- **持久化存储**：账号和 Key 保存在 `.cline-accounts.json`

## 快速开始

### 直接运行

```bash
# 编译并启动（默认端口 3457）
go build -o cline-proxy.exe .
./cline-proxy.exe

# 指定端口
./cline-proxy.exe -port 3457

# 构建 + 启动 + 打开浏览器
go run . -start
```

启动后访问 http://127.0.0.1:3457/admin/ 进入管理后台。

### Docker 部署

```bash
# 构建并启动
docker compose up -d

# 查看日志
docker compose logs -f

# 停止
docker compose down
```

数据持久化在 `./data/` 目录下，`override.md` 会自动从项目根目录挂载到容器内。

### 后台管理面板身份验证

管理后台（`/admin/`）默认启用登录页。在项目根目录的 `.env` 文件中配置用户名和密码（Docker Compose 会自动读取）：

```ini
ADMIN_USER=admin
ADMIN_PASSWORD=请改成强密码
```

未设置 `ADMIN_PASSWORD` 时，服务启动会随机生成一个密码并打印到容器日志（`docker compose logs`），请从日志中获取。访问 `http://<服务器IP>:3457/admin/` 会跳转到登录页，登录成功后会话有效期为 24 小时，后台左下角可退出登录。

代理 API（`/v1/...`）不受影响，仍使用后台生成的 API Key 鉴权。

## 使用指南

### 1. 添加 Cline 账号

在管理后台 **账号管理** → **导入账号**，选择以下任一方式：

- **OAuth 浏览器登录**：点击按钮弹出 WorkOS 登录窗口，完成后自动填入
- **手动输入 Token**：输入已有账号的 Access Token
- **批量文件导入**：上传包含账号数据的 JSON 文件

### 2. 配置客户端

应用（如 Claude Code、Cline）配置为使用此代理：

**OpenAI 格式（/v1/chat/completions）：**
```
Base URL: http://127.0.0.1:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    cline-free/glm-5.2
```

**Anthropic 格式（/v1/messages）：**
```
Base URL: http://127.0.0.1:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    cline-free/glm-5.2
```

### 3. API Key 管理

在后台 **设置** → **API Keys** 中生成和管理。如果未配置任何 Key，代理允许无鉴权访问。

### 4. System Prompt 覆盖

在项目目录下创建 `override.md`，内容将替换所有客户端请求的系统提示词。删除该文件则使用客户端自带的提示词。

### 5. 请求头配置

后台 **设置** → **请求头** 可编辑转发给上游的自定义请求头（如 `x-client-type: cline-cli`）。

## 可用模型（实测）

### 消耗账户额度

| 模型 ID | 状态 | 说明 |
|---------|:----:|------|
| `deepseek/deepseek-v4-pro` | ✅ 可用 · 消耗额度 | DeepSeek V4 Pro |
| `openai/gpt-4.1-nano` | ✅ 可用 · 消耗额度 | GPT-4.1 Nano |
| `qwen/qwen3-235b-a22b` | ✅ 可用 · 消耗额度 | Qwen3 235B |
| `meta-llama/llama-4-maverick` | ✅ 可用 · 消耗额度 | Llama 4 Maverick |
| `deepseek/deepseek-v4-flash` | ⚠️ 响应为空 · 消耗额度 | API 返回 200 但内容为空 |
| `google/gemini-2.5-flash` | ⚠️ 响应为空 · 消耗额度 | API 返回 200 但内容为空 |
| `google/gemini-2.5-pro` | ⚠️ 响应为空 · 消耗额度 | API 返回 200 但内容为空 |

### 不消耗账户额度

| 模型 ID | 状态 | 说明 |
|---------|:----:|------|
| `cline-free/glm-5.2` | ✅ 可用 · 不消耗额度 | 免费模型，无限使用 |
| `cline-pass/glm-5.2` | ❌ 403 · 不消耗额度 | 需要 `cline-pass` 订阅 |
| `cline-pass/deepseek-v4-flash` | ❌ 403 · 不消耗额度 | 需要 `cline-pass` 订阅 |
| `cline-pass/qwen3.7-max` | ❌ 403 · 不消耗额度 | 需要 `cline-pass` 订阅 |

可在后台 **设置** → **默认模型** 中修改默认模型。

## 项目结构

```
├── main.go             入口，CLI 参数处理
├── proxy.go            HTTP 服务，API 路由，协议转换，SSE 流式处理
├── admin.go            管理后台 REST API
├── admin_html.go       管理后台前端 HTML（嵌入 Go 二进制）
├── auth.go             WorkOS OAuth 登录与 Token 刷新
├── pool.go             账号池管理、持久化、策略轮询
├── types.go            数据结构定义
├── capture.go          OAuth 信息捕获工具
├── http.go             HTTP 客户端与工具函数
├── Dockerfile          Docker 构建
├── docker-compose.yml  Docker Compose 配置
└── override.md         可选的系统提示词覆盖文件

---

感谢 [LINUX DO](https://linux.do) 社区
```

## GitHub Actions 自动打包镜像

仓库包含 `.github/workflows/docker-build.yml`，推送到 GitHub 后会自动构建镜像并发布到 GitHub Container Registry（ghcr.io）。

触发方式：

- 推送到 `main` / `master` 分支
- 推送 `v*` 格式的 tag（如 `v1.0.0`）
- 在仓库 Actions 页面手动触发

构建产物：

| 镜像 Tag | 说明 |
|---------|------|
| `ghcr.io/<用户名>/cline-proxy:latest` | 默认分支最新构建 |
| `ghcr.io/<用户名>/cline-proxy:v1.0.0` | 对应 tag 版本 |
| `ghcr.io/<用户名>/cline-proxy:<commit-sha>` | 每次构建 |

镜像同时构建 `linux/amd64` 和 `linux/arm64`。首次推送后，到仓库 **Packages** 页面把镜像设为 Public（或保持 Private，在服务器上 `docker login ghcr.io` 后再拉取）。

服务器直接拉镜像运行：

```bash
docker pull ghcr.io/<用户名>/cline-proxy:latest
docker run -d --name cline-proxy --restart unless-stopped \
  -p 3457:3457 \
  -e ADMIN_USER=admin -e ADMIN_PASSWORD=你的密码 \
  -v /path/to/.cline-accounts.json:/app/.cline-accounts.json \
  ghcr.io/<用户名>/cline-proxy:latest
```
