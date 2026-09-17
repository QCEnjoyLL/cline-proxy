# ⚡ Cline Go Proxy

[![Docker Image](https://img.shields.io/badge/GHCR-ghcr.io%2Fqcenjoyll%2Fcline--proxy-blue?logo=docker)](https://github.com/QCEnjoyLL/cline-proxy/pkgs/container/cline-proxy)

一个面向 Cline API 的轻量反向代理：支持多账号轮询、OpenAI / Anthropic 双协议、API Key 鉴权，以及带登录保护的中文管理后台。

> 📦 镜像已经发布到 GHCR，使用者只需拉取镜像，无需安装 Go，也无需在本地构建。

## ✨ 功能亮点

- 🔄 **多账号轮询**：支持 `round_robin`、`fill`、`random` 三种策略
- 🔌 **双协议兼容**：支持 OpenAI Chat Completions 与 Anthropic Messages API
- 🖥️ **中文管理后台**：管理账号、API Key、模型、请求头和运行状态
- 🔐 **双层鉴权**：后台登录会话与代理 API Key 相互独立
- 📥 **多种账号导入方式**：OAuth、手动输入 Refresh Token、JSON 批量导入
- 📤 **批量导出账号**：导出文件可直接重新批量导入
- 🧠 **模型管理**：内置模型与自定义模型均可随时删除，删除后可通过添加恢复，支持选择默认模型
- 📝 **System Prompt 覆盖**：通过 `override.md` 替换客户端系统提示词
- 💾 **配置持久化**：账号、API Key、自定义模型和默认模型统一保存
- 🌊 **流式响应**：支持 SSE 与工具调用转换

## 🚀 Docker Compose 快速部署

### 1️⃣ 获取部署文件

```bash
git clone https://github.com/QCEnjoyLL/cline-proxy.git
cd cline-proxy
```

### 2️⃣ 准备配置和持久化文件

Linux / macOS：

```bash
cp .env.example .env
touch .cline-accounts.json override.md
```

Windows PowerShell：

```powershell
Copy-Item .env.example .env
New-Item .cline-accounts.json -ItemType File -Force
New-Item override.md -ItemType File -Force
```

编辑 `.env`，设置后台登录信息：

```ini
ADMIN_USER=admin
ADMIN_PASSWORD=请换成强密码
```

> 🔒 如果 `ADMIN_PASSWORD` 留空，服务会在每次新建容器时生成随机密码，可通过容器日志查看。

### 3️⃣ 拉取镜像并启动

```bash
docker compose pull
docker compose up -d
docker compose logs -f
```

Compose 会直接使用已经发布的镜像：

```text
ghcr.io/qcenjoyll/cline-proxy:latest
```

### 4️⃣ 打开管理后台

```text
http://<服务器IP>:3457/admin/
```

本机部署可直接访问：<http://127.0.0.1:3457/admin/>

## 🐳 使用 `docker run`

先创建持久化文件：

```bash
touch .cline-accounts.json override.md
```

然后启动容器：

```bash
docker pull ghcr.io/qcenjoyll/cline-proxy:latest

docker run -d \
  --name cline-proxy \
  --restart unless-stopped \
  -p 3457:3457 \
  -e ADMIN_USER=admin \
  -e ADMIN_PASSWORD=请换成强密码 \
  -v "$(pwd)/.cline-accounts.json:/app/.cline-accounts.json" \
  -v "$(pwd)/override.md:/app/override.md:ro" \
  ghcr.io/qcenjoyll/cline-proxy:latest
```

## 🔄 更新镜像

```bash
git pull
docker compose pull
docker compose up -d
```

查看当前状态：

```bash
docker compose ps
docker compose logs --tail=100
```

## 💾 持久化文件

| 主机文件 | 容器路径 | 内容 | 是否必需 |
|---|---|---|:---:|
| `.cline-accounts.json` | `/app/.cline-accounts.json` | 账号、Refresh Token、API Key、自定义模型、默认模型 | ✅ |
| `override.md` | `/app/override.md` | 自定义 System Prompt | 可选 |

> ⚠️ `.cline-accounts.json` 含账号凭据，请限制文件权限，不要上传、分享或提交到 Git。

当前版本不使用 `/app/data`，也不会再为该目录创建匿名卷。

## 📁 项目结构

```
.
├── cmd/cline-proxy/          # 程序源码（Go，单包）
│   ├── main.go               #   CLI 入口与 -start 编排
│   ├── proxy.go              #   OpenAI / Anthropic 代理主体
│   ├── admin.go              #   管理面板鉴权与全部 REST handler
│   ├── pool.go               #   账号池与持久化
│   ├── models.go             #   模型配置（增删、默认模型）
│   ├── recommended.go        #   官方推荐模型抓取与缓存
│   ├── catalog.go            #   「全部模型」清单抓取与缓存
│   ├── auth.go               #   WorkOS / Cline OAuth
│   ├── capture.go            #   -capture 抓包调试工具
│   ├── http.go, types.go     #   公共 HTTP 辅助与数据结构
│   ├── web_embed.go          #   go:embed 入口
│   └── web/                  #   前端页面（go:embed 嵌入二进制）
│       ├── admin.html        #     管理面板 SPA
│       └── login.html        #     登录页
├── Dockerfile
├── docker-compose.yml
└── README.md
```

> 📌 `web/` 必须与 Go 源文件同目录：`go:embed` 不支持 `../` 路径。
> 运行时数据（账号池、凭据、抓包日志）都定位在**可执行文件所在目录**，与源码布局无关。

## 🖥️ 管理后台

### 👤 账号管理

后台支持以下导入方式：

- 🔑 OAuth 浏览器登录
- ✏️ 手动输入 Cline `refreshToken`
- 📦 JSON 批量导入
- 📄 从 `.json` / `.txt` 文件导入

批量导入格式：

```json
[
  {
    "refreshToken": "xxx",
    "email": "user@example.com"
  }
]
```

点击 **账号管理 → 批量导出** 可下载相同格式的 JSON 文件。

> 🔐 导出文件包含 Refresh Token，应按密码文件处理。

### 🔑 API Key

在 **设置 → API 密钥管理** 中生成或删除代理 API Key。

- 支持 `Authorization: Bearer <key>`
- 支持 `x-api-key: <key>`
- 未配置任何 Key 时，代理 API 默认允许无鉴权访问

> 🛡️ 公网部署务必生成 API Key，并通过防火墙或反向代理限制访问。

### 🧠 模型管理

- 内置模型与自定义模型一样，可以随时删除；删除后可在「添加模型」中重新添加以恢复
- 可添加上游支持的自定义模型
- 可通过下拉框选择默认模型
- 自定义模型会出现在 `/v1/models` 和 `/models` 中
- 客户端请求显式指定 `model` 时，优先使用客户端提供的模型
- 删除当前默认模型时，默认模型会自动切换到剩余的第一个可用模型

#### 模型库

「模型库」标签页直接展示 Cline 官方推荐模型清单（`api.cline.bot` 的 `recommended-models` 接口），按上游分组平铺：

- **一键添加整个分组**：点击分组标题右侧的「全部添加」，把该组所有模型加入代理
- **单独添加**：点击任意模型卡片即可添加
- **复制模型 ID**：点击已添加的卡片（或卡片上的 ⧉）复制其 ID
- **不会重复添加**：已存在的模型会跳过，并提示「已存在 N 个」；整组已添加完时按钮自动禁用
- **手动/自动刷新**：右上角「刷新数据」强制回源；停留在该标签页时每 10 分钟自动刷新一次
- **描述保持上游原文**：模型描述不做翻译，直接显示 `api.cline.bot` 返回的原文，上游新增模型时不会出现中英混排
- **全部模型（默认折叠）**：列表底部还有一个默认收起的折叠区块，展示 `api/v1/ai/cline/models` 的全部模型（目前 444 条）。展开后按供应商（模型 ID 里 `/` 前那一段）分成若干组，每组可单独「全部添加」，顶部总览行也可一次加完全部。卡片格式与交互和上方分组完全一致，只是展开时才向服务端取数

> 📌 该接口只对 Cline 自家域名放行 CORS，浏览器无法直接读取，因此由代理在服务端抓取后转发。服务端带 30 分钟缓存；上游暂时不可用时会继续显示上次数据并提示原因。

对应接口：

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/admin/api/recommended-models` | 获取分组模型清单；加 `?refresh=1` 强制回源 |
| `GET` | `/admin/api/model-catalog` | 获取「全部模型」清单（400+ 条）；加 `?refresh=1` 强制回源。面板默认折叠，展开时才调用 |
| `POST` | `/admin/api/models/batch` | 批量添加模型，body `{"ids":["a","b"]}`；已存在的计入 `skipped` |

内置模型：

| 模型 ID | 类型 | 说明 |
|---|---|---|
| `cline-free/glm-5.2` | 免费 | 初始默认模型 |
| `cline-pass/glm-5.2` | Pass | 需要对应订阅 |
| `cline-pass/deepseek-v4-flash` | Pass | 需要对应订阅 |
| `cline-pass/qwen3.7-max` | Pass | 需要对应订阅 |

部分实测可用的额度模型：

| 模型 ID | 状态 |
|---|---|
| `deepseek/deepseek-v4-pro` | ✅ 可用 |
| `openai/gpt-4.1-nano` | ✅ 可用 |
| `qwen/qwen3-235b-a22b` | ✅ 可用 |
| `meta-llama/llama-4-maverick` | ✅ 可用 |
| `deepseek/deepseek-v4-flash` | ⚠️ 可能返回空内容 |
| `google/gemini-2.5-flash` | ⚠️ 可能返回空内容 |
| `google/gemini-2.5-pro` | ⚠️ 可能返回空内容 |

> 📌 模型可用性、额度消耗和订阅要求由上游决定，可能随时变化。

### 📝 System Prompt 覆盖

将自定义提示词写入 `override.md`，代理会使用该内容替换请求中的系统提示词。文件留空则继续使用客户端原始提示词。

### 📨 自定义请求头

在 **设置 → 请求头配置** 中可添加转发给上游的请求头，例如：

```text
x-client-type: cline-cli
```

## 🔌 客户端配置

### OpenAI 兼容接口

```text
Base URL: http://127.0.0.1:3457/v1
API Key:  <管理后台生成的 Key>
Model:    cline-free/glm-5.2
```

请求端点：

```text
POST /v1/chat/completions
```

### Anthropic Messages 接口

```text
Base URL: http://127.0.0.1:3457/v1
API Key:  <管理后台生成的 Key>
Model:    cline-free/glm-5.2
```

请求端点：

```text
POST /v1/messages
```

### 其他端点

| 方法 | 路径 | 用途 |
|---|---|---|
| `GET` | `/v1/models` | 获取模型列表 |
| `GET` | `/v1/health` | 健康检查 |
| `GET` | `/admin/` | 管理后台 |

## 📦 镜像信息

镜像地址：

```text
ghcr.io/qcenjoyll/cline-proxy
```

| Tag | 说明 |
|---|---|
| `latest` | 默认分支的最新构建 |
| `1.4.1` | 源码版本号（`cmd/cline-proxy/version.go`）。**每次提交都会随版本号自动更新** |
| `sha-<commit>` | 对应某次提交 |

镜像标签直接来自源码里的版本号，所以拉取任意一个版本标签就能固定到具体那一版：

```bash
docker pull ghcr.io/qcenjoyll/cline-proxy:1.4.1   # 固定版本
docker pull ghcr.io/qcenjoyll/cline-proxy:latest  # 跟随默认分支
```

打 Git 标签（如 `v1.4.0`）发布时，镜像标签取自标签名（去掉 `v`），
此时不会再额外打上源码里的版本号，避免两者不一致时产生混淆。

镜像支持：

- 🐧 `linux/amd64`
- 🍓 `linux/arm64`

GitHub Actions 会自动构建并发布镜像，普通使用者无需自行构建。

## 🩺 常见问题

<details>
<summary><strong>后台打不开或提示连接被拒绝</strong></summary>

确认容器正在运行、端口映射为 `3457:3457`，并检查服务器防火墙：

```bash
docker compose ps
docker compose logs --tail=100
```

</details>

<details>
<summary><strong>后台登录密码是什么</strong></summary>

优先查看 `.env` 中的 `ADMIN_PASSWORD`。如果留空，请从日志中查找自动生成的密码：

```bash
docker compose logs | grep ADMIN_PASSWORD
```

</details>

<details>
<summary><strong>修改 .env 后没有生效</strong></summary>

环境变量只在创建容器时注入，执行：

```bash
docker compose up -d --force-recreate
```

</details>

<details>
<summary><strong>.cline-accounts.json 变成了目录</strong></summary>

挂载前主机上不存在该文件，Docker 会将其创建为目录。删除错误目录，创建同名文件后重新创建容器：

```bash
touch .cline-accounts.json
docker compose up -d --force-recreate
```

</details>

<details>
<summary><strong>为什么以前会出现 /app/data 匿名卷</strong></summary>

旧镜像曾声明 `/app/data` 为 Volume，但程序实际不使用该目录。当前镜像已移除该声明；删除旧容器并更新镜像后不会再次创建。

</details>

<details>
<summary><strong>更新后仍然是旧版本</strong></summary>

先拉取最新镜像，再重新创建容器：

```bash
docker compose pull
docker compose up -d --force-recreate
```

</details>

## 🙏 致谢

感谢 [LINUX DO](https://linux.do) 社区。
