# elm2api — 把 ELM 网页版变成 OpenAI 兼容 API

独立小服务：用浏览器里已登录的 ELM `SESSION` cookie，在本机开一个 OpenAI 兼容接口。
**不包含 ds2api / DeepSeek 的任何代码**，单个可执行文件即可运行（无需 Node、无需 npm、无需网页构建）。

- 默认监听 `http://127.0.0.1:8787`
- 上游 `https://elm.edina.ac.uk`
- 网页端全部 31 个模型都能用，请求里换 `model` 即可切换

## 1. 启动

Windows：

```bat
elm2api.exe
```

或双击 `start.bat`（读取同目录的 `config.json`）。

Linux：

```bash
./elm2api
```

首次启动如果还没有 cookie，终端会打印获取 cookie 的步骤，服务照常监听。

## 2. 登录（粘贴 cookie）

1. 浏览器打开 <https://elm.edina.ac.uk/elm> 并登录
2. F12 → Application(应用) → Cookies → `elm.edina.ac.uk` → 复制 `SESSION` 的值
3. 浏览器打开 <http://127.0.0.1:8787>，粘贴 cookie，点“登录”

也可以写进 `config.json` 的 `sessions[].cookie`，或设置环境变量 `ELM_SESSION`（`SESSION=...` 或只贴值）。

## 3. 客户端配置

| 项目 | 值 |
| --- | --- |
| base_url | `http://127.0.0.1:8787/v1`（写成 `http://127.0.0.1:8787` 也支持） |
| api_key | 随便填，例如 `sk-elm`（只有配置了 `api_keys` 才会校验） |
| model | `Qwen/Qwen3.5-397B-A17B-FP8`，或简名 `Qwen 3.5 397B` |

模型列表：`GET /v1/models`（网页端全部模型，含 Claude / GPT / Gemini / Llama 等）。

切换模型：请求里带不同的 `model` 即可。ELM 的模型是**会话级**设置，换模型会**继续同一个对话**（不新建 chat、不丢上下文）。
但**一个账号同一时刻只能跑一个聊天**：第二个请求会排队等第一个结束（日志打印 `queueing request`），要并行请配多个账号的 cookie。

同一个 session 复用同一个 ELM chat：上下文由 ELM 网页端历史承担，所以回答会出现在网页聊天记录里（正常现象）。
续聊时只把**新增的那一轮**发给 ELM；只有**回滚/新对话/换 session** 才新建 chat。chat id 与 Responses 的响应 id 都保存在 `state_file`（默认 `elm2api-state.json`），重启后能接着聊。

### 工具调用

`tools` / `tool_choice` 可用：服务把工具定义写进 prompt，模型按协议输出 JSON 调用，服务再翻译成标准 `tool_calls`（Chat Completions）或 `function_call`（Responses）。
带 tools 的流式请求会先缓冲整段回复再输出，非流式行为不变。

### Responses API

`POST /v1/responses` 支持 `previous_response_id`：把返回的 `resp_...` 下次传回来即可继续同一段对话，不需要重发历史。

```bash
curl http://127.0.0.1:8787/v1/responses -H "Content-Type: application/json" \
  -d '{"model":"qwen-3.5","input":"hi"}'
```

响应 id 失效（网页里删了 chat、state 重置）时会自动新建 chat；`store:false` 暂不支持续聊，等价于每次新建 chat。

## 4. 验证

```bash
curl http://127.0.0.1:8787/v1/models

curl http://127.0.0.1:8787/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3.5-397B-A17B-FP8","messages":[{"role":"user","content":"hi"}]}'
```

## 5. cookie 过期怎么办

终端会打印更新步骤；`/v1/*` 返回 401 `elm_session_expired`。**不用重启服务**：

```bash
curl -X POST http://127.0.0.1:8787/api/sessions -H "Content-Type: application/json" \
  -d '{"name":"main","cookie":"SESSION=..."}'
```

网页首页也能直接粘贴新 cookie。

## 6. 常见问题

- **404**：base_url 指向 `http://127.0.0.1:8787` 或 `http://127.0.0.1:8787/v1` 都可以，
  不要写成 `.../v1/chat/completions` 当 base_url。
- **API key wrong**：默认不校验 key；只有 `config.json` 的 `api_keys` 非空时才校验。
- **空回复（EMPTY_RESPONSE）**：ELM 网页端偶发断流，服务会自动重试；仍失败就直接重试请求（重试会复用同一个 chat，上下文不丢）。
- **长 prompt 超限**：ELM 网页端单条消息上限约 16K 字符；超过时服务会自动把 prompt 切成多条消息依次发送（同一 chat），不需要你手动处理。
- **工具调用**：通过 prompt 协议模拟（ELM 无原生 function calling），参数由模型生成，客户端仍应校验；模型偶尔会把调用写成普通文本，此时按普通回答返回。
- **只支持纯文本对话**：不支持文件上传、图片输入。
- **回答跑到浏览器里打开的聊天中**：这是预期行为（API 伪装自网页聊天），历史会留在 ELM 网页端；回滚/新对话/换 session 时才会新建 chat，换模型不会。
- **第二个请求很慢 / 日志出现 `queueing request`**：一个 ELM 账号同一时刻只能跑一个聊天，正常排队；要并行请配多个账号。
- **`previous_response_id` 失效**：会当作新对话自动新建 chat（日志会说明）。
- **模型列表里只有 DeepSeek**：说明启动的是旧的 `ds2api-elm-*.exe`，请运行本包的 `elm2api.exe`（29 个网页端模型 + Qwen 等，共 31 个）。

## 7. 配置文件

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | 监听地址；要局域网访问可改 `0.0.0.0:8787` |
| `base_url` | `https://elm.edina.ac.uk` | 上游地址 |
| `sessions` | `[]` | 一个或多个 `{"name":"main","cookie":"SESSION=..."}` |
| `default_model` | 第一个模型 | 请求不传 model 时使用 |
| `max_concurrency` | 4 | 并发上限 |
| `queue_wait_seconds` | 30 | 超出并发时的排队时间 |
| `api_keys` | `[]` | 非空时 `/v1` 需要 `Authorization: Bearer <key>` |
| `model_list_ttl_seconds` | 300 | 模型列表缓存时间 |
| `check_interval_seconds` | 300 | cookie 巡检间隔，0 关闭 |
| `state_file` | `elm2api-state.json` | 记录 chat id / Responses 映射（不含完整 prompt），留空则不落盘 |

环境变量：`ELM_CONFIG`、`ELM_LISTEN`、`ELM_BASE_URL`、`ELM_MODEL`、`ELM_MAX_CONCURRENCY`、`ELM_API_KEYS`、`ELM_CHECK_INTERVAL`、`ELM_SESSION`。