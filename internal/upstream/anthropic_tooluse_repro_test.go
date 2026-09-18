// anthropic_tooluse_repro_test.go —— 复现 NAS 上 Claude Code 报
// "The model's tool call could not be parsed" 的场景（纯离线，不碰任何上游）。
//
// 复现依据（NAS 网关 2026-09-19 05:42 的真实日志）：
//
//	model="hy4-preview" stream=true msgs=2 tools=26 max_tokens=64000 thinking=true
//	toolSchemas: total=26 resolved=24 emptyProps=2 unresolved=[CronList EnterPlanMode]
//	stream tool_use: Bash id=toolu_chatcmpl-tool-b13618cc60c87d27
//	  args={"command":"pwd && ls -la","description":"Show current directory and list files"}
//	stream done: stop=tool_use text_len=36 tool_calls=1 reasoning_len=144 out_tokens=64
//
// 关键：thinking=true 且同时有 text(36) 与 tool_use(1)，即三个 content block 都要下发。
// 本测试断言 SSE 事件的 index 与结构是否符合 Anthropic 协议。
package upstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// claudeCodeTools 近似 Claude Code 的真实工具清单（26 个，含 2 个无 properties 的）。
// 用真实的 input_schema 形态，确保 toolSchemas 行为与线上一致。
func claudeCodeTools() []any {
	mk := func(name string, props map[string]any) map[string]any {
		return map[string]any{
			"name": name,
			"input_schema": map[string]any{
				"type":       "object",
				"properties": props,
			},
		}
	}
	// Bash：注意 description / timeout 都是 Claude Code schema 里的合法字段
	bash := mk("Bash", map[string]any{
		"command":     map[string]any{"type": "string"},
		"description": map[string]any{"type": "string"},
		"timeout":     map[string]any{"type": "number"},
	})
	tools := []any{
		bash,
		mk("Read", map[string]any{"file_path": map[string]any{"type": "string"}}),
		mk("Glob", map[string]any{"pattern": map[string]any{"type": "string"}}),
		mk("Grep", map[string]any{"pattern": map[string]any{"type": "string"}}),
		// 无 properties 的工具（对应日志 unresolved=[CronList EnterPlanMode]）
		map[string]any{"name": "CronList", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}},
		map[string]any{"name": "EnterPlanMode", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}},
	}
	return tools
}

// TestRerepro_ThinkingTextToolUseIndices 复现「thinking + text + tool_use」三块并存的 index 分配。
func TestRerepro_ThinkingTextToolUseIndices(t *testing.T) {
	// 上游 SSE：先推理（144 字节级）、再正文（36 字符级）、最后 tool_calls
	sse := "" +
		`data: {"id":"c1","choices":[{"delta":{"reasoning_content":"用户让我读项目。我需要先看看目录结构，然后阅读关键文件来理解这个项目。"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"content":"我来看看这个项目。"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"chatcmpl-tool-b13618cc60c87d27","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"pwd && ls -la\",\"description\":\"Show current directory and list files\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9814,"completion_tokens":64}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	err := StreamAsAnthropic(&buf, strings.NewReader(sse), "hy4-preview",
		anthropicToolsToChat(claudeCodeTools()), true, nil)
	if err != nil {
		t.Fatalf("StreamAsAnthropic: %v", err)
	}

	events := parseEvents(t, buf.String())
	t.Logf("事件序列: %v", eventNames(events))

	// 收集每个 content_block_start 的 index 与类型
	type block struct {
		idx  float64
		typ  string
		name string
		id   string
	}
	var blocks []block
	for _, e := range events {
		if e[0] != "content_block_start" {
			continue
		}
		d := decode(t, e[1])
		cb, _ := d["content_block"].(map[string]any)
		typ, _ := cb["type"].(string)
		name, _ := cb["name"].(string)
		id, _ := cb["id"].(string)
		idx, _ := d["index"].(float64)
		blocks = append(blocks, block{idx: idx, typ: typ, name: name, id: id})
		t.Logf("  content_block_start index=%v type=%s name=%s id=%s", idx, typ, name, id)
	}

	if len(blocks) != 3 {
		t.Fatalf("期望 3 个 content block（thinking/text/tool_use），实际 %d 个: %+v", len(blocks), blocks)
	}
	// index 必须从 0 开始严格递增（Anthropic 协议硬要求）
	for i, b := range blocks {
		if int(b.idx) != i {
			t.Errorf("block[%d] index=%v，期望 %d —— index 不连续会让客户端解析失败", i, b.idx, i)
		}
	}
	if blocks[0].typ != "thinking" {
		t.Errorf("block[0] 应为 thinking，实际 %s", blocks[0].typ)
	}
	if blocks[1].typ != "text" {
		t.Errorf("block[1] 应为 text，实际 %s", blocks[1].typ)
	}
	if blocks[2].typ != "tool_use" {
		t.Errorf("block[2] 应为 tool_use，实际 %s", blocks[2].typ)
	}

	// tool_use 块必须带合法 id（toolu_ 前缀）与 name
	if !strings.HasPrefix(blocks[2].id, "toolu_") {
		t.Errorf("tool_use id=%q 缺少 toolu_ 前缀", blocks[2].id)
	}
	if blocks[2].name != "Bash" {
		t.Errorf("tool_use name=%q，期望 Bash", blocks[2].name)
	}

	// input_json_delta 必须是合法 JSON，且 description 作为 Bash 的合法字段应保留
	for _, e := range events {
		if e[0] != "content_block_delta" {
			continue
		}
		d := decode(t, e[1])
		delta, _ := d["delta"].(map[string]any)
		if delta["type"] == "input_json_delta" {
			pj, _ := delta["partial_json"].(string)
			t.Logf("  input_json_delta partial_json=%s", pj)
			var parsed map[string]any
			if err := json.Unmarshal([]byte(pj), &parsed); err != nil {
				t.Fatalf("partial_json 不是合法 JSON（客户端必然解析失败）: %v — %s", err, pj)
			}
			if parsed["command"] != "pwd && ls -la" {
				t.Errorf("command 丢失或错误: %v", parsed["command"])
			}
		}
	}

	// message_delta 的 stop_reason 必须是 tool_use
	md := decode(t, events[len(events)-2][1])
	delta, _ := md["delta"].(map[string]any)
	if delta["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v，期望 tool_use", delta["stop_reason"])
	}
}

// TestRerepro_OnlyToolUseNoText 复现「只有 tool_use、无 text（text_len=0）」的情形。
func TestRerepro_OnlyToolUseNoText(t *testing.T) {
	sse := "" +
		`data: {"choices":[{"delta":{"reasoning_content":"想一下"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"chatcmpl-tool-a33637abfca1f93e","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"pwd && ls -la\",\"description\":\"Show current directory and contents\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":75}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsAnthropic(&buf, strings.NewReader(sse), "hy4-preview",
		anthropicToolsToChat(claudeCodeTools()), true, nil); err != nil {
		t.Fatalf("StreamAsAnthropic: %v", err)
	}
	events := parseEvents(t, buf.String())
	t.Logf("事件序列: %v", eventNames(events))

	var idxs []float64
	for _, e := range events {
		if e[0] == "content_block_start" {
			d := decode(t, e[1])
			idx, _ := d["index"].(float64)
			cb, _ := d["content_block"].(map[string]any)
			typ, _ := cb["type"].(string)
			idxs = append(idxs, idx)
			t.Logf("  block index=%v type=%s", idx, typ)
		}
	}
	// thinking(0) + tool_use(1)
	if len(idxs) != 2 || idxs[0] != 0 || idxs[1] != 1 {
		t.Errorf("index 序列 = %v，期望 [0 1]", idxs)
	}
}

// TestRerepro_ToolCallWithExtraFieldsKeptAsLegal 明确记录：
// Claude Code 的 Bash schema 里 description/timeout 是合法字段，
// conformToolInput 不剔除它们是**正确**的（此前曾误判为 bug）。
func TestRerepro_ToolCallWithExtraFieldsKeptAsLegal(t *testing.T) {
	schemas := toolSchemas(anthropicToolsToChat(claudeCodeTools()))
	props := schemas["Bash"]
	if props == nil {
		t.Fatal("Bash 的 schema 未被收录（这才是真 bug 信号）")
	}
	for _, k := range []string{"command", "description", "timeout"} {
		if _, ok := props[k]; !ok {
			t.Errorf("Bash schema 缺少合法字段 %s: %v", k, keys(props))
		}
	}
	in := map[string]any{"command": "ls", "description": "list", "timeout": 1000.0}
	got := conformToolInput(in, props)
	if len(got) != 3 {
		t.Errorf("三个字段都是 Bash 的合法参数，应全部保留，实际保留 %d 个: %v", len(got), got)
	}
	// 真正的幻觉字段（schema 外）必须被剔除
	in2 := map[string]any{"command": "ls", "hallucinated": "x"}
	got2 := conformToolInput(in2, props)
	if _, bad := got2["hallucinated"]; bad {
		t.Error("schema 外的 hallucinated 字段应被剔除")
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAnthropicSSE_EveryDataHasType 回归测试：Anthropic SSE 规范要求每个
// data 事件的 JSON 必须自带顶层 "type" 字段。缺失会让 Claude Code 丢弃该
// content block（其内部计数为 discarded_block_count/tombstoned_had_tool_use），
// 进而报 "The model's tool call could not be parsed"。
//
// 历史 bug（2026-09-19 修复）：content_block_start/delta/stop 三个事件全部漏了
// 顶层 type，而 message_start/delta/stop 都有——这个不对称导致了 NAS 上
// Claude Code 直连时全部工具调用失败。
func TestAnthropicSSE_EveryDataHasType(t *testing.T) {
	sse := "" +
		`data: {"choices":[{"delta":{"reasoning_content":"想"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"正文"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"toolu_01_x","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsAnthropic(&buf, strings.NewReader(sse), "m", nil, true, nil); err != nil {
		t.Fatalf("StreamAsAnthropic: %v", err)
	}

	// event: 名与 data.type 必须逐条一致（Anthropic 客户端两者都会校验）
	for _, e := range parseEvents(t, buf.String()) {
		eventName, dataJSON := e[0], e[1]
		var m map[string]any
		if err := json.Unmarshal([]byte(dataJSON), &m); err != nil {
			t.Fatalf("data 非法 JSON: %v — %s", err, dataJSON)
		}
		typ, _ := m["type"].(string)
		if typ == "" {
			t.Errorf("event=%s 的 data 缺少顶层 type 字段（客户端会丢弃该块）: %s", eventName, dataJSON)
			continue
		}
		if typ != eventName {
			t.Errorf("event=%s 与 data.type=%s 不一致", eventName, typ)
		}
	}
}

// TestAnthropicSSE_ContentBlockEventsCarryType 更精确地锁定三个曾漏 type 的事件。
func TestAnthropicSSE_ContentBlockEventsCarryType(t *testing.T) {
	sse := "" +
		`data: {"choices":[{"delta":{"content":"t"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"toolu_01_x","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsAnthropic(&buf, strings.NewReader(sse), "m", nil, false, nil); err != nil {
		t.Fatalf("StreamAsAnthropic: %v", err)
	}
	want := map[string]bool{
		"content_block_start": false,
		"content_block_delta": false,
		"content_block_stop":  false,
	}
	for _, e := range parseEvents(t, buf.String()) {
		var m map[string]any
		_ = json.Unmarshal([]byte(e[1]), &m)
		if typ, _ := m["type"].(string); typ != "" {
			if _, ok := want[typ]; ok {
				want[typ] = true
			}
		}
	}
	for ev, seen := range want {
		if !seen {
			t.Errorf("事件 %s 从未带顶层 type（历史 bug 复发）", ev)
		}
	}
}
