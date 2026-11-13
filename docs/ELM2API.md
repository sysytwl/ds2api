# elm2api — ELM 网页版 → 本地 OpenAI API（独立版）

语言 / Language: [中文](ELM2API.md) | [English](#english)

`elm2api` 是一个**独立二进制**：只要一个 ELM 网页版的 `SESSION` cookie，就能在本机（默认 `127.0.0.1:8787`）启动 OpenAI 兼容 API。
它**不依赖 ds2api 的 DeepSeek 部分**，只复用 `internal/elmcore` 里与 ELM 相关的 WebSocket/STOMP 协议实现（`cmd/elm2api` 的依赖树里没有任何 `deepseek` 包）。

## 1. 功能

- **cookie 登录**：启动时配置 cookie，或运行中通过 `POST /api/sessions` 追加；也可在网页界面粘贴 cookie 登录。
- **显示网页端全部模型**：`GET /v1/models`（以及 `/` 网页表格）列出 ELM 网页里所有模型（31 个，含 Qwen/Llama/Mistral/GPT/Claude/Gemini 等），带名称、来源、上下文长度、是否免费。
- **按请求切换模型**：`/v1/chat/completions` 的 `model` 可以是任意 ELM 模型 id，也支持名称/简写（如 `Qwen 3.5 397B`、`qwen-3.5`、`llama-3.3`）；也可通过 `PUT /api/sessions/{id}/model` 给某个会话固定模型。
- **并发**：`max_concurrency`（默认 4）是服务总并发上限；但**一个 ELM 账号同一时刻只能跑一个聊天**（网页端本身如此，第二个对话要等第一个结束），所以同一 cookie 的请求会在服务内排队，不同 cookie（不同账号）才真正并行。
- **流式/非流式**：`stream:true` 输出标准 SSE（含 `[DONE]`），thinking 内容以 `reasoning_content` 增量输出；`stream:false` 聚合返回。
- **对话连续、聊天可追溯**：把网页聊天当上下文存储——每段对话对应一个 ELM chat，续聊时只把**新增的那一轮**发给 ELM，网页里能直接看到每次 API 对话。换模型**不**新建 chat（ELM 的模型是会话级设置）；只有客户端重发的内容对不上任何已知对话（编辑/回滚/换话题）或换 session 时才新建 chat。
- **可同时保留多段对话**：一个 session 记住最近 8 个 chat（主对话、subagent 等），谁续聊就回到谁的 chat，不会串台。
- **Responses API**：`POST /v1/responses` 支持 `previous_response_id`——响应 id 映射到 ELM chat，客户端只发新增输入即可继续；id 失效（网页里删了 chat、state 被重置）自动新建 chat；`store:false` 暂不支持续聊，按“每次新建 chat”处理。
- **工具调用**：`tools` / `tool_choice` 可用。ELM 网页没有原生 function calling，服务把工具定义写进 prompt，再把模型输出的 JSON 调用翻译成标准 `tool_calls`（Chat Completions）或 `function_call`（Responses）。

## 2. 快速开始

```bash
# 1) 从浏览器登录 ELM 后取 cookie：DevTools → Application → Cookies → elm.edina.ac.uk → SESSION
export ELM_SESSION="SESSION=..."           # 或写进 config.json
./elm2api --config config.json             # 默认监听 127.0.0.1:8787

# 2) 查看网页端全部模型
curl http://127.0.0.1:8787/v1/models

# 3) 用任意模型聊天（OpenAI 兼容）
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen3.5-397B-A17B-FP8","messages":[{"role":"user","content":"hello"}],"stream":true}'

# 4) Responses API（推荐给 agent：只发新增输入，历史留在 ELM 网页端）
curl http://127.0.0.1:8787/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-3.5","input":"hello"}'
# 返回里的 id（resp_...）下次当作 previous_response_id 传回来，即可继续同一段对话
```

浏览器打开 <http://127.0.0.1:8787> 可以看到：已登录会话、预算、当前模型、全部模型列表，并可粘贴 cookie 登录/切换模型。

客户端里的 `base_url` 写 `http://127.0.0.1:8787/v1` 或 `http://127.0.0.1:8787`（带不带 `/v1`、末尾带不带 `/` 都支持）；
`api_key` 没有配置 `api_keys` 时随便填即可（例如 `sk-elm`），不会报 “API key wrong”。

## 3. 配置

```json
{
  "listen": "127.0.0.1:8787",
  "base_url": "https://elm.edina.ac.uk",
  "sessions": [
    { "name": "main", "cookie": "SESSION=..." }
  ],
  "default_model": "Qwen/Qwen3.5-397B-A17B-FP8",
  "max_concurrency": 4,
  "queue_wait_seconds": 30,
  "api_keys": [],
  "model_list_ttl_seconds": 300,
  "check_interval_seconds": 300,
  "state_file": "elm2api-state.json"
}
```

环境变量：`ELM_SESSION`（多个用逗号分隔）、`ELM_LISTEN`、`ELM_BASE_URL`、`ELM_MODEL`、`ELM_MAX_CONCURRENCY`、`ELM_API_KEYS`、`ELM_CHECK_INTERVAL`、`ELM_STATE_FILE`、`ELM_CONFIG`。

- `state_file`：记录每个 session 记住的 chat（id、模型、上一轮 prompt 的长度与 sha256、120 字预览）以及 Responses 的 `resp_*` → chat 映射；重启后或切回某个 session 时能接着原来的 ELM 聊天（留空则不落盘）。不会写入完整 prompt。

- `sessions`：可以放多个 `SESSION` cookie（不同账号时才能真正并行；同一账号通常只能拿到一个可用 cookie），池会按负载选择，并在切换模型时优先选已在目标模型上的会话。
- `api_keys`：非空时 `/v1/*` 需要 `Authorization: Bearer <key>`（或 `?key=`），适合把服务暴露到局域网/公网时保护。
- `max_concurrency`：服务端总并发上限；同一 session 内部的请求由“回合锁”串行（ELM 一个账号只能跑一个聊天），排队等待只受客户端超时限制。

## 4. HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/models` | 网页端全部模型（OpenAI 格式 + 扩展字段） |
| GET | `/v1/models/{id}` | 单个模型 |
| POST | `/v1/chat/completions` | OpenAI 兼容聊天，`stream` 支持 |
| POST | `/v1/responses` | OpenAI Responses API，支持 `previous_response_id` / `instructions` / `stream` |
| GET | `/healthz` | 健康检查 |
| GET | `/api/status` | 会话、预算、并发、模型数量 |
| GET/POST | `/api/sessions` | 列出 / 用 cookie 登录新会话 |
| DELETE | `/api/sessions/{id}` | 移除会话 |
| PUT | `/api/sessions/{id}/model` | 切换该会话的模型 |
| POST | `/api/models/refresh` | 强制刷新模型列表 |

管理接口（`/api/*`、`/`、`/ui/*`）只允许回环地址访问；`/v1/*` 可选 API key 保护。

## 5. 模型切换与账号并发

ELM 的模型是**会话级设置**（`PUT /api/settings` 的 `model`），聊天请求里只带提示词。因此：

- 请求里换 `model` 只是调一次设置接口，**继续同一个 chat**，上下文不丢（ELM 网页端的模型选择器也是这个行为）。
- **一个账号同一时刻只能跑一个聊天**：第二个对话会等第一个结束。服务用每个 session 的“回合锁”把并发请求排队（日志打印 `queueing request`），而不是让两个回答在同一个网页聊天里交错。
- `max_concurrency` 只在配了多个账号 cookie 时才有意义；单 cookie 时实际并发恒为 1。想真正并行只能配多个账号（同一账号重复登录拿不到第二个可用 cookie）。
- 切换会同步影响用同一 cookie 的网页端当前模型。
- 回答会出现在 ELM 网页端的聊天记录里，这是"用网页聊天伪装成 API"的正常现象，也方便回看/审计。

## 6. 对话是怎么复用的（chat 规则）

服务记的是每段对话**最后一次发给 ELM 的 prompt 长度 + sha256**（不存原文），据此判断新请求属于哪段对话：

| 情况 | 行为 |
|---|---|
| 新 prompt 以上次 prompt 开头且更长（OpenAI 客户端通常重发完整历史） | 复用该 chat，只发多出来的那一轮 |
| prompt 与上次完全相同（客户端重试空回复） | 复用该 chat，原样重发；网页里会多一条相同的提问，但上下文保住了 |
| prompt 对不上任何已知对话（编辑、回滚、换话题） | 新建 chat（网页聊天无法回滚，宁可重开也不串台） |
| 换模型 | **不**新建 chat，继续当前对话 |
| 换 session | 各 session 有各自的 chat 记录，切回来还能接上 |
| 同一 session 有多段对话（subagent、第二个窗口） | 按“最长前缀匹配”回到各自的 chat，最多记 8 段 |

`state_file` 只保存 chat id、模型、prompt 长度/哈希和 120 字预览，重启后仍能接上原来的聊天；`GET /api/status` 与首页会列出每个 session 记住的 chat。

## 7. 工具调用（文本协议 → tool_calls）

ELM 的网页聊天只能收发文本，没有原生 function calling。服务的做法是：

1. 把 `tools` 的 name / description / JSON Schema 写进 prompt 开头，并要求模型**需要调用时只输出一个 JSON 代码块**：

   ```json
   {"tool_calls":[{"name":"pwsh","arguments":{"command":"git status"}}]}
   ```

2. 收到回复后解析这段调用，转成标准 OpenAI 结构：Chat Completions 返回 `message.tool_calls` + `finish_reason:"tool_calls"`；Responses 返回 `function_call` 输出项。
3. 客户端把工具结果以 `role:"tool"`（Responses 里是 `function_call_output`）发回时，服务渲染成 `Tool result (<工具名>): ...` 继续对话。

兼容的模型输出格式（解析器都接受）：JSON 代码块、裸 JSON、`<tool_call>...</tool_call>`，以及部分模型会写的 YAML 形式：

```
pwsh
description: Show git working tree status
command: git status
workdir: C:/work
```

长 prompt 的关键点：coding agent 的 system prompt + 历史很容易超过 16K，此时 prompt 会被切成多条 ELM 消息发送，而工具协议本来只在 prompt 开头。**最后一片（真正被回答的那一条）会重新附带一遍工具协议**，否则模型看到的只有对话尾巴、没有工具定义，就会用自然语言回答"我去执行一下"而不是给出调用。

行为细节：

- `tool_choice: "none"` 时不注入协议、也不解析；`tool_choice` 指定某个函数时会要求模型必须调用它。
- **流式请求带 tools 时会先缓冲整段回复**（否则无法判断 JSON 调用是否完整），结束后一次性发出文本或 `tool_calls`；不带 tools 的请求仍然逐字流式输出。
- 参数由模型生成，客户端仍应校验 schema；模型偶尔可能把调用写成普通文本，此时按普通回答返回。
- 工具结果过长同样受 16K 消息上限约束，会被自动分段。

## 8. cookie 过期与重新登录（shell 会明确提示）

ELM 的 `SESSION` cookie 会过期（登出、服务端会话到期、改密码等）。服务不会静默失败，而是在**终端日志**里直接给出处理方式，并在接口/页面里同步提示：

```text
2026/11/12 23:51:11 elm2api: ⚠ ELM SESSION cookie 已过期/失效（会话 "main"，you@ed.ac.uk）
2026/11/12 23:51:11 elm2api: 获取/更新 ELM SESSION cookie：浏览器登录 https://elm.edina.ac.uk/elm → F12 →
  Application(应用) → Cookies → elm.edina.ac.uk → 复制 SESSION 的值；然后二选一：
  (a) 写进 config.json 的 sessions[].cookie（或设置环境变量 ELM_SESSION）后重启服务；
  (b) 不重启：curl -X POST http://127.0.0.1:8787/api/sessions -H 'Content-Type: application/json' \
      -d '{"name":"main","cookie":"SESSION=..."}'
2026/11/12 23:51:11 elm2api: 服务仍在 http://127.0.0.1:8787 运行，修复 cookie 后无需重启（方式 b）
```

行为细节：

- **启动时**：cookie 失效不会让进程退出，服务照常监听；日志打印上面这段提示，并把该会话标记为"已过期"（网页上显示红条）。
- **请求时**：`/v1/*` 返回 HTTP 401，错误体为 `code = elm_session_expired`，并附 `hint` 字段（就是上面的更新步骤）；不会让客户端误以为在重试真流量。
- **自动巡检**：`check_interval_seconds`（默认 300 秒，0 关闭）会定期用 `/api/open/is-logged-in` 复验 cookie；一旦失效立刻在 shell 打印提示，恢复后打印"✔ 已恢复"。
- **重新登录无需重启**：`POST /api/sessions` 传入新 cookie 即生效；旧会话条目可 `DELETE /api/sessions/{id}` 移除。
- `GET /api/status` 的 `warnings` 字段和网页首页红条会同步展示过期会话。

## 9. 限制

- 只支持纯文本对话；文件上传、图片输入未实现。工具调用是通过 prompt 协议模拟的（见上一节），不是原生 function calling。
- `POST /v1/responses` 的 `tools` 支持 function 类型；其他内置工具类型会被忽略。
- **一个账号同一时刻只能跑一个聊天**：并发请求会排队，这是 ELM 侧的限制，不是本服务的开关。
- Responses 的 `usage` 是按字符估算的（ELM 网页端不返回 token 用量）；`store:false` 不支持续聊，等价于每次新建 chat。
- 非 Hosted 模型（Claude/GPT/Gemini 等）会消耗 ELM 个人预算；Web Search 等工具费用同样计入。
- cookie 即账号凭证，等同于密码；不要提交仓库或发到聊天里。

## 10. 排错（404 / API key / 空回复 / 排队）

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `GET /v1/models` 返回 **404** | 客户端把 base_url 拼成了别的路径（例如 base_url 写成 `.../v1/chat/completions`，或旧版本不接受 `/models`、结尾 `/`） | 新版本同时支持 `/v1/models`、`/models` 和末尾 `/`；base_url 只写到 `http://127.0.0.1:8787` 或 `http://127.0.0.1:8787/v1`。仍 404 时响应体会直接写出收到的 `method + path` 和可用接口 |
| `API key wrong` | 用的是 ds2api 的 `DS2API_ADMIN_KEY` 逻辑 | elm2api 默认**不校验** key；只有 `config.json` 里 `api_keys` 非空时才校验 `Authorization: Bearer <key>` |
| `EMPTY_RESPONSE` / "completed response with no content" | ELM 网页端偶发把新 websocket 断开（日志会出现 `wsasend ... forcibly closed`），旧版本已经先发了 200 再失败，客户端只看到空内容 | 新版本：① 建立聊天流失败会自动重试 3 次；② 拿到第一个增量前不发送 SSE 头，失败时返回真正的 4xx/5xx；③ 真的空回复返回 `empty_upstream_response` + `hint`，客户端重试即可 |
| 回答出现在浏览器打开的聊天里 | 预期行为：API 是把网页聊天伪装成 API | 复用该对话的 chat，历史留在网页端可查；换模型不新建 chat，只有回滚/新对话/换 session 才会新建 |
| 第二个请求很慢，或日志出现 `queueing request` | 一个 ELM 账号同一时刻只能跑一个聊天，第二个对话要等第一个结束 | 正常排队；要并行就配多个账号的 cookie |
| 长 prompt（如 coding agent 的 system prompt）直接返回空回复 | ELM 网页端单条消息上限约 **16K 字符**，超长消息会被上游直接丢弃 | 服务会自动把过长 prompt 按行切成多条消息依次发送（同一 chat，仅最后一条的输出返回给客户端）；如果仍然失败会返回 413 `prompt_too_long` 并给出实际字符数 |
| 模型把工具调用写成了普通文本（例如回"我去执行一下"，DSH 里只显示一段 JSON） | 常见原因是 prompt 超过 16K 被切分，工具协议留在了第一片、最后一片没有（已修复：最后一片会重述协议）；其次是模型没按协议输出 | 升级到当前版本；日志里 `tools=N prompt=… reply=… tool_calls=0` 表示确实没解析到调用，`tool_calls>=1` 表示已翻译成标准结构。仍失败可换更强的模型或精简 system prompt |
| `previous_response_id` 失效 / 网页里把 chat 删了 | 映射记在 `state_file`，chat 被删或 state 被重置后找不到 | 自动新建 chat 并把这次当作新对话（日志打印 `previous_response_id ... is unknown`）；想接回旧上下文就重发完整历史 |
| 请求一直被客户端取消 | ELM 网页端在长 prompt / 大历史时首字较慢，客户端超时设得太短 | 把客户端超时调到 120s 以上；`ELM_DEBUG=1` 可看到逐帧日志 |

模型列表在网页端是 31 个；如果只看到 DeepSeek 模型，说明启动的是旧的 `ds2api-elm-*.exe`，不是 `elm2api`。

## 11. 构建与分发

```bash
go build -o elm2api ./cmd/elm2api          # Go 1.26
# 或
go build -ldflags="-s -w" -o elm2api ./cmd/elm2api
```

`dist/` 下有 Windows / Linux amd64 / Linux arm64 预编译二进制及打包。

---

## English

`elm2api` is a standalone binary that turns the ELM web chat into a local OpenAI-compatible API using only an ELM `SESSION` cookie. It does **not** depend on ds2api's DeepSeek code: `cmd/elm2api` only imports `internal/elmcore` (ELM WebSocket/STOMP client).

Features: cookie login (config, env, management API or the localhost page), every model the ELM web app offers (31 models), per-request model switching by id or friendly name, `POST /v1/chat/completions` and `POST /v1/responses` (with `previous_response_id`), streaming SSE, and a localhost dashboard with sessions, quota, remembered chats and the model table.

Run `./elm2api --config config.json` (listens on `127.0.0.1:8787`). Point any OpenAI client at `http://127.0.0.1:8787/v1`.

Notes: the ELM model is a session-level setting, so switching models keeps the same chat. One ELM account runs one chat at a time, so requests on the same cookie queue; configure several accounts' cookies for real parallelism. Only plain text chat is implemented (no tools, files or images). Non-hosted models consume the ELM personal budget.