package main

import (
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
	b.WriteString("# 工具调用协议（必须严格遵守）\n\n")
	b.WriteString("需要调用工具时，整个回答只输出一个 JSON 代码块，前面后面都不要写任何解释：\n\n")
	b.WriteString("```json\n")
	b.WriteString("{\"tool_calls\":[{\"name\":\"工具名\",\"arguments\":{\"参数\":\"值\"}}]}")
	b.WriteString("\n```\n\n")
	b.WriteString("规则：\n")
	b.WriteString("1. 代码块语言标记必须是 json，内容是单个对象，键名是 tool_calls。\n")
	b.WriteString("2. arguments 必须是 JSON 对象，键名和类型严格符合该工具的 JSON Schema，不要发明字段。\n")
	b.WriteString("3. 需要多个调用时，把多个 {name, arguments} 放进同一个 tool_calls 数组。\n")
	b.WriteString("4. 字符串里的换行写成 \\n，不要写成真实换行或 YAML/XML。\n")
	b.WriteString("5. 工具执行结果会以 \"Tool result (<工具名>):\" 出现在后续对话里；拿到结果后再继续。\n")
	b.WriteString("6. 不需要调用工具时，直接用自然语言回答，不要输出任何 JSON 代码块。\n")
	b.WriteString("7. 不要假装工具已经执行，也不要一边说\"我来执行\"一边给代码块；要调用就直接给代码块。\n\n")
	b.WriteString("反例（都不要这样做）：\n")
	b.WriteString("- 在代码块前后写\"好的，我来运行 git status\"。\n")
	b.WriteString("- 用 ```yaml 或 ```xml 写调用，或写成裸 XML。\n")
	b.WriteString("- 编造参数（例如把 command 写成空字符串）。\n")
	if forced != "" {
		b.WriteString(fmt.Sprintf("- 本次请求必须调用工具 %q。\n", forced))
	} else if mode == "required" {
		b.WriteString("- 本次请求必须调用至少一个工具。\n")
	}
	b.WriteString("\n## 可用工具\n\n")
	for _, tool := range tools {
		b.WriteString(fmt.Sprintf("### %s\n%s\n参数 schema：%s\n\n", tool.Name, strings.TrimSpace(tool.Description), compactJSON(tool.Parameters)))
	}
	return b.String()
}

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
		return nil, reply
	}
	return calls, strings.TrimSpace(rest)
}

func parseToolBlock(body string, known map[string]bool) ([]invokedTool, bool) {
	text := strings.TrimSpace(body)
	if text == "" {
		return nil, false
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		if calls, ok := decodeToolJSON([]byte(text)); ok {
			return calls, true
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

func decodeToolJSON(raw []byte) ([]invokedTool, bool) {
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
