# ELM Web 上游（网页对话 → API）

语言 / Language: [中文](ELM.md) | [English](#english)

本仓库支持把 **ELM（https://elm.edina.ac.uk）网页版聊天** 直接当作上游，对外仍然暴露标准的 OpenAI / Claude / Gemini / Responses 接口。
适合“只有网页账号、没有或不愿使用 API Key”的场景；对 University of Edinburgh 用户，ELM Hosted 模型（如 Qwen 3.5 397B）在网页端是免费且不占个人预算的。

## 1. 工作原理（逆向结论）

- 网页聊天走 **STOMP over WebSocket**：`wss://elm.edina.ac.uk/ws`，STOMP CONNECT 头带 `host:/` 和 `X-CSRF-TOKEN`（由 `GET /api/csrf` 取得）。
- 每轮对话：
  1. `GET /api/chat-id` 取一个新的 chatId；
  2. `SUBSCRIBE /topic/query-<chatId>`；
  3. `SEND /app/queue/query-<chatId>`，body 为 `{"query":"<纯文本提示词>","promptId":<空模板 id>}`；`promptId` 对应 ELM 账号里的一个 prompt 模板，空模板（"Empty Prompt"）可避免机构默认 prompt 干扰工具协议，传 `null` 时 ELM 会套用默认 prompt；
  4. 从订阅主题收到 `USER` 回显、`AI` 增量帧（`reply` 为增量文本）、以及 `CHAT_FINISHED` 结束帧。
- 认证是浏览器 Cookie `SESSION=...`（HttpOnly）；模型是**会话级设置**（`GET/PUT /api/settings` 的 `model` 字段），不在单次聊天请求里。
- `internal/elmcore` 负责以上协议，并把结果转换成 ds2api 现有的 DeepSeek SSE 格式，因此 OpenAI/Claude/Gemini/Responses、工具调用、历史等**全部沿用原有流水线**。

## 2. 配置

```json
{
  "keys": ["your-api-key"],
  "accounts": [
    {
      "name": "elm",
      "email": "you@ed.ac.uk",
      "token": "SESSION=..."
    }
  ],
  "upstream": "elm",
  "elm": {
    "base_url": "https://elm.edina.ac.uk",
    "model": "Qwen/Qwen3.5-397B-A17B-FP8"
  },
  "current_input_file": { "enabled": false }
}
```

- `upstream`：`deepseek`（默认）或 `elm`。
- `accounts[].token`：登录 ELM 后的 `SESSION` cookie，可写 `SESSION=...` 或只写值。该字段里放的是一个浏览器会话凭证。
- `elm.model`：ELM 模型 id（见网页 `Model Guide` 或 `GET /api/settings/models`）；留空则沿用该会话当前选中的模型。
- `current_input_file.enabled` 建议关闭：ELM 网页上游没有 ds2api 所需的文件上传接口。

### 获取 SESSION cookie

1. 在浏览器登录 ELM；
2. 打开开发者工具 → Application/存储 → Cookies → `https://elm.edina.ac.uk` → 复制 `SESSION` 的值；
3. 作为 `accounts[].token` 填入（**不要提交到仓库**，等同于密码）。

> DSH 电脑操作插件也可以直接读到：浏览器通道的“Keep website sign-ins”开启后，用有头浏览器登录一次即可。

## 3. 调用示例

服务启动后与 DeepSeek 上游完全一致：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

## 4. 限制与注意事项

- **文件上传/检索**：不支持（`UploadFile` 返回错误）；`/v1/files` 不可用。
- **会话删除**：`DeleteSessionForToken` / `DeleteAllSessionsForToken` 为 no-op；ELM 会话随浏览器会话过期。
- **模型切换**：为会话级设置，切换会同时影响使用同一 cookie 的网页登录状态。
- **并发/预算**：并发仍由 ds2api 账号池控制；非 Hosted 模型（Claude/GPT/Gemini 等）会消耗 ELM 个人预算，可用 `GET /api/quota/status/summary` 查看。
- **凭证安全**：`SESSION` cookie 即账号凭证；配置写回、日志、仓库中都不要泄露。

## 5. 在其他设备上使用（不依赖本机 DSH）

这套桥接**不依赖 DSH**，DSH 只用于浏览器登录/查看预算；运行时就是普通 ds2api 服务。三种分发方式：

1. **git clone / 补丁**
   ```bash
   git clone <this-repo> && cd ds2api
   git checkout feat/elm-web-upstream      # 或 git apply elm-web-upstream.patch
   go build -o ds2api ./cmd/ds2api          # Go 1.26
   ./ds2api -config config.json             # 或 DS2API_CONFIG_PATH=./config.json
   ```
2. **直接复制二进制**（`dist/` 下有 windows-amd64 / linux-amd64 构建）+ `config.json`，无需 Go 环境。
3. **Docker**：仓库自带 `Dockerfile`（内含 webui 与 Go 构建阶段）：
   ```bash
   docker build -t ds2api-elm .
   docker run -p 8080:8080 -v "$PWD/config.json:/app/config.json" ds2api-elm
   ```

任一方式，其他设备上的 OpenAI 兼容客户端只需：

- `base_url` = `http://<该设备或服务器>:8080/v1`
- `api_key` = `config.json` 里的 `keys[0]`

**关于 cookie**：`SESSION` 是 ELM 服务端会话，不与浏览器所在设备绑定，因此同一 cookie 可以在多台机器/服务器上复用，直到过期或退出登录；过期后在该设备的浏览器重新登录 ELM 取新值即可。**不要**把 cookie 提交进 git。

> DSH 作为客户端时：在 DSH 的 provider 配置里把 `baseURL` 指向这套 ds2api（`http://<host>:8080/v1`），`apiKeyEnv` 指向 `config.json` 里的 key 即可，此时 DSH 与企业内其他设备完全解耦。

---

## English

The repo can use the **ELM web chat** (`https://elm.edina.ac.uk`) as an upstream while still serving the standard OpenAI / Claude / Gemini / Responses APIs.

How it works: the web app speaks **STOMP over WebSocket** (`wss://elm.edina.ac.uk/ws`). A chat round is `GET /api/chat-id` → `SUBSCRIBE /topic/query-<chatId>` → `SEND /app/queue/query-<chatId>` with `{"query": "...", "promptId": <empty template id>}` (the id selects a prompt template; `null` applies ELM's default institution prompt); replies arrive as `USER` echo, incremental `AI` frames and a final `CHAT_FINISHED`. Auth is the browser `SESSION` cookie, and the model is a session-level setting (`/api/settings`).

`internal/elmcore` implements the protocol and converts the stream into the existing DeepSeek SSE shape, so every ds2api surface keeps working.

Configuration: set `"upstream": "elm"`, put the `SESSION` cookie into `accounts[].token`, and pick an ELM model id in `elm.model`.

Limitations: no file upload/retrieval, session deletion is a no-op, model switching is session-wide, and non-hosted models consume the personal ELM budget.
