package upstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 用真实上游格式（每段 chunk 都带 reasoning_content 字段，思考阶段为空串、正文阶段缺失/空）跑转换，
// 量化会产生几个「思考块 / reasoning_content chunk」。
func TestRealFormatThinkingBlocks(t *testing.T) {
	// deepseek-r1 真实形态：首 chunk reasoning_content=""，中间纯推理，之后纯 content
	sse := "" +
		`data: {"id":"x","model":"deepseek-r1","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}]}` + "\n\n" +
		`data: {"id":"x","model":"deepseek-r1","choices":[{"index":0,"delta":{"content":"","reasoning_content":"我们"},"logprobs":null,"finish_reason":""}]}` + "\n\n" +
		`data: {"id":"x","model":"deepseek-r1","choices":[{"index":0,"delta":{"content":"","reasoning_content":"被问到"},"logprobs":null,"finish_reason":""}]}` + "\n\n" +
		`data: {"id":"x","model":"deepseek-r1","choices":[{"index":0,"delta":{"content":"","reasoning_content":"：1+1等于几？"},"logprobs":null,"finish_reason":""}]}` + "\n\n" +
		`data: {"id":"x","model":"deepseek-r1","choices":[{"index":0,"delta":{"content":"北京是中国的首都","reasoning_content":""},"logprobs":null,"finish_reason":""}]}` + "\n\n" +
		`data: {"id":"x","model":"deepseek-r1","choices":[{"index":0,"delta":{"content":"，拥有三千年历史","reasoning_content":""},"logprobs":null,"finish_reason":""}]}` + "\n\n" +
		`data: {"id":"x","model":"deepseek-r1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":23,"completion_tokens":10}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	// --- OpenAI ---
	var ob bytes.Buffer
	if err := StreamAsOpenAI(&ob, strings.NewReader(sse), "deepseek-r1", nil); err != nil {
		t.Fatalf("StreamAsOpenAI: %v", err)
	}
	var rcChunks int
	for _, line := range strings.Split(ob.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if p == "[DONE]" {
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(p), &c); err != nil {
			continue
		}
		ch, _ := c["choices"].([]any)
		if len(ch) == 0 {
			continue
		}
		d, _ := ch[0].(map[string]any)["delta"].(map[string]any)
		if d == nil {
			continue
		}
		if s, ok := d["reasoning_content"].(string); ok && s != "" {
			rcChunks++
			t.Logf("[OpenAI] reasoning_content chunk #%d: %q", rcChunks, s)
		}
	}
	t.Logf("[OpenAI] total reasoning_content chunks = %d (期望 1)", rcChunks)

	// --- Anthropic ---
	var ab bytes.Buffer
	if err := StreamAsAnthropic(&ab, strings.NewReader(sse), "deepseek-r1", nil); err != nil {
		t.Fatalf("StreamAsAnthropic: %v", err)
	}
	var thinkingBlocks int
	for _, blk := range strings.Split(ab.String(), "\n\n") {
		blk = strings.TrimSpace(blk)
		if blk == "" {
			continue
		}
		var ev, data string
		for _, l := range strings.Split(blk, "\n") {
			if strings.HasPrefix(l, "event: ") {
				ev = strings.TrimPrefix(l, "event: ")
			} else if strings.HasPrefix(l, "data: ") {
				data = strings.TrimPrefix(l, "data: ")
			}
		}
		if ev == "content_block_start" {
			var m map[string]any
			if json.Unmarshal([]byte(data), &m) == nil {
				if cb, ok := m["content_block"].(map[string]any); ok {
					if cb["type"] == "thinking" {
						thinkingBlocks++
						t.Logf("[Anthropic] thinking block #%d", thinkingBlocks)
					}
				}
			}
		}
	}
	t.Logf("[Anthropic] total thinking blocks = %d (期望 1)", thinkingBlocks)
}
