# AGENTS.md

本仓库是独立的 `elm2api` 服务：把 ELM 网页版聊天包装成 OpenAI 兼容 API。
模块路径为 `elm2api`，只保留 `cmd/elm2api` 与 `internal/elmcore`。

## 提交前检查

按顺序运行，全部通过再提交：

```bash
gofmt -l cmd internal
go vet ./...
go test ./...
```

改动用户可见行为时，同步更新 `docs/ELM2API.md` 与 `scripts/elm2api-release/README.md`。

## 约束

- 不要提交 cookie、`elm2api-state.json`、`dist/` 或 `.tmp/`。
- 发送到 ELM 的单条消息上限约 16K 字符，超长 prompt 必须继续走 `splitPrompt`。
- 一个 ELM 账号同一时刻只能跑一个聊天：请求必须经过 session 的"回合锁"排队，不能并发写同一个 chat。
- 对话连续性依赖"prompt 前缀匹配 + sha256"：不要改回"每个请求新建 chat"，也不要在 state 文件里存完整 prompt。
- 工具调用是文本协议（模型输出 JSON，服务翻译成 `tool_calls`）：协议改动要同时更新 `tools.go`、两条 API 路径和测试。
