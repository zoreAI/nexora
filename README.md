<h1 align="center">
  <img src="assets/logo.png" alt="Nexora" width="72" height="72" />
  <br />
  Nexora
</h1>

<p align="center">Nexora 是一个自托管的 ChatGPT Web API 转发服务。它通过 ChatGPT 网页端账号调用上游能力，并向外提供 OpenAI 兼容接口、Anthropic 兼容接口、在线画图工作台、账号池管理、图片管理、日志审计和 Web 管理面板。</p>

> [!WARNING]
> 免责声明：
>
> 本项目基于对 ChatGPT 网页端相关接口的技术研究实现，面向个人学习、技术研究和技术交流场景。本项目不面向商业服务场景，作者不提供商业使用支持。
>
> - 使用者不得将本项目用于破坏市场秩序、恶意竞争、套利、刷量、账号滥用或绕过平台限制。
> - 使用者不得将本项目用于生成、传播或协助生成违法、暴力、色情、未成年人相关内容，或用于诈骗、欺诈、骚扰等非法或不当用途。
> - 使用本项目可能导致账号被限制、临时封禁或永久封禁。
> - 使用者应自行承担全部风险和法律责任。
> - 使用本项目即表示你已理解并同意本免责声明。

> [!IMPORTANT]
> 请不要使用重要账号、常用账号或高价值账号进行测试。上游接口、风控策略、模型权限和额度规则可能随时变化，本项目不保证长期可用。

## 项目定位

Nexora 的目标是把 ChatGPT 网页端能力封装成可自托管的 API 服务，适合下列场景：

- 需要把 ChatGPT 网页端文本能力接入 OpenAI 兼容客户端。
- 需要通过 API 调用 ChatGPT 网页端图片生成和图片编辑能力。
- 需要集中管理多个 ChatGPT 账号、额度、限流状态和失败状态。
- 需要一个带权限控制的在线画图工作台。
- 需要在服务器端缓存、管理、下载和清理生成图片。
- 需要查看每次请求的完整处理链路和上游请求过程。

本项目是 Go 后端实现，后端入口为 `cmd/server/main.go`，前端静态产物由 Go 服务直接托管。

## 核心能力

### OpenAI 兼容接口

- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/responses`
- `POST /v1/images/generations`
- `POST /v1/images/edits`

### Anthropic 兼容接口

- `POST /v1/messages`

### ChatGPT Web 上游能力

- 文本对话 SSE 流式转发。
- ChatGPT 网页端图片生成。
- ChatGPT 网页端图片编辑。
- 普通 1K 图片生成路线。
- Codex 高清图片生成路线，支持 2K 和 4K 规格。
- 参考图上传到 ChatGPT 文件系统后再发起图生图请求。
- 对话触发图片工具后解析并下载最终图片。
- Sentinel proof token、Turnstile token、SO token、Conduit token 和 trace id 请求头处理。
- 账号指纹复用，包括 `user-agent`、`oai-device-id`、`oai-session-id`、`sec-ch-ua` 等字段。
- 403 HTML 拦截识别和未输出前换号重试。
- 无输出错误时自动换号重试，避免拼接半截响应。

### 工具调用支持

- `/v1/chat/completions` 支持 OpenAI tools 格式，并返回标准 `tool_calls` / `finish_reason=tool_calls`。
- `/v1/messages` 支持将工具调用转换为 Anthropic `tool_use` 内容块。
- `/v1/responses` 支持输出 `function_call` item；流式模式会发送 `response.function_call_arguments.*` 生命周期事件。
- 推荐上游模型使用 canonical XML 工具块，长文本、代码和路径建议放入 CDATA：

  ```xml
  <tool_calls>
    <invoke name="read_file">
      <parameter name="path"><![CDATA[README.md]]></parameter>
    </invoke>
  </tool_calls>
  ```

- 兼容旧版 JSON `{"tool_calls":[...]}` 和旧 XML 工具标记。
- 会剥离上游工具 XML / JSON 标记，避免把 `<tool_calls>` 等内部标记直接显示给客户端。
- 会忽略 Markdown 代码块和 inline code span 中的工具调用示例，减少误触发。
- 支持 `tool_choice=none`、`required` 和强制函数名；不满足 required / forced 约束时返回协议级错误。

### 多模态输入

- 支持 OpenAI 风格图片输入。
- 支持 ChatGPT 文件上传链路中的图片上传、确认上传、图片指针引用。
- 支持 `input_file` / `file` 内容块的本地文本提取。
- 支持 TXT、Markdown 和轻量 PDF 文本提取。
- 含图片的 PDF 不做 OCR，会返回友好错误。

### 在线画图工作台

- 内置 Web 管理面板和在线画图页面。
- 支持文生图。
- 支持图生图。
- 支持多图参考编辑。
- 支持 `gpt-image-2` 和 `codex-gpt-image-2`。
- 支持 `1:1`、`16:9`、`9:16`、`4:3`、`3:4` 画幅。
- 支持 1K、2K、4K 清晰度。
- 支持图片任务、历史记录、图片归属和下载。
- 生图输入区提供提示词库，可搜索、筛选、预览并选用提示词；管理员可启停和刷新词源。
- 提示词库内置离线快照，远端刷新失败时继续使用已有缓存；刷新请求使用系统选定的代理出口。

### 账号池管理

- 支持直接导入 ChatGPT `access_token`。
- 支持导入带账号元数据的 JSON 记录。
- 支持账号类型、状态、额度、恢复时间和失败次数管理。
- 支持账号轮询选择。
- 支持图片账号并发控制。
- 支持普通账号和 Plus / Team / Pro 账号分层使用。
- 支持 Token 失效识别。
- 支持限流识别和恢复时间记录。
- 支持失败账号标记。
- 支持按账号类型筛选文本请求。

### 权限和配额

- 支持全局 `auth-key`。
- 支持用户级密钥。
- 支持 admin / user 权限区分。
- 支持普通用户和高级用户等级。
- 支持图片日额度、月额度、总额度。
- 支持聊天日额度、月额度、总额度。
- 普通用户可限制为 1K 图片和 free 账号池。
- 高级用户可使用 Plus / Team / Pro 账号池和高清图片能力。

### 日志和可视化链路追踪

终端会打印同一个 trace id 下的完整处理链路，便于排查请求失败原因。

可看到的步骤包括：

- 客户端请求进入。
- 选择文本账号或图片账号。
- bootstrap ChatGPT 首页。
- 获取 Sentinel chat requirements。
- 计算 legacy proof input。
- 是否携带 proof、turnstile、SO token。
- 上传图片 metadata 请求。
- object storage PUT 上传。
- 标记文件 uploaded。
- 转换消息格式。
- 请求 `/backend-api/conversation`。
- SSE 开始、事件数量和结束。
- 图片任务 prepare。
- 图片任务 start。
- 轮询图片 tool records。
- 解析下载 URL。
- 下载生成图片。
- 失败后换号重试。
- 最终成功或失败。

敏感信息会脱敏，包括：

- `Authorization`
- access token
- refresh token
- id token
- cookie
- password
- secret
- `OAI-Device-Id`
- `OAI-Session-Id`
- 上传 URL query

关闭终端链路追踪：

```bash
CHATGPT2API_TRACE=0 ./start.sh --port 3000
```

也可以使用：

```bash
CHATGPT2API_NETWORK_TRACE=0 ./start.sh --port 3000
```

### 图片管理和画廊

- 服务端缓存生成图片。
- 可在系统设置中选择仅本机、WebDAV、Cloudflare R2 或本机加远端双份保存生成图片；提供连接测试和历史图片同步。R2 需填写 S3 Endpoint、Bucket、Access Key ID 和 Secret Access Key；公开访问前缀可留空，由服务端读取私有 Bucket。
- 支持图片浏览和下载。
- 支持图片归属记录。
- 支持提示词记录。
- 支持标签管理。
- 支持按日期清理旧图片的本地副本；已同步到 WebDAV 的图片仍可由 `/images/` 回读。
- 支持保护画廊图片和用户图片，避免被自动清理。
- 支持公共画廊发布、撤回、查看和批量查询发布状态。

### 存储

默认使用 SQLite，数据库文件为 `data/chatgpt2api.db`。设置 `DATABASE_URL=postgresql://user:password@host:5432/dbname` 可切换到 PostgreSQL。SQLite 自定义路径使用 `DATABASE_URL=sqlite:///绝对路径/数据库文件.db`。旧的 `STORAGE_BACKEND=postgres` 仍可配合连接串使用；JSON 和 Git 后端不再支持。

首次启动时，服务会将 `data/` 下已有的账号、密钥、任务、画廊、注册状态和 `logs.jsonl` 导入数据库；旧文件原样保留作为备份。数据库已初始化后不会再次导入。图片文件继续存放在 `data/images/`，启动配置仍使用根目录的 `config.json`。不再支持 JSON 或 Git 作为业务存储后端。

账号与用户密钥在数据库中分别存为独立记录。旧版 Go SQL 数据库中整份集合文档会在启动时一次性迁入记录表；迁移失败会阻止启动，原文档保留供排查。升级前应停止旧服务进程，避免新旧版本同时写入同一数据库。

## 模型说明

文本接口的 `model` 字段会尽量透传给 ChatGPT 上游。实际可用模型取决于账号权限和上游当前开放状态。

图片接口推荐使用以下模型名：

| 模型 | 用途 |
|---|---|
| `gpt-image-2` | 普通图片生成和图片编辑 |
| `codex-gpt-image-2` | Codex 高清图片生成路线，适合 2K / 4K |

图片参数：

| 参数 | 支持值 |
|---|---|
| `size` | `1:1`、`16:9`、`9:16`、`4:3`、`3:4` |
| `resolution` | `1k`、`2k`、`4k` |

`resolution=2k` 或 `resolution=4k` 会优先走 Codex 高清路线，需要可用的 Plus / Team / Pro 账号。

## 快速开始

### 一键安装或更新

无需下载整个仓库，直接运行 GitHub 托管的安装脚本。脚本会从 GitHub Release 下载最新版本并安装到 `./nexora-install`：

```bash
curl -fsSL https://raw.githubusercontent.com/zoreAI/nexora/main/scripts/install_latest.sh | bash
```

指定安装目录：

```bash
curl -fsSL https://raw.githubusercontent.com/zoreAI/nexora/main/scripts/install_latest.sh | bash -s -- --dir /opt/nexora
```

如果没有可用 Release 包，也可以从最新源码构建安装：

```bash
curl -fsSL https://raw.githubusercontent.com/zoreAI/nexora/main/scripts/install_latest.sh | bash -s -- --from-source --web
```

已下载仓库源码时，也可以直接运行本地脚本：

```bash
bash scripts/install_latest.sh
```

脚本会保留已安装目录中的 `config.json` 和 `data/`。首次安装后先设置 `config.json` 中的 `auth-key`，或导出 `CHATGPT2API_AUTH_KEY`，再按提示运行：

```bash
cd ./nexora-install
./start.sh --port 3000
```

### 使用发布包运行

```bash
tar -xzf nexora-linux-amd64.tar.gz
cd nexora-linux-amd64
cp config.example.json config.json
./start.sh --port 3000
```

启动前请编辑 `config.json`，设置 `auth-key`。

访问地址：

- Web 面板：`http://localhost:3000`
- API 地址：`http://localhost:3000/v1`

### 使用 Docker 运行

```bash
docker build -t nexora .
docker run --rm \
  -p 3000:80 \
  -v "$PWD/data:/app/data" \
  -e CHATGPT2API_AUTH_KEY='replace-with-your-key' \
  nexora
```

镜像不包含本地 `config.json`。也可以将配置文件挂载到 `/app/config.json`；绑定的 `data/` 目录需要允许容器内非 root 用户写入。使用 `docker-compose.yml` 时需先准备根目录 `config.json`，再运行 `docker compose up -d --build`。

## 部署与升级

源码部署需要 `go.mod` 指定的 Go 版本和 Node.js 22。首次部署先设置 `config.json` 中的 `auth-key`，或设置环境变量 `CHATGPT2API_AUTH_KEY`，然后执行：

```bash
make web
go test ./...
./start.sh --build --port 3000
```

使用发布包时，解压后按上文创建 `config.json`，直接运行 `./start.sh --port 3000`；发布包已包含后端二进制和 `web_dist/`，无需在目标机器安装 Go 或 Node.js。

升级前先停止旧进程。在安装目录中备份运行数据和配置：

```bash
backup_dir="../nexora-backup-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$backup_dir"
cp -a data "$backup_dir/"
if [ -f config.json ]; then cp -a config.json "$backup_dir/"; fi
```

SQLite 的数据库及其 WAL 文件都在 `data/` 中；账号密码、2FA 密钥、邮箱 provider 配置和注册状态也保存在数据库。不要让新旧版本同时写入同一个数据库。更新程序和前端后重新启动，访问 `http://localhost:3000/` 确认页面正常，再在管理面板检查账号池与注册配置。升级失败时先停止新进程，恢复旧程序和前端，再恢复同一份备份中的 `data/` 与 `config.json`，最后启动旧版本。

旧版 `data/*.json` 和 `logs.jsonl` 只会在新数据库首次初始化时导入；导入失败会阻止启动，旧文件仍保留。使用 PostgreSQL 时先备份数据库，再升级程序，`DATABASE_URL` 必须指向预期实例。Docker 部署需持续挂载 `/app/data`；容器内服务监听 `:80`，示例映射到宿主机 `3000` 端口。

### 本地开发运行

要求 Go 版本与 `go.mod` 一致。

```bash
go test ./...
make run
```

构建前端静态产物：

```bash
make web
make run
```

### 打包发布

```bash
scripts/package_release.sh
```

或：

```bash
make package
```

如需重新构建前端后再打包：

```bash
make package-web
```

手动打包支持以下目标：

| 目标 | 环境变量 |
|---|---|
| Linux x86_64 | `TARGET_OS=linux TARGET_ARCH=amd64` |
| Linux ARM64 | `TARGET_OS=linux TARGET_ARCH=arm64` |
| macOS x86_64 | `TARGET_OS=darwin TARGET_ARCH=amd64` |

每个发布包都会包含：

- 对应目标的 `nexora` 主程序二进制。
- 已构建的前端静态文件 `web_dist/`。
- 对应目标可用的 `curl-impersonate` 二进制目录 `data/bin/curl-impersonate/`。

发布包会生成到：

```text
release/nexora-linux-amd64.tar.gz
```

## 配置

配置文件路径：

```text
config.json
```

常用字段：

```json
{
  "auth-key": "change-me",
  "proxy": "",
  "base_url": "",
  "refresh_account_interval_minute": 60,
  "image_retention_days": 15,
  "image_poll_timeout_secs": 120,
  "image_poll_interval_secs": 4,
  "image_poll_initial_wait_secs": 0,
  "image_account_concurrency": 3,
  "auto_remove_rate_limited_accounts": false,
  "auto_remove_invalid_accounts": false,
  "cleanup_protect_gallery": true,
  "cleanup_protect_user_images": true,
  "global_system_prompt": ""
}
```

图片任务后台队列默认使用 16 个 worker，最多接收 256 个待处理任务。队列满时接口返回 `503`，不会创建任务或扣除额度。可通过环境变量调整：

图片相关超时说明：

- `image_poll_timeout_secs`：图片 SSE 等待和最终图片记录轮询的总超时，设置为 `600` 即最多等待 10 分钟。
- `image_poll_interval_secs`：查询最终图片记录的间隔。
- `image_poll_initial_wait_secs`：SSE 结束后首次查询前的等待时间，网络慢或容易 429 时可设为 `5` 到 `10`。

### 环境变量

| 变量 | 说明 |
|---|---|
| `CHATGPT2API_AUTH_KEY` | 覆盖 `config.json` 中的 `auth-key` |
| `CHATGPT2API_ADDR` | 服务监听地址，例如 `:3000` |
| `DATABASE_URL` | PostgreSQL 连接串，或 `sqlite:///绝对路径/数据库文件.db`；不设置时使用 `data/chatgpt2api.db` |
| `STORAGE_BACKEND` | 可选 `sqlite` 或 `postgres`；设置 `DATABASE_URL` 时会按 URL 自动选择 |
| `CHATGPT2API_UPSTREAM_TRANSPORT` | 上游传输方式，支持 `tls-client`、`curl-impersonate` |
| `CHATGPT2API_CURL_IMPERSONATE_BIN` | 指定 `curl-impersonate` 二进制路径 |
| `CHATGPT2API_CURL_IMPERSONATE_URL` | 指定 `curl-impersonate` 下载地址 |
| `CHATGPT2API_IMAGE_TASK_WORKERS` | 图片任务后台 worker 数量，默认 `16` |
| `CHATGPT2API_IMAGE_TASK_QUEUE_SIZE` | worker 之外的排队容量，默认 `256` |
| `CHATGPT2API_TLS_PROFILE` | 指定 tls-client 指纹 profile |
| `CHATGPT2API_SENTINEL_MODE` | 默认 `goja`，仅需 Go 进程；可选 `browser` 或试验性 `node` |
| `CHATGPT2API_CHROME_BIN` | `browser` 模式使用的 Chrome/Chromium 可执行文件 |
| `CHATGPT2API_NODE_BIN` | `node` 模式使用的 Node.js 可执行文件 |
| `CHATGPT2API_UPSTREAM_TIMEZONE` | 指定上游请求和求解器共同使用的 IANA 时区，默认取系统时区 |
| `CHATGPT2API_USER_AGENT` | 覆盖默认 User-Agent |
| `CHATGPT2API_SEC_CH_UA` | 覆盖默认 `Sec-Ch-Ua` |
| `CHATGPT2API_TRACE` | 设置为 `0`、`false`、`off`、`no`、`quiet` 可关闭终端链路追踪 |
| `CHATGPT2API_NETWORK_TRACE` | 同样用于关闭终端链路追踪 |

Sentinel 需要 Turnstile token 时，默认由 Go 进程内的 Goja 执行挑战 VM；无需 Chrome、Node.js、`sentinel_solver` 或 `18080`。设置 `CHATGPT2API_SENTINEL_MODE=browser` 可改用本机 Chrome/Chromium，设置 `CHATGPT2API_SENTINEL_MODE=node` 可试验 Node.js 22 求解。上游请求与求解器应使用一致的时区和代理出口。

## 上游传输模式

默认使用 Go 内置 tls-client 模式。

也可以使用外部 `curl-impersonate`：

```bash
CHATGPT2API_UPSTREAM_TRANSPORT=curl-impersonate \
CHATGPT2API_CURL_IMPERSONATE_BIN=/path/to/curl_edge101 \
./start.sh --port 3000
```

发布包会包含对应目标的 `curl-impersonate` 二进制目录：

```text
data/bin/curl-impersonate/
```

如果目标包内没有 `curl_edge101`，请使用 `data/bin/curl-impersonate/` 中存在的 `curl_*` 启动脚本或 `curl-impersonate` 可执行文件。

## API 使用

所有 AI 接口都需要鉴权：

```http
Authorization: Bearer <auth-key>
```

### GET /v1/models

```bash
curl http://localhost:3000/v1/models \
  -H "Authorization: Bearer <auth-key>"
```

### POST /v1/chat/completions

非流式文本请求：

```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-5",
    "messages": [
      {"role": "user", "content": "写一段简短的项目介绍"}
    ]
  }'
```

流式文本请求：

```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-5",
    "stream": true,
    "messages": [
      {"role": "user", "content": "用三点说明这个项目"}
    ]
  }'
```

带工具请求：

```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-5",
    "messages": [
      {"role": "user", "content": "查询天气并返回摘要"}
    ],
    "tools": [
      {
        "type": "function",
        "function": {
          "name": "get_weather",
          "description": "查询城市天气",
          "parameters": {
            "type": "object",
            "properties": {
              "city": {"type": "string"}
            },
            "required": ["city"]
          }
        }
      }
    ]
  }'
```

### POST /v1/responses

```bash
curl http://localhost:3000/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-5",
    "input": "用一句话介绍 Nexora"
  }'
```

### POST /v1/messages

```bash
curl http://localhost:3000/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "gpt-5",
    "max_tokens": 512,
    "messages": [
      {"role": "user", "content": "解释这个项目的作用"}
    ]
  }'
```

### POST /v1/images/generations

```bash
curl http://localhost:3000/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "一只漂浮在太空里的猫，电影感，细节丰富",
    "n": 1,
    "size": "1:1",
    "resolution": "1k",
    "response_format": "b64_json"
  }'
```

### POST /v1/images/edits

```bash
curl http://localhost:3000/v1/images/edits \
  -H "Authorization: Bearer <auth-key>" \
  -F "model=gpt-image-2" \
  -F "prompt=把这张图改成赛博朋克夜景风格" \
  -F "n=1" \
  -F "size=9:16" \
  -F "resolution=1k" \
  -F "image=@./input.png"
```

### 聊天接口上传图片

`/v1/chat/completions` 支持 OpenAI 风格图片输入：

```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-5",
    "messages": [
      {
        "role": "user",
        "content": [
          {"type": "text", "text": "描述这张图片"},
          {
            "type": "image_url",
            "image_url": {
              "url": "data:image/png;base64,<base64>"
            }
          }
        ]
      }
    ]
  }'
```

## Web 管理面板

Web 面板包含以下页面和能力：

- 登录和用户鉴权。
- 账号池管理。
- 用户密钥管理。
- 在线画图。
- 图片任务。
- 图片管理。
- 图片标签。
- 公共画廊。
- 日志查询。
- 系统设置。
- 代理测试。
- 存储信息。

## 安卓客户端

项目可配合安卓客户端 Draw 使用。安卓客户端通过本项目后端完成文生图、图生图、画廊、作品管理等操作。

安卓客户端为独立 APK 发布，本仓库不包含其源码。后端 API 完全开放，可参考 `docs/android-integration.md` 自行实现客户端。

兼容性：

| 项 | 要求 |
|---|---|
| Android 最低版本 | 8.0，API 26 |
| 后端接口 | 需要支持 `/v1/images/*`、`/api/gallery/*`、`/api/me/images` |
| 网络 | 建议生产环境使用 HTTPS 反向代理 |

## 截图

号池管理：

![accounts](assets/accounts.png)

在线画图：

![image-studio](assets/image-studio.png)

日志管理：

![logs](assets/logs.png)

图片管理：

![image-manager](assets/image-manager.png)

## 已禁用或未包含的能力

当前 Go 后端不包含以下能力：

- 注册机真实逻辑。
- Cloudflare R2 自动备份真实逻辑。
- SQLite、PostgreSQL、Git 存储后端。
- CPA 远程导入真实同步。
- sub2api 远程导入真实同步。
- 非图片文件上传到 ChatGPT 网页端的真实文件协议。
- PDF 图片 OCR。
- Arkose token 自动求解。

相关接口会尽量返回 disabled 状态或保留兼容结构，避免前端页面崩溃。

## 常见问题

### 返回 401 或 Token 失效

说明账号 access token 可能已失效。请刷新或重新导入账号。

### 返回 429 或额度限制

说明上游账号触发限流或额度不足。系统会标记账号状态，并在恢复时间后重新尝试。

### 返回 403 HTML

说明请求可能触发上游风控。系统会识别该类错误，并在没有输出内容前尝试换号重试。仍然失败时，请检查账号质量、代理质量和账号指纹配置。

### 返回 upstream requires arkose token

说明上游要求 Arkose 校验。本项目当前不内置 Arkose token 求解。

### 上传图片后返回 422

新版已修复普通聊天图片消息格式。`multimodal_text.parts` 中的文字部分会作为字符串发送，图片部分作为 `image_asset_pointer` 对象发送。

## 安全建议

- 不要暴露未加鉴权的服务到公网。
- 生产环境建议使用 HTTPS 反向代理。
- 不要使用重要账号。
- 不要在日志、Issue、截图中公开 token、cookie、账号邮箱和代理地址。
- 建议为不同用户创建不同用户密钥，并配置额度。

## 开发命令

```bash
go test ./...
gofmt -w internal/app cmd/server
make run
make web
make package
make package-web
```

## 目录结构

```text
cmd/server/main.go                 服务入口
internal/app/server.go             HTTP 服务和路由
internal/app/v1.go                 OpenAI / Anthropic 兼容接口
internal/app/upstream.go           ChatGPT Web 上游协议
internal/app/codex.go              Codex 图片路线
internal/app/text_stream_retry.go  文本流换号重试
internal/app/image_pool.go         图片账号池和重试
internal/app/turnstile.go          Turnstile token 本地求解
internal/app/trace_log.go          终端链路追踪日志
internal/app/accounts.go           账号导入和刷新
internal/app/account_pool.go       账号选择和并发控制
internal/app/store.go              SQLite / PostgreSQL 存储与旧文件导入
web/                               前端源码
web_dist/                          前端构建产物
scripts/package_release.sh         发布打包脚本
data/                              运行数据目录
```

## 致谢与上游项目

本项目向以下项目和维护者致谢：

- [RemotePinee/ChatGPT2API](https://github.com/RemotePinee/ChatGPT2API)：本项目的主要致敬项目，也是前端实现的重要来源。
- [basketikun/chatgpt2api](https://github.com/basketikun/chatgpt2api)：ChatGPT2API 项目的上上游来源。

## 许可证

本项目使用 GNU Affero General Public License v3.0，完整文本见 `LICENSE`。

本项目包含或改写了上游 MIT 许可项目的代码、前端实现或设计思路。MIT 许可证允许在 AGPL v3 项目中再分发，但必须保留上游版权声明和许可声明。相关声明见 `THIRD_PARTY_NOTICES.md`。

## 友情链接

- [LINUX DO - 新的理想型社区](https://linux.do/)
