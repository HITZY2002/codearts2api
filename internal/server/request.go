// OpenAI chat.completions 请求解析与会话指纹。
//
// 支持完整 OpenAI 消息模型：system/user/assistant/tool 四种角色、
// string 与分片 content、assistant 的 tool_calls、tool 结果的 tool_call_id。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// openAIToolCall 一次函数调用（assistant 历史或响应里用）。
type openAIToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串（OpenAI 线格式）
}

// openAIMessage 拍平后的一条消息。
type openAIMessage struct {
	Role string
	Text string
	// ContentParts user 多模态分片（text / image_url），nil 表示纯文本。
	// 上游 v2 接受 content 数组（官方内核 image-analyze 同款形态），原样透传。
	ContentParts     []map[string]any
	ReasoningContent string
	ToolCalls        []openAIToolCall
	ToolCallID       string // role=tool 时对应的 call id
	Name             string // role=tool 时的函数名（部分客户端会带）
}

// toolChoiceOpenAI 归一化后的 tool_choice。
type toolChoiceOpenAI struct {
	Mode     string // "" / "none" / "auto" / "required" / "function"
	Function string // Mode=="function" 时指定的函数名
}

// chatRequest 完整请求体。
type chatRequest struct {
	Model           string
	Stream          bool
	IncludeUsage    bool
	ReasoningEffort string
	MaxTokens       *int
	Temperature     *float64
	TopP            *float64
	Messages        []openAIMessage
	Tools           []map[string]any
	ToolChoice      toolChoiceOpenAI
	ConversationID  string
}

// parseChatRequest 解析并校验请求体。
func parseChatRequest(body []byte) (*chatRequest, error) {
	var raw struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		ReasoningEffort     string            `json:"reasoning_effort"`
		MaxTokens           *int              `json:"max_tokens"`
		MaxCompletionTokens *int              `json:"max_completion_tokens"`
		Temperature         *float64          `json:"temperature"`
		TopP                *float64          `json:"top_p"`
		ConversationID      string            `json:"conversation_id"`
		Messages            []json.RawMessage `json:"messages"`
		Tools               []map[string]any  `json:"tools"`
		ToolChoice          json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	if len(raw.Messages) == 0 {
		return nil, errors.New("messages is empty")
	}
	req := &chatRequest{
		Model:           raw.Model,
		Stream:          raw.Stream,
		IncludeUsage:    raw.StreamOptions.IncludeUsage,
		ReasoningEffort: strings.ToLower(strings.TrimSpace(raw.ReasoningEffort)),
		MaxTokens:       raw.MaxTokens,
		Temperature:     raw.Temperature,
		TopP:            raw.TopP,
		ConversationID:  raw.ConversationID,
		Tools:           raw.Tools,
		ToolChoice:      parseToolChoice(raw.ToolChoice),
	}
	if raw.MaxCompletionTokens != nil {
		req.MaxTokens = raw.MaxCompletionTokens
	}
	if req.MaxTokens != nil && *req.MaxTokens <= 0 {
		return nil, errors.New("max_tokens must be greater than 0")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return nil, errors.New("temperature must be between 0 and 2")
	}
	if req.TopP != nil && (*req.TopP < 0 || *req.TopP > 1) {
		return nil, errors.New("top_p must be between 0 and 1")
	}
	for i, rm := range raw.Messages {
		m, err := parseMessage(rm)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, m)
	}
	// 至少要有一条 user 或 tool 消息（否则没有可发送的内容）。
	hasSendable := false
	for _, m := range req.Messages {
		if m.Role == "user" || m.Role == "tool" {
			hasSendable = true
			break
		}
	}
	if !hasSendable {
		return nil, errors.New("no user or tool message found")
	}
	return req, nil
}

// parseMessage 解析单条消息：content 支持 string 与 [{type:text,text}] 分片。
func parseMessage(rm json.RawMessage) (openAIMessage, error) {
	var probe struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		ToolCalls        json.RawMessage `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		Name             string          `json:"name"`
	}
	if err := json.Unmarshal(rm, &probe); err != nil {
		return openAIMessage{}, err
	}
	parts, text, cerr := parseContentParts(probe.Content)
	if cerr != nil {
		return openAIMessage{}, cerr
	}
	m := openAIMessage{
		Role:             strings.ToLower(strings.TrimSpace(probe.Role)),
		Text:             text,
		ReasoningContent: probe.ReasoningContent,
		ToolCallID:       probe.ToolCallID,
		Name:             probe.Name,
	}
	// 仅 user 消息透传多模态分片；其余角色维持纯文本（assistant/tool 的
	// content 数组对上游无意义）。
	if len(parts) > 0 && m.Role == "user" {
		m.ContentParts = parts
	}
	if len(probe.ToolCalls) > 0 && string(probe.ToolCalls) != "null" {
		var calls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(probe.ToolCalls, &calls); err == nil {
			for _, c := range calls {
				m.ToolCalls = append(m.ToolCalls, openAIToolCall{
					ID:        c.ID,
					Name:      c.Function.Name,
					Arguments: rawJSONString(c.Function.Arguments),
				})
			}
		}
	}
	return m, nil
}

// parseContentParts 解析 content：string → (nil, s, nil)；分片数组 →
// (parts, 拼接文本, nil)。支持的形态与官方内核 image-analyze 对齐
// （text / image_url.data|https，agent-kernel.exe vlm_caller.rs 逆向确认）。
// 其余分片类型显式报错——旧版静默丢弃 image_url，VL 模型"看不懂"且无提示。
func parseContentParts(raw json.RawMessage) ([]map[string]any, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return nil, s, nil
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, "", nil // 非 string 非数组：维持旧行为置空
	}
	var sb strings.Builder
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch t, _ := p["type"].(string); t {
		case "text":
			txt, _ := p["text"].(string)
			sb.WriteString(txt)
			out = append(out, map[string]any{"type": "text", "text": txt})
		case "image_url":
			iu, ok := p["image_url"].(map[string]any)
			if !ok {
				return nil, "", errors.New(`image_url part missing "image_url" object`)
			}
			if u, _ := iu["url"].(string); u == "" {
				return nil, "", errors.New(`image_url part has empty "url"`)
			}
			sb.WriteString("[image]")
			// image_url 对象整体保留（含 detail 等 OpenAI 字段），原样透传。
			out = append(out, map[string]any{"type": "image_url", "image_url": iu})
		default:
			return nil, "", fmt.Errorf("unsupported content part type %q", t)
		}
	}
	return out, sb.String(), nil
}

// flattenContent 拍平 string / 分片 content 为纯文本。
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if t, ok := p["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

// rawJSONString 把 arguments 统一成 JSON 字符串（兼容字符串与对象两种线格式）。
func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	out, err := json.Marshal(v) // Go map marshal 按 key 排序，天然归一化
	if err != nil {
		return "{}"
	}
	return string(out)
}

// parseToolChoice 归一化 tool_choice：字符串或 {"type":"function","function":{"name":...}}。
func parseToolChoice(raw json.RawMessage) toolChoiceOpenAI {
	if len(raw) == 0 || string(raw) == "null" {
		return toolChoiceOpenAI{Mode: "auto"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "none":
			return toolChoiceOpenAI{Mode: "none"}
		case "required":
			return toolChoiceOpenAI{Mode: "required"}
		default:
			return toolChoiceOpenAI{Mode: "auto"}
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		return toolChoiceOpenAI{Mode: "function", Function: obj.Function.Name}
	}
	return toolChoiceOpenAI{Mode: "auto"}
}

// ---------------------------------------------------------------------------
// 会话指纹（无状态客户端的历史对齐）
// ---------------------------------------------------------------------------

// canonical 消息归一化串：指纹链的输入。client 回发的 assistant 消息
// 由我们生成（tool_calls id 也是我们发的），因此可逐字节复算。
func canonical(m openAIMessage) string {
	var sb strings.Builder
	sb.WriteString(m.Role)
	sb.WriteString("\x00")
	sb.WriteString(m.Text)
	if len(m.ContentParts) > 0 {
		// 不同图片 = 不同轮次：把分片纳入指纹，避免同文本不同图共享 msg_id。
		if raw, err := json.Marshal(m.ContentParts); err == nil {
			sb.WriteString("\x00")
			sb.WriteString(string(raw))
		}
	}
	for _, c := range m.ToolCalls {
		sb.WriteString("\x00")
		sb.WriteString(c.ID)
		sb.WriteString("\x00")
		sb.WriteString(c.Name)
		sb.WriteString("\x00")
		sb.WriteString(normalizeArgs(c.Arguments))
	}
	if m.ToolCallID != "" {
		sb.WriteString("\x00tool:")
		sb.WriteString(m.ToolCallID)
	}
	return sb.String()
}

// normalizeArgs 归一化 arguments（key 排序、去空白）。
func normalizeArgs(args string) string {
	if args == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	out, err := json.Marshal(v)
	if err != nil {
		return args
	}
	return string(out)
}

// chainFingerprint 链式指纹：fp_i = sha256(fp_{i-1} + canonical(m_i))。
func chainFingerprint(prev string, m openAIMessage) string {
	sum := sha256.Sum256([]byte(prev + "\x01" + canonical(m)))
	return hex.EncodeToString(sum[:16])
}

// fingerprintOf 计算整段消息列表的链式指纹。
func fingerprintOf(msgs []openAIMessage) string {
	fp := ""
	for _, m := range msgs {
		fp = chainFingerprint(fp, m)
	}
	return fp
}

// upstreamTurnKey identifies one logical user turn, not one HTTP request.
// Tool continuations append assistant/tool messages after the latest user
// message; hashing only through that user message keeps the turn stable.
func upstreamTurnKey(req *chatRequest) string {
	if req == nil || len(req.Messages) == 0 {
		return ""
	}
	lastUser := -1
	for i, m := range req.Messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	prefix := req.Messages
	if lastUser >= 0 {
		prefix = req.Messages[:lastUser+1]
	}
	fp := fingerprintOf(prefix)
	if req.ConversationID != "" {
		return req.ConversationID + ":" + fp
	}
	return fp
}

// conversationIDFor returns an explicit client conversation identity. Requests
// without one get a fresh upstream chat ID in the handler, preventing two
// unrelated conversations with the same first message from sharing state.
func conversationIDFor(req *chatRequest, headerValue string) string {
	for _, explicit := range []string{req.ConversationID, headerValue} {
		explicit = strings.TrimSpace(explicit)
		if explicit == "" {
			continue
		}
		if validChatID(explicit) {
			return strings.ToLower(explicit)
		}
		sum := sha256.Sum256([]byte(explicit))
		return hex.EncodeToString(sum[:16])
	}
	return ""
}
