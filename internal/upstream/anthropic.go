// anthropic.go Anthropic Messages 协议（Claude Code / CC Switch 走 /v1/messages）与
// 上游 OpenAI 兼容协议之间的双向转换。
//
// 方向：
//   - 请求：Anthropic {system, messages[content blocks], tools[name/input_schema]}
//     → OpenAI {messages, tools[type=function]}
//   - 响应：上游 OpenAI SSE / 聚合结果 → Anthropic SSE 事件 / message 对象
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
)

// ────────────────────────── 请求：Anthropic → OpenAI ──────────────────────────

// AnthropicToChat 把 Anthropic /v1/messages 的请求体转成发给上游的 chat 请求体。
// model 为最终要发给上游的模型名（调用方已完成模型名映射）。
func AnthropicToChat(req map[string]any, model string) (map[string]any, error) {
	out := map[string]any{
		"model":  model,
		"stream": true, // 上游只支持流式
	}

	// 采样参数与终止序列
	for _, k := range []string{"temperature", "top_p", "top_k", "max_tokens"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if v, ok := req["stop_sequences"]; ok {
		if arr, ok2 := v.([]any); ok2 && len(arr) > 0 {
			out["stop"] = arr
		}
	}

	msgs := make([]any, 0, 8)

	// system：Anthropic 顶层独立字段，OpenAI 里是首条 system 消息
	if sys := anthropicText(req["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	// messages：content 可能是字符串，也可能是 block 数组
	rawMsgs, _ := req["messages"].([]any)
	for _, rm := range rawMsgs {
		m, _ := rm.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		msgs = append(msgs, anthropicMessageToChat(role, m["content"])...)
	}
	out["messages"] = msgs

	// tools：input_schema → parameters
	if raw, ok := req["tools"]; ok {
		if arr, ok2 := raw.([]any); ok2 {
			if tools := anthropicToolsToChat(arr); len(tools) > 0 {
				out["tools"] = tools
			}
		}
	}

	return out, nil
}

// AnthropicTextOf 导出版本的 anthropicText，供日志统计 system 长度等使用。
func AnthropicTextOf(v any) string { return anthropicText(v) }

// anthropicText 归一化 Anthropic 的文本字段：
// 既可能是纯字符串，也可能是 [{type:"text",text:"..."}]。
func anthropicText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, it := range t {
			bm, ok := it.(map[string]any)
			if !ok {
				if s, ok2 := it.(string); ok2 {
					parts = append(parts, s)
				}
				continue
			}
			// 只取 text 类型，忽略 cache_control 等包装
			if s, ok := bm["text"].(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// anthropicMessageToChat 把一条 Anthropic 消息转成 1..n 条 OpenAI 消息
// （一条含 tool_result 的 user 消息会拆成多条 role=tool 消息）。
func anthropicMessageToChat(role string, content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": s}}
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}

	var (
		texts       []string
		toolCalls   []any
		toolResults []any
	)
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm == nil {
			if s, ok2 := b.(string); ok2 {
				texts = append(texts, s)
			}
			continue
		}
		switch bm["type"] {
		case "text":
			if s, _ := bm["text"].(string); s != "" {
				texts = append(texts, s)
			}
		case "tool_use":
			id, _ := bm["id"].(string)
			name, _ := bm["name"].(string)
			args := "{}"
			if bm["input"] != nil {
				if raw, err := json.Marshal(bm["input"]); err == nil {
					args = string(raw)
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			})
		case "tool_result":
			id, _ := bm["tool_use_id"].(string)
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      anthropicText(bm["content"]),
			})
		}
		// thinking / redacted_thinking / image 等：上游不消费，忽略
	}

	var out []any
	joined := strings.Join(texts, "")
	switch {
	case len(toolCalls) > 0:
		out = append(out, map[string]any{
			"role": role, "content": joined, "tool_calls": toolCalls,
		})
	case joined != "" || len(toolResults) == 0:
		out = append(out, map[string]any{"role": role, "content": joined})
	}
	out = append(out, toolResults...)
	return out
}

// anthropicToolsToChat 把 Anthropic tool（name/description/input_schema）
// 转成 OpenAI function tool（type=function + function.parameters）。
func anthropicToolsToChat(arr []any) []any {
	out := make([]any, 0, len(arr))
	for _, it := range arr {
		t, _ := it.(map[string]any)
		if t == nil {
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		params, _ := t["input_schema"].(map[string]any)
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": t["description"],
				"parameters":  params,
			},
		})
	}
	return out
}

// ────────────────────────── 响应：OpenAI → Anthropic ──────────────────────────

// newMessageID 生成 Anthropic 风格的 msg_ id。
func newMessageID() string {
	return fmt.Sprintf("msg_%d", time.Now().UnixNano())
}

// writeAnthropicEvent 写一个 Anthropic SSE 事件（event: 行 + data: 行）。
func writeAnthropicEvent(w io.Writer, event string, data map[string]any, flush func()) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}

// syntheticSignature 生成一个非空、稳定的「思考块签名」。
//
// Anthropic 协议里每个 thinking 块都必须带 signature（官方是 base64 编码的
// 加密签名，用于把思考回传给 Claude 时做完整性校验）。本服务上游是 CodeBuddy
// 的 OpenAI 兼容端点，根本没有真签名可用。若直接发空串 ""，Claude Code 等
// 严格客户端会判定 thinking 块非法而丢弃/不渲染，于是客户端「看不到思考过程」。
//
// 这里填一个非空占位签名：仅用于让客户端接受并渲染思考文本；客户端下一轮把
// thinking 块回传时，本服务的 anthropicMessageToChat 会直接忽略 thinking 块
// （上游不消费），所以占位签名永远不会被送去上游，不会引发任何校验错误。
const syntheticSignature = "Egf0aGlua2luZy12MS1jYjJhcGkSIGZvcm1hdC1wbGFjZWhvbGRlcg=="

// usageTokens 从 OpenAI 风格 usage 里取 (输入, 输出) token 数。
func usageTokens(u map[string]any) (int, int) {
	return numVal(u["prompt_tokens"]), numVal(u["completion_tokens"])
}

// StreamAsAnthropic 把上游 OpenAI SSE 转写为 Anthropic Messages SSE。
// 事件序列：message_start → [thinking 块] → [text 块] → [tool_use 块] → message_delta → message_stop
//
// 与 StreamAsOpenAI 一致：所有 reasoning_content 合并成「单个」thinking 块下发，无论上游是
// 先推理后正文还是推理与正文交错（R→C→R），客户端都只看到一个思考块；正文按原始 delta
// 顺序逐条回放，保持流式渲染。代价是推理模型响应需等上游跑完才下发（牺牲部分流式实时性）。
func StreamAsAnthropic(w io.Writer, r io.Reader, model string, tools any, wantThinking bool, flush func(), onUsage ...func(map[string]any)) error {
	schemas := toolSchemas(tools)
	msgID := newMessageID()
	started := time.Now()
	_ = writeAnthropicEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}, flush)

	var (
		finalUsage map[string]any
		reasoning  strings.Builder
		textDeltas []string
		toolCalls  []map[string]any
		toolOrder  []int
		stopReason = "end_turn"
	)

	// 读完整段流：合并思考、收集正文 delta、合并工具调用。
	// 注意：本实现刻意「先读完再下发」，因此客户端在 message_start 之后要一直等到
	// 上游整段流结束才能看到第一个 content_block_delta。上游越慢（长思考、
	// 大 system + 多工具），客户端空等越久，可能触发其流式空闲超时后重试。
	// 这里记录首个 chunk 与整段流的耗时，便于定位这类「客户端只见重试、日志安静」的问题。
	var firstChunk time.Time
	err := ParseSSE(r, func(chunk map[string]any) error {
		if firstChunk.IsZero() {
			firstChunk = time.Now()
			log.Printf("messages: upstream first chunk after %s", firstChunk.Sub(started).Round(time.Millisecond))
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			finalUsage = mergeUsage(finalUsage, u)
		}
		ch, _ := chunk["choices"].([]any)
		if len(ch) == 0 {
			return nil
		}
		c, _ := ch[0].(map[string]any)
		if c == nil {
			return nil
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			switch fr {
			case "tool_calls":
				stopReason = "tool_use"
			case "length":
				stopReason = "max_tokens"
			}
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			reasoning.WriteString(rc)
		}
		if txt, ok := delta["content"].(string); ok && txt != "" {
			textDeltas = append(textDeltas, txt)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			toolCalls, toolOrder = mergeToolCallStream(toolCalls, toolOrder, tcs)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 单 thinking 块：合并全部推理，避免交错推理产生多个思考块。
	// 仅当客户端显式开启 thinking 时才下发——未开启却返回 thinking 块（且
	// signature 为占位符）会被 Claude Desktop 等严格客户端整条拒收。
	if wantThinking && reasoning.Len() > 0 {
		_ = writeAnthropicEvent(w, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": syntheticSignature},
		}, flush)
		_ = writeAnthropicEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning.String()},
		}, flush)
		_ = writeAnthropicEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		}, flush)
	}

	// text 块：逐 delta 回放，保持客户端流式渲染与「你好」「世界」分离。
	if len(textDeltas) > 0 {
		idx := 0
		if wantThinking && reasoning.Len() > 0 {
			idx = 1
		}
		_ = writeAnthropicEvent(w, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         idx,
			"content_block": map[string]any{"type": "text", "text": ""},
		}, flush)
		for _, td := range textDeltas {
			_ = writeAnthropicEvent(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{"type": "text_delta", "text": td},
			}, flush)
		}
		_ = writeAnthropicEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		}, flush)
	}

	// tool_use 块
	idx := 0
	if wantThinking && reasoning.Len() > 0 {
		idx++
	}
	if len(textDeltas) > 0 {
		idx++
	}
	validCalls := filterToolCalls(toolCalls, toolOrder, schemas)
	if len(validCalls) > 0 {
		stopReason = "tool_use"
		var names []string
		for _, vc := range validCalls {
			argsJSON, _ := json.Marshal(vc.Input)
			names = append(names, vc.Name+" id="+vc.ID+" args="+string(argsJSON))
			_ = writeAnthropicEvent(w, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    vc.ID,
					"name":  vc.Name,
					"input": map[string]any{},
				},
			}, flush)
			_ = writeAnthropicEvent(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(argsJSON)},
			}, flush)
			_ = writeAnthropicEvent(w, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": idx,
			}, flush)
			idx++
		}
		log.Printf("messages stream tool_use: %s", strings.Join(names, "; "))
	} else if stopReason == "tool_use" {
		// 上游标记了 tool_calls 却没有可解析的调用：降级为 end_turn，
		// 否则客户端会报 "tool call could not be parsed" 且看不到任何正文。
		stopReason = "end_turn"
		log.Printf("messages: upstream signaled tool_calls but no parsable call, downgrade stop_reason to end_turn")
	}

	// message_delta：携带 stop_reason 与输出 token
	outTokens := 0
	if finalUsage != nil {
		_, outTokens = usageTokens(finalUsage)
	}
	_ = writeAnthropicEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	}, flush)
	_ = writeAnthropicEvent(w, "message_stop", map[string]any{"type": "message_stop"}, flush)

	// 请求级日志：流式结果摘要
	textLen := 0
	for _, d := range textDeltas {
		textLen += len(d)
	}
	upstreamWait := time.Duration(0)
	if !firstChunk.IsZero() {
		upstreamWait = firstChunk.Sub(started)
	}
	log.Printf("messages stream done: model=%s stop=%s text_len=%d tool_calls=%d reasoning_len=%d out_tokens=%d upstream_first=%s total=%s",
		model, stopReason, textLen, len(validCalls), reasoning.Len(), outTokens,
		upstreamWait.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))

	if len(onUsage) > 0 && finalUsage != nil {
		onUsage[0](finalUsage)
	}
	return nil
}

// toolCallParts 归一化后的工具调用，供 Anthropic tool_use 块使用。
type toolCallParts struct {
	ID    string
	Name  string
	Input map[string]any
}

// repairJSONArgs 解析工具调用的 arguments。上游偶发截断（缺收尾引号/括号），
// 这里做有限修复；仍无法解析时返回 ok=false，调用方应丢弃该次调用。
func repairJSONArgs(s string) (map[string]any, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}, true
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err == nil && out != nil {
		return out, true
	}
	trimmed := strings.TrimRight(s, " \t\r\n")
	for _, suffix := range []string{"\"}", "\"", "}", "]}"} {
		var o map[string]any
		if err := json.Unmarshal([]byte(trimmed+suffix), &o); err == nil && o != nil {
			return o, true
		}
	}
	return nil, false
}

// sanitizeToolName 清理工具名里的零宽字符（脱敏可能污染），保证客户端能匹配到工具。
func sanitizeToolName(name string) string {
	if !strings.Contains(name, zwsp) {
		return name
	}
	return strings.ReplaceAll(name, zwsp, "")
}

// toolSchemas 从 OpenAI 格式 tools 数组构建「工具名 → 参数 properties」索引。
func toolSchemas(tools any) map[string]map[string]any {
	out := map[string]map[string]any{}
	arr, ok := tools.([]any)
	if !ok {
		// 诊断：tools 不是 []any（形态不符），会导致全部工具都拿不到 schema
		// → conformToolInput 一律放行 → 幻觉字段透传给客户端触发严格校验失败。
		if tools != nil {
			log.Printf("toolSchemas: tools is %T (not []any), no schema resolved at all", tools)
		}
		return out
	}
	var total, noFn, noName, noProps, emptyProps int
	var emptyNames []string
	for _, it := range arr {
		total++
		tool, ok := it.(map[string]any)
		if !ok {
			noFn++
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			noFn++
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			noName++
			continue
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if props == nil {
			noProps++
			emptyNames = append(emptyNames, name)
			continue
		}
		if len(props) == 0 {
			emptyProps++
			emptyNames = append(emptyNames, name)
			continue
		}
		out[name] = props
	}
	// 诊断：noProps/noFn/noName 才是**异常**信号（工具定义残缺，会导致
	// conformToolInput 拿不到 schema 而放行幻觉字段）。emptyProps 是**常态**——
	// Claude Code 本就有若干无参工具（CronList / EnterPlanMode / TaskList 等
	// 的空 properties 是合法的），不再据此打日志，避免每次请求刷屏。
	if noProps > 0 || noFn > 0 || noName > 0 {
		log.Printf("toolSchemas: total=%d resolved=%d noProps=%d emptyProps=%d noFn=%d noName=%d unresolved=%v",
			total, len(out), noProps, emptyProps, noFn, noName, emptyNames)
	}
	return out
}

// conformToolInput 按工具 schema 过滤 input 中未定义的字段。
//
// 模型偶发幻觉出 schema 之外的字段（如实测 glm-5.3-flash 会在 Bash 调用里
// 额外生成一个 description 参数）。Claude Code / Claude Desktop 的工具
// schema 是严格校验（additionalProperties: false），多一个未知字段就会报
// "The model's tool call could not be parsed"。schema 未知的工具不做过滤。
func conformToolInput(input map[string]any, props map[string]any) map[string]any {
	// 诊断（只打日志，不改变行为）：schema 为空却收到参数是「幻觉字段被放行」
	// 的高危信号——此时下方会原样返回，客户端严格校验可能报
	// "The model's tool call could not be parsed"。留证据以便定位是
	// 「工具本就无参」还是「input_schema 传输丢失」。
	if len(props) == 0 && len(input) > 0 {
		names := make([]string, 0, len(input))
		for k := range input {
			names = append(names, k)
		}
		log.Printf("conformToolInput: empty schema but %d field(s) present, passing through (no filtering): %v",
			len(input), names)
	}
	if props == nil || len(props) == 0 || len(input) == 0 {
		return input
	}
	clean := make(map[string]any, len(input))
	for k, v := range input {
		if _, defined := props[k]; defined {
			clean[k] = v
		}
	}
	return clean
}

// filterToolCalls 只保留「可下发」的工具调用：工具名非空且参数可解析。
//
// 必需的防御：上游有时会把 finish_reason 标成 tool_calls，但实际没给出可解析的调用
// （调用体缺失、工具名为空、arguments 被截断成非法 JSON）。若照原样把
// stop_reason 置为 tool_use，客户端（Claude Code）就会报
// "The model's tool call could not be parsed"，且不会显示任何正文。
func filterToolCalls(toolCalls []map[string]any, order []int, schemas map[string]map[string]any) []toolCallParts {
	out := make([]toolCallParts, 0, len(order))
	for _, ti := range order {
		if ti < 0 || ti >= len(toolCalls) {
			continue
		}
		tc := toolCalls[ti]
		if tc == nil {
			continue
		}
		name, argsStr := "", ""
		if fn, ok := tc["function"].(map[string]any); ok && fn != nil {
			name, _ = fn["name"].(string)
			argsStr, _ = fn["arguments"].(string)
		}
		name = sanitizeToolName(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		input, ok := repairJSONArgs(argsStr)
		if !ok {
			continue
		}
		input = conformToolInput(input, schemas[name])
		id, _ := tc["id"].(string)
		// Anthropic 客户端（Claude Desktop / Claude Code）期望 tool_use id 为
		// toolu_ 前缀；上游给的是 OpenAI 风格 call_ 前缀。不规范化会导致客户端
		// 报 "The model's tool call could not be parsed"。
		id = strings.TrimPrefix(strings.TrimSpace(id), "call_")
		if id == "" {
			id = name
		}
		if !strings.HasPrefix(id, "toolu_") {
			id = "toolu_" + id
		}
		out = append(out, toolCallParts{ID: id, Name: name, Input: input})
	}
	return out
}

// iotaRange 生成 0..n-1 的索引切片。
func iotaRange(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// AggregateAsAnthropic 把聚合后的 OpenAI 响应转成 Anthropic message 对象（非流式）。
// tools 为发往上游的 OpenAI 格式工具定义，用于过滤模型幻觉出的未定义参数；
// wantThinking 为 false 时不输出 thinking 块（严格客户端会拒收未请求的思考块）。
func AggregateAsAnthropic(resp map[string]any, model string, tools any, wantThinking bool) map[string]any {
	schemas := toolSchemas(tools)
	var (
		content    []any
		stopReason = "end_turn"
	)
	hasToolUse := false
	getMessage := func() map[string]any {
		ch, _ := resp["choices"].([]any)
		if len(ch) == 0 {
			return nil
		}
		c, _ := ch[0].(map[string]any)
		if c == nil {
			return nil
		}
		m, _ := c["message"].(map[string]any)
		if fr, ok := c["finish_reason"].(string); ok {
			switch fr {
			case "tool_calls":
				stopReason = "tool_use"
			case "length":
				stopReason = "max_tokens"
			}
		}
		return m
	}

	if m := getMessage(); m != nil {
		if rc, ok := m["reasoning_content"].(string); ok && rc != "" && wantThinking {
			content = append(content, map[string]any{"type": "thinking", "thinking": rc, "signature": syntheticSignature})
		}
		if txt, ok := m["content"].(string); ok && txt != "" {
			content = append(content, map[string]any{"type": "text", "text": txt})
		}
		// 注意：Aggregate 构造的 tool_calls 是 []map[string]any（非 []any），
		// 直接断言 []any 会永远失败，导致 Anthropic 路径丢失全部工具调用。
		// 这里同时兼容两种形态。
		switch tcv := m["tool_calls"].(type) {
		case []map[string]any:
			if valid := filterToolCalls(tcv, iotaRange(len(tcv)), schemas); len(valid) > 0 {
				stopReason = "tool_use"
				hasToolUse = true
				for _, vc := range valid {
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    vc.ID,
						"name":  vc.Name,
						"input": vc.Input,
					})
				}
			} else if stopReason == "tool_use" {
				stopReason = "end_turn"
				log.Printf("messages: aggregate has finish_reason=tool_calls but no parsable call, downgrade to end_turn")
			}
		case []any:
			if len(tcv) == 0 {
				break
			}
			calls := make([]map[string]any, 0, len(tcv))
			order := make([]int, 0, len(tcv))
			for i, it := range tcv {
				if tc, ok2 := it.(map[string]any); ok2 && tc != nil {
					calls = append(calls, tc)
					order = append(order, i)
				}
			}
			if valid := filterToolCalls(calls, order, schemas); len(valid) > 0 {
				stopReason = "tool_use"
				hasToolUse = true
				for _, vc := range valid {
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    vc.ID,
						"name":  vc.Name,
						"input": vc.Input,
					})
				}
			} else if stopReason == "tool_use" {
				stopReason = "end_turn"
				log.Printf("messages: aggregate has finish_reason=tool_calls but no parsable call, downgrade to end_turn")
			}
		}
	}
	// 兜底：finish_reason 标了 tool_calls（或调用全部无法解析）却没有可下发的
	// tool_use 块时，降级为 end_turn，避免客户端报
	// "The model's tool call could not be parsed" 且看不到任何正文。
	if stopReason == "tool_use" && !hasToolUse {
		stopReason = "end_turn"
		log.Printf("messages: no parsable tool_use content, downgrade stop_reason to end_turn")
	}
	if len(content) == 0 {
		content = []any{map[string]any{"type": "text", "text": ""}}
	}

	inTok, outTok := 0, 0
	if u, ok := resp["usage"].(map[string]any); ok {
		inTok, outTok = usageTokens(u)
	}
	if inTok == 0 && outTok == 0 {
		// 上游未给 usage 时按文本长度粗估，避免客户端读到 0 后误判
		outTok = EstimateTokens(flattenAnthropicContent(content))
	}

	log.Printf("messages aggregate done: model=%s stop=%s blocks=%d in=%d out=%d", model, stopReason, len(content), inTok, outTok)
	return map[string]any{
		"id":            newMessageID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": inTok, "output_tokens": outTok},
	}
}

// flattenAnthropicContent 把 Anthropic content 块压平成纯文本（用于 token 估算）。
func flattenAnthropicContent(content []any) string {
	var parts []string
	for _, it := range content {
		bm, _ := it.(map[string]any)
		if bm == nil {
			continue
		}
		if s, ok := bm["text"].(string); ok {
			parts = append(parts, s)
		}
		if s, ok := bm["thinking"].(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "")
}

// CountTokens 估算 Anthropic 请求的 input_tokens（供 /v1/messages/count_tokens 使用）。
// 上游没有计数接口，这里用「CJK 按字、其余按 4 字符/token」的近似值。
func CountTokens(req map[string]any) int {
	var sb strings.Builder
	sb.WriteString(anthropicText(req["system"]))
	if raw, ok := req["messages"].([]any); ok {
		for _, rm := range raw {
			m, _ := rm.(map[string]any)
			if m == nil {
				continue
			}
			sb.WriteString(anthropicText(m["content"]))
		}
	}
	if raw, ok := req["tools"]; ok {
		if arr, ok2 := raw.([]any); ok2 {
			if b, err := json.Marshal(arr); err == nil {
				sb.Write(b)
			}
		}
	}
	return EstimateTokens(sb.String())
}

// EstimateTokens 粗估文本的 token 数：CJK 字符按 1 token/字，其余按 4 字符/token。
func EstimateTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF || r >= 0x3400 && r <= 0x4DBF || r >= 0x3040 && r <= 0x30FF {
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4
}

// mergeToolCallStream 把流式到达的 tool_call 分片合并进累计集合，返回 (集合, 顺序)。
func mergeToolCallStream(acc []map[string]any, order []int, tcs []any) ([]map[string]any, []int) {
	for _, it := range tcs {
		tc, ok := it.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		if idx >= len(acc) {
			// 扩容到能容纳该 index
			for len(acc) <= idx {
				acc = append(acc, map[string]any{"index": idx, "function": map[string]any{}})
			}
		}
		cur := acc[idx]
		if v, ok := tc["id"].(string); ok && v != "" {
			cur["id"] = v
		}
		if v, ok := tc["type"].(string); ok && v != "" {
			cur["type"] = v
		}
		df, _ := tc["function"].(map[string]any)
		if df == nil {
			continue
		}
		mf, _ := cur["function"].(map[string]any)
		if mf == nil {
			mf = map[string]any{}
			cur["function"] = mf
		}
		if v, ok := df["name"].(string); ok && v != "" {
			mf["name"] = v
		}
		if v, ok := df["arguments"].(string); ok && v != "" {
			if prev, _ := mf["arguments"].(string); prev != "" {
				mf["arguments"] = prev + v
			} else {
				mf["arguments"] = v
			}
		}
		if !containsInt(order, idx) {
			order = append(order, idx)
		}
	}
	return acc, order
}

func containsInt(arr []int, v int) bool {
	for _, x := range arr {
		if x == v {
			return true
		}
	}
	return false
}
