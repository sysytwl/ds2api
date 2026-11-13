# elm2api

把 ELM 网页版（<https://elm.edina.ac.uk/elm>）的聊天界面包装成本地的 OpenAI 兼容 API：
只用浏览器里已登录的 `SESSION` cookie，不需要 DeepSeek、不需要 Node、不需要网页构建。

- `GET /v1/models`：网页端全部模型（31 个），支持按 id / 名称 / 简写切换
- `POST /v1/chat/completions`：OpenAI Chat Completions，支持流式、工具调用
- `POST /v1/responses`：OpenAI Responses API，`previous_response_id` 直接映射到 ELM 的 chat
- 上下文存在 ELM 网页端：每段对话对应一个 chat，续聊只发新增内容，网页里可回看
- 工具调用：模型按提示词里的协议输出 JSON 调用，服务翻译成标准 `tool_calls` / `function_call`
- ELM 没有原生 function calling，且默认机构 prompt 会压过工具协议：服务首次聊天前会选中账号里的 “Empty Prompt” 模板，并在每条消息里带上它的 id（`empty_prompt: false` 可关闭）

完整说明（登录、配置、chat 复用规则、并发限制、排错）见 [docs/ELM2API.md](docs/ELM2API.md)。

## 快速开始

```bash
# 1) 浏览器登录 ELM 后取 cookie：DevTools → Application → Cookies → elm.edina.ac.uk → SESSION
export ELM_SESSION="SESSION=..."
go run ./cmd/elm2api                # 默认监听 127.0.0.1:8787

# 2) 用任意模型聊天
curl http://127.0.0.1:8787/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"qwen-3.5","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

浏览器打开 <http://127.0.0.1:8787> 可以粘贴 cookie、切换模型、查看预算和已记住的 chat。

## 构建

```bash
go build -o elm2api ./cmd/elm2api
# 或生成 Windows / Linux amd64 发布包（需要 bash + bsdtar）
./scripts/build-elm2api-release.sh
```

## 目录

| 路径 | 说明 |
|---|---|
| `cmd/elm2api` | 服务本体（HTTP、chat 复用、工具调用翻译） |
| `internal/elmcore` | ELM 的 HTTP / WebSocket / STOMP 协议客户端 |
| `docs/ELM2API.md` | 完整文档 |
| `scripts/elm2api-release` | 发布包模板（config / README / start.bat） |
