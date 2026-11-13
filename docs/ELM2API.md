# elm2api — ELM 网页版 → 本地 OpenAI API（独立版）

语言 / Language: [中文](ELM2API.md) | [English](#english)

`elm2api` 是一个**独立二进制**：只要一个 ELM 网页版的 `SESSION` cookie，就能在本机（默认 `127.0.0.1:8787`）启动 OpenAI 兼容 API。
它**不依赖 ds2api 的 DeepSeek 部分**，只复用 `internal/elmcore` 里与 ELM 相关的 WebSocket/STOMP 协议实现（`cmd/elm2api` 的依赖树里没有任何 `deepseek` 包）。

## 1. 功能

- **cookie 登录**：启动时配置 cookie，或运行中通过 `POST /api/sessions` 追加；也可在网页界面粘贴 cookie 登录。
- **显示网页端全部模型**：`GET /v1/models`（以及 `/` 网页表格）列出 ELM 网页里所有模型（31 个，含 Qwen/Llama/Mistral/GPT/Claude/Gemini 等），带名称、来源、上下文长度、是否免费。
- **按请求切换模型**：`/v1/chat/completions` 的 `model` 可以是任意 ELM 模型 id，也支持名称/简写（如 `Qwen 3.5 397B`、`qwen-3.5`、`llama-3.3`）；也可通过 `PUT /api/sessions/{id}/model` 给某个会话固定模型。
- **并发**：全局并发上限（默认 4）由信号量控制；同一模型可真正并行；不同模型只在"切模型 → 提交提问 → 收到首个增量"这一小段串行，实测 3 路并行 3 条回答 1.45s 完成。
- **流式/非流式**：`stream:true` 输出标准 SSE（含 `[DONE]`），thinking 内容以 `reasoning_content` 增量输出；`stream:false` 聚合返回。
- **请求间互不干扰**：每个 API 请求都会向 ELM 申请一个**新的 chat**（`GET /api/new-chat`），不会混进浏览器里打开的那个聊天，也不会把多个请求串到同一段上下文里；如果上游没有这个接口，会自动退回使用当前 chat。

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
  "check_interval_seconds": 300
}
```

环境变量：`ELM_SESSION`（多个用逗号分隔）、`ELM_LISTEN`、`ELM_BASE_URL`、`ELM_MODEL`、`ELM_MAX_CONCURRENCY`、`ELM_API_KEYS`、`ELM_CHECK_INTERVAL`、`ELM_CONFIG`。

- `sessions`：可以放多个 `SESSION` cookie（同一账号多次登录或不同账号），池会按负载选择，并在切换模型时优先选已在目标模型上的会话。
- `api_keys`：非空时 `/v1/*` 需要 `Authorization: Bearer <key>`（或 `?key=`），适合把服务暴露到局域网/公网时保护。
- `max_concurrency`：同时进行的 ELM 聊天数；`queue_wait_seconds` 为排队等待上限，超过返回 429/繁忙错误。

## 4. HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/models` | 网页端全部模型（OpenAI 格式 + 扩展字段） |
| GET | `/v1/models/{id}` | 单个模型 |
| POST | `/v1/chat/completions` | OpenAI 兼容聊天，`stream` 支持 |
| GET | `/healthz` | 健康检查 |
| GET | `/api/status` | 会话、预算、并发、模型数量 |
| GET/POST | `/api/sessions` | 列出 / 用 cookie 登录新会话 |
| DELETE | `/api/sessions/{id}` | 移除会话 |
| PUT | `/api/sessions/{id}/model` | 切换该会话的模型 |
| POST | `/api/models/refresh` | 强制刷新模型列表 |

管理接口（`/api/*`、`/`、`/ui/*`）只允许回环地址访问；`/v1/*` 可选 API key 保护。

## 5. 模型切换的真实含义

ELM 的模型是**会话级设置**（`PUT /api/settings` 的 `model`），聊天请求里只带提示词。因此：

- 单会话里不同模型的请求会在"切换模型 → 提交 → 收到首个 token"之间串行（拿到首个增量后即释放锁，实测不影响在途回答的模型）。
- 想彻底避免互相影响：为不同模型各配一个 `SESSION` cookie（同一账号多次登录即可），池会优先把请求路由到已在该模型的会话。
- 切换会同步影响用同一 cookie 的网页端当前模型；本服务在测试结束会切回原模型。
- 每个请求都会新建一个 ELM chat（见第 1 节），所以 API 请求不会和网页端正在聊的那条记录混在一起；需要多轮上下文时，把历史消息一起放进 `messages` 即可。

## 6. cookie 过期与重新登录（shell 会明确提示）

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

## 7. 限制

- 只支持纯文本对话；`tools`/`tool_choice`、文件上传、图片输入未实现（ELM 网页接口可以扩展，但当前版本未做）。
- 非 Hosted 模型（Claude/GPT/Gemini 等）会消耗 ELM 个人预算；Web Search 等工具费用同样计入。
- cookie 即账号凭证，等同于密码；不要提交仓库或发到聊天里。
- 并发上限受 ELM 后端实际承载影响，建议从 2–4 开始压测。

## 8. 排错（404 / API key / 空回复 / 串台）

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `GET /v1/models` 返回 **404** | 客户端把 base_url 拼成了别的路径（例如 base_url 写成 `.../v1/chat/completions`，或旧版本不接受 `/models`、结尾 `/`） | 新版本同时支持 `/v1/models`、`/models` 和末尾 `/`；base_url 只写到 `http://127.0.0.1:8787` 或 `http://127.0.0.1:8787/v1`。仍 404 时响应体会直接写出收到的 `method + path` 和可用接口 |
| `API key wrong` | 用的是 ds2api 的 `DS2API_ADMIN_KEY` 逻辑 | elm2api 默认**不校验** key；只有 `config.json` 里 `api_keys` 非空时才校验 `Authorization: Bearer <key>` |
| `EMPTY_RESPONSE` / "completed response with no content" | ELM 网页端偶发把新 websocket 断开（日志会出现 `wsasend ... forcibly closed`），旧版本已经先发了 200 再失败，客户端只看到空内容 | 新版本：① 建立聊天流失败会自动重试 3 次；② 拿到第一个增量前不发送 SSE 头，失败时返回真正的 4xx/5xx；③ 真的空回复返回 `empty_upstream_response` + `hint`，客户端重试即可 |
| 回答出现在浏览器打开的聊天里 / 多个请求串在一起 | 旧版本复用会话当前的 chat | 新版本每个请求都新建 chat（`GET /api/new-chat`） |
| 长 prompt（如 coding agent 的 system prompt）直接返回空回复 | ELM 网页端单条消息上限约 **16K 字符**，超长消息会被上游直接丢弃 | 服务会自动把过长 prompt 按行切成多条消息依次发送（同一 chat，仅最后一条的输出返回给客户端）；如果仍然失败会返回 413 `prompt_too_long` 并给出实际字符数 |

模型列表在网页端是 31 个；如果只看到 DeepSeek 模型，说明启动的是旧的 `ds2api-elm-*.exe`，不是 `elm2api`。

## 9. 构建与分发

```bash
go build -o elm2api ./cmd/elm2api          # Go 1.26
# 或
go build -ldflags="-s -w" -o elm2api ./cmd/elm2api
```

`dist/` 下有 Windows / Linux amd64 / Linux arm64 预编译二进制及打包。

---

## English

`elm2api` is a standalone binary that turns the ELM web chat into a local OpenAI-compatible API using only an ELM `SESSION` cookie. It does **not** depend on ds2api's DeepSeek code: `cmd/elm2api` only imports `internal/elmcore` (ELM WebSocket/STOMP client).

Features: cookie login (config, env, management API or the localhost page), listing every model the ELM web app offers (31 models), per-request model switching by id or friendly name, true concurrency (default limit 4), streaming SSE and non-streaming responses, plus a localhost dashboard with sessions, quota and the model table.

Run `./elm2api --config config.json` (listens on `127.0.0.1:8787`). Point any OpenAI client at `http://127.0.0.1:8787/v1`.

Notes: the ELM model is a session-level setting, so different-model requests serialize only the switch/submit window; configure several cookies to keep different models fully independent. Only plain text chat is implemented (no tools, files or images). Non-hosted models consume the ELM personal budget.
