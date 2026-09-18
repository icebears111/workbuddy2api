# WorkBuddy2API

> **WorkBuddy2API** —— 把腾讯 **WorkBuddy / CodeBuddy**（`copilot.tencent.com`）官方账号与 API 统一转成 **「OpenAI Chat 兼容」网关**。
> Go 单二进制、零外部依赖，可选 Docker。主打两项自动化：**每日 08:00–10:00 自动签到**、**账号智能调度（自动切号 / 冷却 / 停用）**，另带敏感内容脱敏与完整 Web 管理后台。

本项目是我们自研并长期运行维护的开源项目，欢迎在遵守腾讯服务条款的前提下学习与自用。

---

## 界面预览

![管理后台](docs/admin-ui.png)

启动后访问 `http://<host>:7865/admin` 即可看到上述界面（页面随二进制 go:embed 内嵌，不依赖外置静态目录）。

---

## 功能特性

- 🌅 **每日 08:00–10:00 自动签到** — 默认每天在 **08:00–10:00 本地窗口内的随机时刻**自动执行官方签到：各账号分配互不相同的随机时刻、**逐个错开**，避免整点齐发；约 **+100 积分/天**、连签第 **7 天 +1000**。每账号当天「已签 / 未签」看板可见，也支持管理页「一键签到」。
- 🔀 **账号调度策略** — 多账号池自动轮转 + **配额感知自动切号**（某账号对某模型的当日免费额度 / 频率用尽 → 本次自动换下一账号，不整号误伤）+ 429 / 配额耗尽冷却 + 凭证失效与 token 到期**自动禁用**，全程无需人工盯号。
- 🔑 **多凭证账号池** — 支持 `ck_` 静态 API Key 与 OAuth 登录 token（realm `cn` = copilot.tencent.com / `saas` = www.codebuddy.ai）混挂，请求自动轮转。
- 🧊 **冷却与可靠性状态机** — 429 软冷却 / 配额耗尽硬冷却 / 凭证失效自动禁用，并持久化到 `data/state.json`。
- 🔁 **配额感知自动切号** — 某账号对**某模型**的当日免费额度 / 频率用尽时，仅把本次请求切到下一账号重试，**不整号冷却**（不误伤同号其它模型）；真·配额不足仍走 12h 硬冷却后再切号。
- 📡 **流式 + 非流式** — 上游只支持流式，本服务自动聚合；`reasoning_content` 可按需透传或剥离。
- 🗂 **动态模型列表** — 每小时动态拉取上游可用模型，失败回退静态表；`fallback_model` 兜底客户端传入的未知模型名。
- 🕵️ **敏感内容脱敏** — `features` 段的零宽脱敏与字段剥离，缓解 Claude Code / Codex CLI 合规模板被后端内容审核误拦。
- 📊 **用量跟踪** — 依据上游每次对话返回的 usage 累计本地用量并落盘（`usage_file`），`quota_limit` 作看门狗上限。
- 🖥 **Web 管理后台**（自带）— 账号健康 / 额度 / 套餐明细 / 签到状态 / 对话测试 / 接入指引，一站式运维。
- 🐳 **一键部署** — 多阶段 Docker 镜像 + healthcheck + docker-compose，也支持裸二进制运行。

---

## 快速开始

### 你需要什么
- 一个或多个腾讯 **CodeBuddy / WorkBuddy** 账号（OAuth 登录型账号可参与每日签到）。
- Go ≥ 1.23（仅裸二进制编译需要）。

### 1. 获取 / 编译
```bash
git clone <你的仓库地址> workbuddy2api && cd workbuddy2api

# 方式 A：裸二进制
go build -o workbuddy2api ./cmd/server

# 方式 B：Docker
docker compose up -d --build
```

### 2. 配置
```bash
cp config.example.json config.json
```
编辑 `config.json`：**`api_key` 必填，改成一段强随机字符串**（客户端访问本网关的鉴权 Key；留空 = 不鉴权，不安全，不推荐生产使用）。`features` / `schedule` 等均有默认值，按需调整。

### 3. 启动
```bash
./workbuddy2api -config config.json   # 或 docker compose up -d --build
# 监听端口默认 7865，管理后台 http://localhost:7865/admin
```

### 4. 验证
```bash
curl http://localhost:7865/v1/models \
  -H "Authorization: Bearer <你的网关api_key>"

curl http://localhost:7865/v1/chat/completions \
  -H "Authorization: Bearer <你的网关api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"hy4-preview","messages":[{"role":"user","content":"你好"}]}'
```

---

## 添加账号

支持两种凭证、三种入口：

| 凭证 | 说明 | 入口 |
|---|---|---|
| `ck_` API Key | `copilot.tencent.com` 控制台生成 | `addkey.sh` / `addkey.ps1`，或后台「手动添加」 |
| OAuth 登录 token | 浏览器授权获得 | 后台「自动授权登录」一键绑号（推荐，可每日签到） |

```bash
# Linux / macOS：把 ck_ Key 写入 auths/
./addkey.sh ck_xxxx  我的账号

# Windows PowerShell
.\addkey.ps1 -Key ck_xxxx -Name 我的账号
```
脚本运行后到 `/admin` 点「重载目录」（或重启服务）即可生效。更省事的做法：启动后打开 `/admin` →「自动授权登录」→ 选「中国版授权 (copilot.tencent.com)」或「国际版授权 (www.codebuddy.ai)」，浏览器登录即完成绑号。

---

## 配置与 env

### config.json 配置示例

```jsonc
{
  "listen": ":7865",                  // 监听地址
  "api_key": "一段强随机字符串",        // 客户端访问网关的 Key（必填）
  "auth_dir": "./auths",              // 凭证目录
  "state_file": "./data/state.json",  // 账号池/冷却状态持久化
  "usage_file": "./data/usage.json",  // 本地用量累计
  "quota_limit": 500,                 // 用量看门狗上限
  "fallback_model": "hy4-preview",    // 上游未知模型名兜底

  "upstream": {
    "base": "https://copilot.tencent.com", // 国内；海外可填 www.codebuddy.ai
    "env": "internal",                // internal | public | ioa | cloudhosted | selfhosted
    "timeout_seconds": 180
  },
  "oauth": { "token_url": "", "client_id": "" }, // 可 refresh 的 token 型凭证续期时填写

  "cooldown": {
    "hard_credit": "12h",             // 配额/余额不足 → 硬冷却
    "soft_rate": "60s",               // 429 → 软冷却
    "err_threshold": 5,               // 连续错误阈值
    "err_cooldown": "10m"
  },

  "features": {                       // 敏感内容脱敏 / 响应规整
    "desensitize": true,              // 对上游请求做零宽脱敏（缓解合规模板被误拦）
    "desensitize_tools": true,        // 对 tools 的 description/title 做脱敏
    "strip_tool_metadata": false,     // true = 直接删 description/title（过审率高但模型看不到说明）
    "hide_reasoning": false,          // true = 流式剥离 reasoning_content
    "min_reasoning_effort": "",       // 思考深度下限 low/medium/high/max；留空 = 透传

    // 非 OpenAI 协议开关，默认 false = 返回 410 Gone（见「客户端接入」）
    "enable_anthropic_protocol": false,  // true = 开放 /v1/messages（Claude Code 可直连）
    "enable_responses_protocol": false   // true = 开放 /v1/responses（Codex CLI 可直连）
  },

  "schedule": {
    "check_hours": [0, 6, 12, 18],    // 保活探活整点（凭证失效/到期自动禁用）
    "auto_checkin": true,             // 是否开启每日自动签到
    "checkin_start_hour": 8,          // 自动签到窗口起点（本地时区，含）
    "checkin_end_hour": 10            // 自动签到窗口终点（本地时区，不含）
  }
}
```

> 说明：上例 `features.hide_reasoning` 显式设为 **false**，是为了**保留 `reasoning_content`** 便于直连观测 / 调试。若 `features` 段整体缺省，则走代码内置默认，此时 `hide_reasoning=true`（更贴近标准 OpenAI 流）。实际行为以你 config.json 里写的值为准，本文档仅作说明，不改动任何默认值。

### 环境变量覆盖（前缀 `CB2A_`）

下列配置项支持同名环境变量覆盖（`CB2A_` + 大写下划线），优先级高于 config.json；未列出的项（如 `usage_file`、`quota_limit`、`schedule.check_hours`）仅能通过 config.json 配置：

`CB2A_LISTEN` / `CB2A_API_KEY` / `CB2A_AUTH_DIR` / `CB2A_FALLBACK_MODEL` / `CB2A_STATE_FILE`
`CB2A_UPSTREAM_BASE` / `CB2A_UPSTREAM_ENV` / `CB2A_OAUTH_TOKEN_URL` / `CB2A_OAUTH_CLIENT_ID`
`CB2A_HARD_CREDIT` / `CB2A_SOFT_RATE` / `CB2A_ERR_THRESHOLD` / `CB2A_ERR_COOLDOWN` / `CB2A_TIMEOUT_SECONDS`
`CB2A_AUTO_CHECKIN` / `CB2A_CHECKIN_START_HOUR` / `CB2A_CHECKIN_END_HOUR`
`CB2A_DESENSITIZE` / `CB2A_DESENSITIZE_TOOLS` / `CB2A_STRIP_TOOL_METADATA` / `CB2A_HIDE_REASONING` / `CB2A_MIN_REASONING_EFFORT`
`CB2A_ENABLE_ANTHROPIC_PROTOCOL` / `CB2A_ENABLE_RESPONSES_PROTOCOL`

---

## 客户端接入

本网关**默认只对外提供 OpenAI Chat**，仅两个端点：

- `POST /v1/chat/completions`
- `GET /v1/models`

**Base URL**：`http://<host>:7865/v1`，模型名填 `/v1/models` 返回的 `id`，鉴权头 `Authorization: Bearer <你的网关api_key>`。

| 客户端 | 端点 | 协议 |
|---|---|---|
| Cherry Studio / LobeChat / NextChat / Open WebUI 等 | `/v1/chat/completions` | OpenAI Chat，直连 |
| Claude Code / Claude Desktop | `/v1/messages` | Anthropic Messages，需打开开关 |
| Codex CLI | `/v1/responses` | OpenAI Responses，需打开开关 |

> ⚠️ 默认状态下 `/v1/messages`、`/v1/messages/count_tokens`、`/v1/responses`（Anthropic / Responses 协议）**已下线**，请求返回 **HTTP 410** 并提示改用转换代理。此时 **Claude Code 不能直连本网关**：请先在中间套一层转换层（如 CC Switch / claude-code-router / LiteLLM），把 Anthropic / Responses 转成 OpenAI chat，再让转换层把 Base URL 指向本网关的 `http://<host>:7865/v1`。

### 可选：直接开放 Anthropic / Responses 协议（免转换层）

不想再套转换层时，在 `config.json` 的 `features` 段打开对应开关即可（默认 `false`）：

```json
{ "features": { "enable_anthropic_protocol": true, "enable_responses_protocol": false } }
```

也支持环境变量 `CB2A_ENABLE_ANTHROPIC_PROTOCOL` / `CB2A_ENABLE_RESPONSES_PROTOCOL`（`true` 或 `1`）。两个开关**互相独立**，可只开其一。

打开后 Claude Code 可**直连本网关**，不需要任何中间进程：

```powershell
$env:ANTHROPIC_BASE_URL   = 'http://127.0.0.1:7865'   # 注意：不带 /v1
$env:ANTHROPIC_AUTH_TOKEN = '<你的网关api_key>'
claude
```

> **为什么这比转换层更适合「多实例分流」**：转换层（CC Switch）对同一个 app 类型只暴露**一个**本地端口，因此两个终端没法同时指向两个网关；而协议做进网关之后，**每个终端只要在启动时指定自己的实例就锁死一个**，两个实例各用各的 `auths/`，账号池天然互不重复。
>
> **注意用 `--settings` 而非环境变量**：`~/.claude/settings.json` 里 `env` 段的优先级
> **高于进程环境变量**（Claude Code 自身设定），若该文件已被转换层写入
> `ANTHROPIC_BASE_URL`，`$env:` 会被覆盖回去。命令行参数优先级更高：
>
> ```powershell
> claude --settings .\profiles\instance-a.json   # 指向实例 A
> claude --settings .\profiles\instance-b.json   # 指向实例 B
> ```
>
> 两个 profile 内容形如 `{"env":{"ANTHROPIC_BASE_URL":"http://<host>:7865","ANTHROPIC_AUTH_TOKEN":"<该实例的 api_key>"}}`。
> **恢复历史对话时也要带上 `--settings`**（`claude --settings <file> -c`）：
> 会话文件存在本机、两个实例共享，但请求去向由启动参数决定，漏了会走转换层。
>
> 另需注意模型映射：客户端若不发上游认识的模型名（如 Claude Code 发 `claude-opus-5`），
> 网关会按 `fallback_model` 回落；profile 里建议显式指定
> `ANTHROPIC_MODEL` 等变量，避免落到能力较弱的兜底模型上。

---

## 管理与自动签到

管理后台 `/admin`（页面 go:embed 内嵌，随二进制分发）：

- **顶部统计** — 账号总数 / 可用账号 / 冷却中 / 已停用 / 积分剩余 / 本期已用。
- **账号与额度** — 每账号一行：健康 badge、实时额度、行首「▸」展开各套餐 cycle 明细；今日**已签 / 未签**角标；行内操作：**测试 / 测对话 / 停用 ↔ 启用 / 删除**。
- **自动授权登录** — 中国版（cn） / 国际版（saas）OAuth 浏览器一键绑号；两域账号分开管理。
- **一键签到** — 右上「☑ 一键签到」，对池内所有 OAuth 登录账号逐个执行官方每日签到。
- **模型 / 对话测试** — 模型标签 + 流式 / 非流式对话测试（支持指定账号或自动轮询）。
- **客户端接入** — 一键复制 Base URL / API Key；接入指引会**跟随运行时配置**提示 Claude Code / Codex CLI 是「可直连」还是「需转换层」。

### 自动调度说明

配置 `schedule.auto_checkin: true`（默认开启）后，服务会在每天 **08:00–10:00（本地时区）窗口内的随机时刻** 自动签到：各账号分配**互不相同的随机时刻**、逐个错开执行，避免整点齐发。每账号当天「已签 / 未签」状态持久化在 `data/checkin_state.json`，后台逐账号展示。

> **签到域名按凭证域自动选择**：签到接口的两侧边缘（APISIX）各自只认本域签发的 token，用错域会拿到 **401 + HTML 错误页**（不是业务 JSON）。因此
> `realm=cn`（copilot.tencent.com 登录）→ `www.workbuddy.cn`，
> `realm=saas`（www.codebuddy.ai 登录）→ `www.codebuddy.ai`。
> 实测同一枚 saas token：打 codebuddy.ai 返回 200，打 workbuddy.cn 返回 `401 invalid_token`。
> 另：401/403 一律判为「凭证被拒绝，需重新登录」并**自动停用该账号**（不再受响应体是否为 JSON 影响）。
>
> **`code==10001` 在两个域语义不同**，不能只看 code：CN 域是「今天已签到，请明天再来」（幂等命中，算已签），
> SaaS 域是「签到活动未开启或已过期」（**并未签到**）。后者按未签处理，否则后台会把从未签到的 saas 号显示成「已签」。
> 实测 saas 域当前没有可用的签到活动，因此 saas 账号只走转发、拿不到签到积分——这是上游侧的现状，不是网关故障。

配套的保活机制：按 `schedule.check_hours`（默认 `[0, 6, 12, 18]`，即每天 0 / 6 / 12 / 18 点）整点对账号探活；凭证失效（401/403）自动禁用；token 到期（`ExpiresAt`）也会被自动禁用并在后台提示**重新登录**。

> 安全提示：管理页会把真实网关 Key 注入前端（`window.__API_KEY__`），请把 `/admin` 置于可信网络，或反代后自行加鉴权。

---

## 项目结构

```
cmd/server/            主服务入口（config 加载 + main）
cmd/check/             凭证诊断 CLI（go run ./cmd/check -key ck_xxx）
internal/authcb/       OAuth 设备授权登录（cn / saas）
internal/checkin/      每日签到（官方接口），当天状态持久化
internal/cred/         凭证加载 / 保存 / 续期
internal/pool/         账号池 + 冷却状态机 + data/state.json
internal/scheduler/    保活探活 + 到期自动禁用 + 每日自动签到（随机错开）
internal/server/       路由（OpenAI Chat + 可选 Anthropic / Responses）+ 管理 API + 后台页面（admin.html, go:embed）
internal/upstream/     上游客户端 + 错误分类 + 模型 + 脱敏 + 协议转换（Anthropic / Responses）
internal/usage/        本地用量累计 + quota 看门狗
auths/                 凭证目录（.gitignore，不入库）
data/                  运行时状态（state/usage/checkin_state，.gitignore）
addkey.sh / addkey.ps1 添加 ck_ 凭证脚本
deploy.sh              服务器更新脚本（git pull + 重建 + 自检 + 回滚）
docker-compose.server.yml  服务器生产编排（icebears-net，不发布宿主端口）
docs/admin-ui.png      管理后台界面截图
Dockerfile / docker-compose.yml / config.example.json
LICENSE
```

---

## 部署与更新（随时可更新）

`deploy.sh` 面向「已部署的服务器也要能随时更新」这一场景，幂等、可反复执行：

```bash
cd /opt/codebuddy2api
./deploy.sh              # git pull + docker compose 重建 + 健康检查 + 应用自检
./deploy.sh --no-pull    # 只用当前代码重建（离线/调试）
./deploy.sh --rollback   # 一键回滚到上一个镜像
```

要点：

- **数据与代码分离**：`auths/`（凭证）、`data/`（冷却/用量/签到状态）、`config.json`
  都是挂载进容器的宿主机文件，重建容器**不丢账号、不丢状态、key 不变**。
- **自动备份镜像**：每次重建前把当前 `codebuddy2api:latest` 打上
  `codebuddy2api:rollback-<时间戳>` tag，回滚即 `docker tag` + `up -d --no-build`。
- **服务器只读部署**：服务器上不要直接改源码（会被 `git pull` 覆盖）；
  改动走「本地 commit + push → 服务器 `./deploy.sh`」。
- **不在服务器跑 git 工作区改动的场景**：部署脚本只 `git pull`，
  拉取失败会因 `set -e` 中止，不会带着半套代码重建。

---

## 开发

```bash
go build ./...          # 编译全部
go test ./...           # 运行单元 / 协议 / 池测试
go run ./cmd/check -key ck_xxx   # 探测某个 Key 可用性与可见模型（或设环境变量 CB2A_KEY）
```

- 代码无第三方运行时依赖（标准库实现），测试覆盖池状态机、协议转换、错误分类等核心逻辑。
- 欢迎提交 Issue 与 PR；改动请确保 `go test ./...` 通过。

---

## 免责声明

本项目仅供学习研究使用。使用时请遵守腾讯 CodeBuddy / WorkBuddy 的服务条款与账号政策，自行承担相应风险；作者不对因使用本项目产生的任何直接或间接损失负责。

## License

[MIT](./LICENSE)
