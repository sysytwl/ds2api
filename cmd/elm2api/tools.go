package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// toolSpec is one callable function, normalised from either the Chat
// Completions shape ({"type":"function","function":{...}}) or the Responses
// shape ({"type":"function","name":...}).
type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type rawTool struct {
	Type     string          `json:"type"`
	Function *toolSpec       `json:"function"`
	Name     string          `json:"name"`
	Desc     string          `json:"description"`
	Params   json.RawMessage `json:"parameters"`
}

// parseTools accepts either tool shape and drops anything that is not a function.
func parseTools(raw json.RawMessage) []toolSpec {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []rawTool
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]toolSpec, 0, len(items))
	for _, item := range items {
		kind := strings.TrimSpace(item.Type)
		if kind != "" && kind != "function" {
			continue
		}
		spec := toolSpec{Name: item.Name, Description: item.Desc, Parameters: item.Params}
		if item.Function != nil {
			spec = *item.Function
		}
		if strings.TrimSpace(spec.Name) == "" {
			continue
		}
		if len(spec.Parameters) == 0 || string(spec.Parameters) == "null" {
			spec.Parameters = json.RawMessage("{\"type\":\"object\",\"properties\":{}}")
		}
		out = append(out, spec)
	}
	return out
}

// toolChoice reports the mode ("auto", "none", "required") and, when the caller
// forces one function, its name.
func toolChoice(raw json.RawMessage) (string, string) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return "auto", ""
	}
	if raw[0] == '"' {
		var mode string
		if err := json.Unmarshal(raw, &mode); err == nil {
			switch strings.ToLower(strings.TrimSpace(mode)) {
			case "none":
				return "none", ""
			case "required", "any":
				return "required", ""
			default:
				return "auto", ""
			}
		}
		return "auto", ""
	}
	var obj struct {
		Type     string    `json:"type"`
		Name     string    `json:"name"`
		Function *toolSpec `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "auto", ""
	}
	if obj.Function != nil && strings.TrimSpace(obj.Function.Name) != "" {
		return "required", obj.Function.Name
	}
	if strings.TrimSpace(obj.Name) != "" {
		return "required", obj.Name
	}
	return "auto", ""
}

// toolInstructions is the prompt section that teaches the web-chat model how to
// ask for a tool. The ELM chat has no native function calling, so the call has
// to travel as text and be translated back into tool_calls by this service.
func toolInstructions(tools []toolSpec, mode, forced string) string {
	var b strings.Builder
	b.WriteString("# 工具调用\n\n")
	b.WriteString("需要调用工具时，在回复中输出一个 ```json 代码块，内容形如：\n")
	b.WriteString("{\"tool_calls\":[{\"name\":\"工具名\",\"arguments\":{\"参数\":\"值\"}}]}\n\n")
	b.WriteString("可以先用一句话说明意图。arguments 必须是符合该工具 schema 的 JSON 对象；不要编造工具执行结果。\n")
	b.WriteString("工具执行结果会以 \"Tool result (工具名): ...\" 出现在后续对话里，拿到结果后再继续回答。\n")
	b.WriteString("不需要调用工具时，直接用自然语言回答。\n")
	if forced != "" {
		b.WriteString(fmt.Sprintf("\n本次请求必须调用工具 %q。\n", forced))
	} else if mode == "required" {
		b.WriteString("\n本次请求必须调用至少一个工具。\n")
	}
	b.WriteString("\n## 可用工具\n\n")
	for _, tool := range tools {
		b.WriteString("### " + tool.Name + "\n")
		if desc := strings.TrimSpace(tool.Description); desc != "" {
			b.WriteString(desc + "\n")
		}
		b.WriteString("参数 schema：" + compactJSON(tool.Parameters) + "\n\n")
	}
	return b.String()
}

// toolResultReminder follows every tool result. It lives in the canonical
// prompt (so prefix continuation keeps working) and puts the protocol at the
// end of the message the model answers, where instructions are followed best.
const toolResultReminder = "（如果还需要工具，只输出 json 代码块；否则直接回答。）"

func compactJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// repairJSONText makes one last attempt at a call block that is almost JSON:
// "` json" fences leave the language tag in the body, models sometimes close
// the block with a full stop, and Chinese punctuation leaks in.
func repairJSONText(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSpace(t)
	if len(t) > 4 && strings.HasPrefix(strings.ToLower(t), "json") {
		t = strings.TrimSpace(t[4:])
	}
	if i := strings.LastIndexAny(t, "}]"); i >= 0 && i+1 < len(t) {
		if rest := strings.TrimSpace(t[i+1:]); rest != "" && strings.ContainsAny(rest, "。．,，;；\u0060") {
			t = strings.TrimSpace(t[:i+1])
		}
	}
	replacer := strings.NewReplacer("“", "\"", "”", "\"", "：", ":", "，", ",")
	return replacer.Replace(t)
}

// looksLikeToolAttempt reports whether a reply tried to call a tool even though
// the parser could not turn it into one (used to trigger a single repair turn
// and to log the raw reply).
func looksLikeToolAttempt(reply string) bool {
	return strings.Contains(reply, "tool_calls") ||
		strings.Contains(reply, "<tool_call>") ||
		(strings.Contains(reply, "\"name\"") && strings.Contains(reply, "\"arguments\""))
}

// toolRetryPrompt is sent into the same chat when the model wrote a call as
// plain text. Keeping it short leaves the conversation context in place.
func toolRetryPrompt(tools []toolSpec) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return "你上一条回复把工具调用写成了普通文本，没有按协议输出。现在请只输出一个 ```json 代码块，内容形如：\n" +
		"{\"tool_calls\":[{\"name\":\"工具名\",\"arguments\":{...}}]}\n" +
		"可用工具：" + strings.Join(names, "、") + "。不要输出解释或其它代码块。"
}

// invokedTool is one tool call parsed out of the model's reply.
type invokedTool struct {
	Name      string
	Arguments string
}

var (
	fencedBlock = regexp.MustCompile("(?s)\x60\x60\x60[a-zA-Z0-9_-]*[ \\t]*\\n?(.*?)\x60\x60\x60")
	xmlCall     = regexp.MustCompile("(?s)<tool_call>(.*?)</tool_call>")
)

// parseToolCalls extracts tool calls from a model reply. Accepted forms are a
// JSON block ({"tool_calls":[...]} or a single {"name":...,"arguments":{...}}),
// an XML <tool_call> block, and the YAML-ish "name / key: value" block some
// models fall back to. Text outside the call is returned as content.
func parseToolCalls(reply string, tools []toolSpec) ([]invokedTool, string) {
	if strings.TrimSpace(reply) == "" {
		return nil, reply
	}
	known := make(map[string]bool, len(tools))
	for _, tool := range tools {
		known[tool.Name] = true
	}
	var calls []invokedTool
	rest := reply
	for _, match := range fencedBlock.FindAllStringSubmatch(reply, -1) {
		parsed, ok := parseToolBlock(match[1], known)
		if !ok {
			continue
		}
		calls = append(calls, parsed...)
		rest = strings.Replace(rest, match[0], "", 1)
	}
	if len(calls) == 0 {
		for _, match := range xmlCall.FindAllStringSubmatch(reply, -1) {
			parsed, ok := parseToolBlock(match[1], known)
			if !ok {
				continue
			}
			calls = append(calls, parsed...)
			rest = strings.Replace(rest, match[0], "", 1)
		}
	}
	if len(calls) == 0 {
		if parsed, ok := parseToolBlock(reply, known); ok {
			return parsed, ""
		}
		// Some models put the object right after a sentence and forget the
		// fence ("我将读取文件：{\"tool_calls\": ...}"). Only a JSON value that
		// really looks like a call is accepted here; otherwise the text stays
		// a normal answer.
		if parsed, rest, ok := extractEmbeddedCall(reply, known); ok {
			return parsed, rest
		}
		return nil, reply
	}
	return calls, strings.TrimSpace(rest)
}

// extractEmbeddedCall finds the first JSON object/array in text that decodes
// into at least one tool call and returns it with the surrounding text.
func extractEmbeddedCall(text string, known map[string]bool) ([]invokedTool, string, bool) {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' && text[i] != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			continue
		}
		if !looksLikeCallJSON(raw) {
			continue
		}
		calls, ok := decodeToolJSON(raw)
		if !ok {
			continue
		}
		if calls = filterKnownCalls(calls, known); len(calls) == 0 {
			continue
		}
		rest := strings.TrimSpace(text[:i] + text[i+int(dec.InputOffset()):])
		return calls, rest, true
	}
	return nil, "", false
}

// looksLikeCallJSON guards against turning an ordinary JSON snippet in an
// answer ({"name":"..."}) into a fake tool call.
func looksLikeCallJSON(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	if raw[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return false
		}
		for _, item := range items {
			if looksLikeCallJSON(item) {
				return true
			}
		}
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	if _, ok := probe["tool_calls"]; ok {
		return true
	}
	_, hasName := probe["name"]
	_, hasArgs := probe["arguments"]
	_, hasParams := probe["parameters"]
	if hasName && (hasArgs || hasParams) {
		return true
	}
	if fn, ok := probe["function"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(fn, &inner); err == nil {
			if _, ok := inner["name"]; ok {
				return true
			}
		}
	}
	return false
}

func parseToolBlock(body string, known map[string]bool) ([]invokedTool, bool) {
	text := strings.TrimSpace(body)
	if text == "" {
		return nil, false
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		if calls, ok := decodeToolJSON([]byte(text)); ok {
			if calls = filterKnownCalls(calls, known); len(calls) > 0 {
				return calls, true
			}
		}
		if repaired := repairJSONText(text); repaired != text {
			if calls, ok := decodeToolJSON([]byte(repaired)); ok {
				if calls = filterKnownCalls(calls, known); len(calls) > 0 {
					return calls, true
				}
			}
		}
	}
	lines := strings.Split(text, "\n")
	name := strings.TrimSuffix(strings.TrimSpace(lines[0]), ":")
	if name == "" || (len(known) > 0 && !known[name]) {
		return nil, false
	}
	args := map[string]any{}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if key == "" {
			continue
		}
		args[key] = parseScalar(value)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, false
	}
	return []invokedTool{{Name: name, Arguments: string(raw)}}, true
}

func parseScalar(value string) any {
	switch {
	case value == "true":
		return true
	case value == "false":
		return false
	case value == "null":
		return nil
	case strings.HasPrefix(value, "\""), strings.HasPrefix(value, "{"), strings.HasPrefix(value, "["),
		(value != "" && (value[0] == '-' || (value[0] >= '0' && value[0] <= '9'))):
		var v any
		if err := json.Unmarshal([]byte(value), &v); err == nil {
			return v
		}
	}
	return strings.Trim(value, "\"")
}

// decodeToolJSON accepts one call envelope or a bare array of calls.
func decodeToolJSON(raw []byte) ([]invokedTool, bool) {
	if calls, ok := decodeToolEnvelope(raw); ok {
		return calls, true
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, false
	}
	var calls []invokedTool
	for _, item := range items {
		if parsed, ok := decodeToolEnvelope(item); ok {
			calls = append(calls, parsed...)
		}
	}
	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
}

func decodeToolEnvelope(raw []byte) ([]invokedTool, bool) {
	var envelope struct {
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Function  *struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, false
	}
	var calls []invokedTool
	for _, item := range envelope.ToolCalls {
		name := item.Name
		args := item.Arguments
		if item.Function != nil {
			if strings.TrimSpace(name) == "" {
				name = item.Function.Name
			}
			if len(args) == 0 || string(args) == "null" {
				args = item.Function.Arguments
			}
		}
		call, ok := buildInvoked(name, args)
		if ok {
			calls = append(calls, call)
		}
	}
	if len(calls) > 0 {
		return calls, true
	}
	if call, ok := buildInvoked(envelope.Name, firstNonEmpty(envelope.Arguments, envelope.Parameters)); ok {
		return []invokedTool{call}, true
	}
	return nil, false
}

// filterKnownCalls drops calls to names the client did not declare, so an
// ordinary JSON example in an answer is not mistaken for a tool call.
func filterKnownCalls(calls []invokedTool, known map[string]bool) []invokedTool {
	if len(known) == 0 {
		return calls
	}
	out := calls[:0]
	for _, call := range calls {
		if known[call.Name] {
			out = append(out, call)
		}
	}
	return out
}

func buildInvoked(name string, args json.RawMessage) (invokedTool, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return invokedTool{}, false
	}
	text := strings.TrimSpace(string(args))
	if text == "" || text == "null" {
		text = "{}"
	}
	if strings.HasPrefix(text, "\"") {
		var inner string
		if err := json.Unmarshal(args, &inner); err == nil {
			text = strings.TrimSpace(inner)
		}
	}
	if text == "" {
		text = "{}"
	}
	if !json.Valid([]byte(text)) {
		return invokedTool{}, false
	}
	return invokedTool{Name: name, Arguments: text}, true
}

func firstNonEmpty(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}

// toolCallPayload renders OpenAI Chat Completions tool_calls.
func toolCallPayload(ids []string, calls []invokedTool) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for i, call := range calls {
		id := newPrefixedID("call")
		if i < len(ids) && ids[i] != "" {
			id = ids[i]
		}
		out = append(out, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return out
}
