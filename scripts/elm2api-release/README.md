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

切换模型：请求里带不同的 `model` 即可。注意 ELM 的模型是**会话级**设置，同一个 cookie 同时只能跑一个模型；
需要并行不同模型时，在 `sessions` 里配多个 cookie。

每个 API 请求都会向 ELM 申请一个**新的 chat**，不会混进你在浏览器里打开的那个聊天；多轮上下文请把历史消息一起放进 `messages`。

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
- **空回复（EMPTY_RESPONSE）**：ELM 网页端偶发断流，服务会自动重试；仍失败就直接重试请求。
- **长 prompt 超限**：ELM 网页端单条消息上限约 16K 字符；超过时服务会自动把 prompt 切成多条消息依次发送（同一 chat），不需要你手动处理。
- **只支持纯文本对话**：不支持工具调用、文件上传、图片输入。
- **回答跑到浏览器里打开的聊天中**：旧版本复用会话当前 chat；新版本每个请求新建 chat，已修复。
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

环境变量：`ELM_CONFIG`、`ELM_LISTEN`、`ELM_BASE_URL`、`ELM_MODEL`、`ELM_MAX_CONCURRENCY`、`ELM_API_KEYS`、`ELM_CHECK_INTERVAL`、`ELM_SESSION`。
