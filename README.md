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

Go 服务启动时会自动读取当前目录的 `.env`，例如 `DATABASE_URL=postgresql:///nexora?host=/tmp`；数据库迁移命令也会读取该文件。已有的进程环境变量优先于 `.env`。`start.sh` 的 `--port`、`--curl` 和 `--tls` 仍由脚本控制。`.env` 已被 Git 忽略；修改后需重新启动服务才会生效。

Docker 使用 PostgreSQL 18 时，先在项目根目录创建 `.env`：

```dotenv
POSTGRES_DB=nexora
POSTGRES_USER=nexora
POSTGRES_PASSWORD=change_this_password
```

然后启动 PostgreSQL 覆盖配置：

```bash
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d --build
```

应用会等待 PostgreSQL 健康检查通过后启动，数据库数据保存在 Docker volume `nexora-postgres-data`。已有 SQLite 数据不会自动复制到 PostgreSQL。迁移时先停止写入 SQLite 的服务，备份 `data/`，用 `sqlite3 data/chatgpt2api.db '.backup /tmp/nexora.db'` 制作一致性快照，再在**空的** PostgreSQL 数据库上运行：

```bash
DATABASE_URL='postgresql://user:password@host:5432/nexora' \
  go run ./cmd/migrate-store -sqlite /tmp/nexora.db
```

命令会复制全部 `app_*` 表，在提交事务前逐表比对行数和内容摘要，并保留源 SQLite 文件。图片文件仍在 `data/images/`，切换服务时必须继续提供相同的 `data/` 目录；根目录 `config.json` 也需保留。若迁移期间 SQLite 服务继续写入，应在停写后重新制作快照并迁移到新的空库，不能直接使用旧副本切换。

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

注册中断的记录会在号池页作为“注册未完成”的异常账号显示，可单个或批量修复。号池页面仅显示密码是否已保存，密码设置结果由注册或修复流程记录。本地保存的密码可能只是待提交的候选值；旧记录没有设置成功的状态时，由修复流程按实际步骤处理。修复按上游当前步骤接续：进入设置密码页时才提交候选密码，进入邮箱验证页时读取原邮箱验证码，再补完资料并保存 token。若 Platform 接续返回 `registration_disallowed`，会使用同一邮箱、候选密码及代理，从 ChatGPT Web 建立独立授权并按实际页面完成注册；这一步需要本机 Chrome/Chromium。验证码登录成功本身不会确认密码，只有注册、密码验证或密码设置接口明确接受后才确认。若密码尚未确认，会检查添加密码资格，通过浏览器完成邮箱验证并补设或重置密码，然后启用并保存 2FA，全部完成后才转为正常。没有候选密码时会生成并保存一个，供接续使用。浏览器密码设置需要本机 Chrome 支持的代理地址；带认证的远程代理应通过 Mihomo 本地监听端口接入。任何步骤失败都会保留已取得的 token、候选密码及异常状态供重试；邮箱保留供接续。无 token 的记录不参与聊天、生图账号调度。

已入池且仍有注册待接续记录的账号，先通过 `/backend-api/me` 校验现有 Token 和邮箱。会话有效时直接补全密码与 2FA，减少重复登录和发码；Token 失效时再重新授权，身份不匹配或网络故障时保留原记录。Platform 取码期间邮箱接口正常但一直返回无新验证码时，可回退至正常的 ChatGPT Web 浏览器授权；已有此类失败记录的未入池账号会直接使用该浏览器分支。邮箱服务故障、取消、限流及封号不会触发无验证码回退。密码重置授权若进入密码登录页，会沿页面的“忘记密码”入口处理邮箱表单或确认页。2FA 设置失败或待完成的账号保持异常；接口明确返回 `account_deactivated` 时保存为封号。已成功保存的密码、Token 和 2FA 状态不会因后续步骤失败而丢弃。

未登录浏览器的密码重置使用普通登录页的“忘记密码”流程，避免混用已登录账号的 `post_login_password_reset` 授权。密码接口成功后立即保存确认状态；普通重置完成页 `reset_password_success` 不会自动返回 ChatGPT，程序会再使用保存密码及已有 2FA 登录获取令牌。`password_already_used` 本身不能证明当前密码正确，仍需实际密码登录成功才确认。上游要求 `/verify-your-identity` 时直接记录需手动身份验证，保留原记录，不作为缺少 callback 重复提交资料。

未入池记录在号池显示具体处理状态：需要身份验证、ChatGPT Web 明确拒绝注册，或仍可接续。整批异常修复会跳过前两类并记录数量，避免重复发码和提交资料；邮箱、候选密码及错误详情继续保留。管理员在官方页面处理后，可在号池对该记录点击“重新检查”，或导入已完成的账号。仅有此前 Platform 拒绝的诊断不会被当作 Web 最终拒绝。管理接口 `/api/accounts/repair/start` 默认跳过这两类，响应的 `skipped` 为跳过数量；主动重新检查时传 `retry_blocked: true`。

待接续邮箱和未入池密码记录统一显示在号池的“异常”列表，按邮箱合并，并计入号池和仪表盘的异常数量。注册页不再单独展示这两类记录；密码查看、复制及修复入口都在号池中。封号使用独立状态，仅计入封号数量，不出现在异常筛选或“移除异常账号”的范围内。

2FA 登记接口返回密钥后、激活成功前，账号仍处于待修复状态。导入或编辑账号时，只有密码已确认、2FA 状态为 `enabled` 且密钥已保存，才会清除待接续标记；单有待激活密钥不会清除修复记录。

手动导入的邮箱若仍有注册待接续记录，且密码或 2FA 尚未完整保存，会保留为“异常”，继续出现在修复列表中；启动时也会校正此类旧记录。没有注册待接续记录的旧正常账号不受此规则影响。同一密码已成功设置时，重复导入或编辑不会丢失该结果；更换密码或显式导入 `password_status` 时按新值处理。仅标记 2FA 已启用却缺少密钥的待修复账号，需要手动补录原密钥。

浏览器接续和密码设置按独立方法执行，每个步骤都会记录开始、完成或失败及耗时。主要步骤为 `authorize`（建立授权）、`submit_email`（提交邮箱）、`register_password` / `verify_password`（注册或验证密码）、`wait_email_code`（取码）、`validate_email_code`（校验验证码）、`email_code_navigation`（验证后的跳转）、`create_profile`（完善资料）、`set_password`（补设或重置密码）、`read_session`（读取同邮箱登录会话）。验证码、密码和资料提交会检查相应接口的响应；日志仅保留 HTTP 状态、错误码和下一步类型/路径，不保存响应正文、验证码或授权回调参数。页面检测忽略隐藏或禁用控件，提交按钮限定在当前控件的表单内；验证码已由页面自动提交时不再主动点击。等码一分钟仍无结果时可通过页面重发一次；服务器明确返回验证码错误或过期时，也可重新发码并校验一次，其他错误不会套用此重试。已入池账号的直接授权若因旧会话或缺少 callback code 失败，会清理一次授权 Cookie 后重试；仍无法取得会话时，只有保存了邮箱和密码的账号才回退到浏览器登录，成功后沿用原账号记录继续设置 2FA，不会新建重复账号。上游返回 `account_deactivated` 的账号统一记为封号并退出可修复队列。

修复单个账号的总超时按三轮验证码等待加六分钟授权、资料和 2FA 处理时间计算，取启用 provider 中最长的等待配置；例如等待验证码配置为 180 秒时，总预算为 15 分钟，每轮等码仍独立限时 180 秒。iCloud 取码日志显示请求次数、`no_code` 次数及最近接口结果，`no_code` 表示接口尚未返回本轮验证码，不能据此统计整个收件箱的邮件数量。

添加或重置密码时，还会检查浏览器发送响应的账号邮箱和 `email_otp_verification` 步骤；仅收到 HTTP 200 不会直接进入空等。首轮最多等码 60 秒，超时后遵守页面按钮的冷却状态，最多点击一次“重新发送”，校验新响应后更新邮件时间边界并等待新验证码。发送接口接受请求不等于邮件已投递，重发仍无验证码时会保留异常账号及已取得的凭据。

注册页和号池页的异常修复均使用注册配置中的线程数作为并发上限。不同账号可同时登录和设置 2FA，同一邮箱会去重并保持互斥。注册页“线程”显示正在处理的数量与配置上限；停止修复后不再提交待处理账号，等待已运行任务退出。

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
  "refresh_account_interval_minute": 5,
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

### 账号额度自动刷新

`refresh_account_interval_minute` 控制后台扫描间隔，默认 5 分钟。可在设置页修改，保存后动态调整下一次扫描，无需重启。启动服务时会立即检查一次，后续每轮处理完成后等待配置的间隔再扫描。

| 账号情况 | 检查时机 |
| --- | --- |
| 限流，或正常但本地已知额度为 0 | 每轮扫描 |
| 正常且到达上游返回的额度重置时间 | 下一轮扫描 |
| 其他正常账号，包括额度未知的账号 | 距上次成功检查满 24 小时后的下一轮扫描 |
| 尚无 `quota_checked_at` 的旧账号 | 升级启动后的首次扫描 |
| 异常、封号、禁用或没有 access token | 不参与额度自动刷新 |

额度以真实上游响应为准，不在零点直接补满。后台刷新最多并发 8 个账号，与手动刷新共用并发限制，并复用令牌续期及重新登录流程。普通网络错误会保留当前额度，后续扫描重试；确认令牌无效且重新授权失败时，账号可能转入异常状态，需要修复。刷新期间账号已被使用、状态变更或凭据更新时，会放弃过时的结果。

“刷新全部”跳过异常和封号账号；管理 API 在未指定账号时也采用此规则。封号使用独立统计和筛选，不计入异常列表。批量修复仅处理可修复的异常账号和注册待接续记录。

号池卡片、列表的成功/失败次数，以及健康状态条，统一使用当前额度窗口内的调用统计。窗口边界采用上游额度重置时间，新窗口重新计数；临时限流产生的本地恢复时间不替代已记录的窗口边界。API 的 `window_success`、`window_fail` 是窗口统计，`window_started_at`、`window_reset_at` 标记统计范围；原 `success`、`fail` 保留历史累计语义。旧账号从升级后首次统计开始计数，历史累计次数不会直接计入当前窗口；没有上游重置时间时保留本轮统计，取得窗口边界后再按实际时间切换。

账号额度刷新与 Python `chatgpt2api` 一样，读取 `/backend-api/conversation/init` 的 `limits_progress`，使用 `image_gen.remaining` 更新剩余次数，使用 `reset_after` 更新重置时间；顶部剩余额度根据刷新后的正常账号重新汇总。卡片按剩余比例显示，例如 `100% · 5/5`。上游未返回单个账号的总额度，分母 `image_quota_baseline` 是同一重置窗口内读取到的最高剩余次数，悬停说明此参考口径；首次检查或重置窗口变化时重新建立基准，额度未知时清除基准，刷新失败时保留原值。旧账号尚未建立基准时暂用当前剩余次数，不再使用历史字段 `initial_quota` 或按套餐固定的 25/50。该百分比不代表上游实际已用比例，重置时间也不代表确认恢复到历史最高额度。

升级时需重建并重启后端，使新逻辑生效；首次扫描会为旧账号补充额度检查时间，不需要手工修改数据库。已有配置值会继续保留，例如原来设为 `60`，仍按 60 分钟扫描；要使用更短间隔，请改为 `5`。

### 注册使用 Mihomo 分组

注册页展开配置，在“注册代理”下点击“从 Mihomo 选择分组”。填写控制 API 地址及密钥后点击“读取分组”，选择任意分组并“保存并用于注册”。本机 Clash Verge 可留空自动识别；`http://127.0.0.1:7897` 是代理入口，不能用于读取分组，控制 API 应使用 Mihomo 的 `external-controller` 地址或 `unix:///socket/path`。

此功能要求 Mihomo 与 Nexora 同机运行，且 Nexora 能读取其运行 YAML 配置；非 Clash Verge 用户在高级配置中指定运行配置路径。Docker 部署必须确保两者共享网络命名空间，并挂载运行配置及控制 socket；仅填写宿主机的 `127.0.0.1` 不会连接到容器外的代理。

分组内的具体节点参与轮询，子分组入口、直连及拒绝节点跳过。Nexora 为每个节点向现有核心加载一个注册专用监听端口（默认从 `18001` 开始），每个注册任务选一次并固定出口；不另启动核心，不切换系统代理或 Nexora 默认/备用代理。保存会加载当前运行配置并保留控制 API 中的端口、模式、TUN 状态及原有选择；原配置文件不改写。注册或修复仍在运行时不能切换分组。起始端口冲突时可在高级配置中修改。

选择保存在 Nexora 数据库，后续启动注册前会重新读取所选分组并恢复注册出口，因此核心或服务重启后无需手动启动额外进程。订阅节点变化会在下一次启动注册时同步。Mihomo 由用户原有的 Clash/Mihomo 服务运行；Nexora 不负责启动它。部署此功能后需重建前端并重启后端。

“修复异常账号”和号池批量修复也使用注册页配置的代理池：每个账号选一次节点，登录、令牌续期、接续注册和补设 2FA 共用该出口；2FA 遇到网络故障时仍可切换健康节点重试。注册控制台会显示 `修复代理节点[节点ID]`。未配置注册代理时，已入池账号沿用账号及全局代理配置，待接续账号使用直连。修复不会覆盖账号保存的代理配置。修复前会检查 Mihomo 注册出口，核心重启导致端口失效时自动恢复。

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

`/v1/images/generations` 和 `/v1/images/edits` 的响应包含 `created`、`data` 和标准图片用量结构的 `usage`（`input_tokens`、`input_tokens_details`、`output_tokens`、`output_tokens_details`、`total_tokens`）。上游提供 token 用量时优先使用上游数值。上游未提供时，响应顶层另带 `usage_estimated=true`、`usage_estimation_method=chatgpt2api_patch_proxy` 和 `usage_text_tokenizer=o200k_base`；本地使用 `o200k_base` 计算发送的提示词，参考 chatgpt2api 的 32px patch 规则计算输入图和实际输出图。输出按当前上游使用的 `quality=auto` 估算。该估算并非 GPT Image 模型的实际计费 token。若文本编码失败，`usage_text_tokenizer=character_fallback` 表示采用字符数回退。多图响应汇总各张图片的用量；流式生成在每个 `image.generation.result` 事件附带对应 `usage` 和估算标记。

图片模型支持 `gpt-image-2.6-sunburst` 和 `gpt-image-2.6-flare`。版本模型名会原样传给 ChatGPT Web；`gpt-image-2` 使用 `auto`。指定版本模型时，`2k` / `4k` 作为提示词中的尺寸要求发送，不会切换到 Codex，实际尺寸由上游决定；`gpt-image-2` 的高分辨率请求仍使用原有 Codex 路径。

生成 GIF 时，在提示词中说明动画内容、时长和循环方式。若上游返回当前轮次的 GIF 附件，接口会下载真实 GIF，按 `.gif` 保存并返回其 URL 或原始 `b64_json`；不会用静态预览图代替下载失败的 GIF。本地、WebDAV 和 R2 均保留原始动画，缩略图为静态预览。模型可能无法按要求生成动画，不能仅凭提示词保证输出格式。

```bash
curl http://localhost:3000/v1/images/generations \
  -H "Authorization: Bearer <auth-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-image-2.6-sunburst","prompt":"生成红球上下弹跳的 GIF 动画，2 秒，无限循环，提供 GIF 下载文件","n":1,"response_format":"url"}'
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

### 第二天账号额度仍未更新

先确认运行的是包含额度自动刷新修复的后端，并检查设置页的账号刷新间隔。额度按上游重置时间恢复，不保证当地时间零点刷新。等待一轮扫描，或在号池手动刷新对应账号；后台日志中的 `[account-quota-watcher]` 会显示检查数量及成功、失败数量。若刷新失败，检查该账号的令牌和代理；已转为异常的账号需先执行异常修复。

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
