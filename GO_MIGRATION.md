# Go 迁移说明

本仓库已加入 Go 后端实现，入口为 `cmd/server/main.go`，默认监听 `:3000`，Docker 内监听 `:80`。

## 本次迁移范围

- 保留 `web/` Next.js 前端，生产环境由 Go 后端直接服务 `web_dist/`。
- 默认使用 SQLite，支持通过 PostgreSQL `DATABASE_URL` 切换数据库。
- 旧业务 JSON 文件首次启动时导入数据库，原件保留作备份；生成图片默认存放在 `data/images/`，可在系统设置中改为 WebDAV、Cloudflare R2 或双份保存。远端图片索引保存在 `data/image_index.json`，备份时需与 `config.json` 一起保留。
- 注册任务、邮箱 provider 和账号 2FA 已接入；注册成功后账号与密码、2FA 状态存入数据库。启动前需在管理面板配置可用的邮箱 provider。
- 移除 Cloudflare R2 备份真实逻辑，备份接口返回 disabled/空列表。
- 保留管理面板核心端点：账号、用户密钥、设置、日志、图片管理、画廊、图片任务、聊天会话。
- 保留 OpenAI/Anthropic 兼容端点：`/v1/models`、`/v1/images/*`、`/v1/chat/completions`、`/v1/responses`、`/v1/messages`。

## 重要说明

当前 Go 版本已经接入真实 ChatGPT Web 上游链路：

- `/v1/images/generations` 通过账号池 access_token 调用 ChatGPT Web 图片生成；
- `/v1/images/edits` 会先上传参考图，再调用 ChatGPT Web 图片链路；
- `/v1/chat/completions`、`/v1/responses`、`/v1/messages` 会通过 ChatGPT Web SSE 文本链路返回；
- `/api/chat/stream` 使用同一条真实文本 SSE 链路；
- `/api/accounts/refresh` 会调用 `/backend-api/me` 刷新账号基础信息。

上游 HTTP 客户端支持两种模式：

- 默认：纯 Go `github.com/bogdanfinn/tls-client`，默认 `Chrome_146` profile，不使用 C/C++、curl 或 CGO。
- 完全一致模式：外部 `curl-impersonate` 子进程，由 Go 通过 `exec.Command` 启动，不使用 CGO。

环境变量：

- `CHATGPT2API_UPSTREAM_TRANSPORT=tls-client` 默认纯 Go 模式
- `CHATGPT2API_UPSTREAM_TRANSPORT=curl-impersonate` 使用外部 curl-impersonate
- `CHATGPT2API_CURL_IMPERSONATE_BIN=/path/to/curl_edge101` 指定二进制
- `CHATGPT2API_TLS_PROFILE=chrome_146|chrome_133|chrome_120|...` 仅 tls-client 模式有效
- `CHATGPT2API_USER_AGENT=<自定义 UA>`

如果上游触发 Arkose，Go 版会返回真实上游错误；Arkose token 求解仍未内置。

## 本地运行

```bash
go test ./...
make run
```

然后访问：

- Web：`http://localhost:3000`
- API：`http://localhost:3000/v1`

## 构建前端

```bash
make web
make run
```

## Docker

```bash
docker build -t nexora .
docker run --rm -p 3000:80 -v "$PWD/data:/app/data" -e CHATGPT2API_AUTH_KEY='replace-with-your-key' nexora
```

镜像不包含本地 `config.json`。也可以挂载配置文件到 `/app/config.json`；`/app/data` 需对容器内 `app` 用户可写。完整的升级和备份步骤见 [README 部署说明](README.md#部署与升级)。

## 数据库存储

- SQLite：默认 `data/chatgpt2api.db`，可通过 `DATABASE_URL=sqlite:///绝对路径/chatgpt2api.db` 指定位置。
- PostgreSQL：`DATABASE_URL=postgresql://user:password@host:5432/dbname`。
- 旧 `data/*.json` 业务数据和 `logs.jsonl` 在数据库首次初始化时自动导入；导入失败会阻止启动。保留旧文件以便核对和备份。
- 旧版 Go SQL 数据库中以整份文档保存的账号和用户密钥会在启动时迁入独立记录表；升级时先停止旧服务进程。
