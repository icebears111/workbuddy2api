// anthropic_e2e_debug_test.go —— 端到端 dump：让网关对一个假上游发出完整的
// /v1/messages 流式响应，并把网关写回客户端的**每一个 SSE 字节**打印出来，
// 用于逐字节对照 Anthropic 协议（纯离线，不碰任何真实上游）。
//
// 复现依据（NAS 网关 2026-09-19 05:57 的真实失败请求）：
//
//	model="deepseek-v4.1-flash" stream=true msgs=2 tools=30 max_tokens=32000 thinking=true
//	toolSchemas: total=30 resolved=27 emptyProps=3 unresolved=[CronList EnterPlanMode TaskList]
//	stream tool_use: Bash id=toolu_00_TiJULQy2dfzR2PuYevJP6370
//	                 args={"command":"pwd && ls -la","description":"Show current directory and contents"}
//	stream done: stop=tool_use text_len=84 tool_calls=2 reasoning_len=0
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codebuddy2api/internal/cred"
	"codebuddy2api/internal/pool"
	"codebuddy2api/internal/upstream"
)

// fakeToolUpstream 假上游：对一个带 tools 的请求返回 thinking-less、
// text + 两个 tool_calls 的 SSE（对齐 05:57:33 的真实形态）。
func fakeToolUpstream(t *testing.T, sseChunks []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v2/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-v4.1-flash"}]}`))
			return
		}
		if r.URL.Path != "/v2/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, c := range sseChunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newToolHandler(t *testing.T, srv *httptest.Server) *Handler {
	t.Helper()
	up := upstream.NewWithBase(srv.URL)
	p := pool.New("")
	p.Add(&cred.Cred{UID: "a1", Nickname: "a1", Token: "ck_test", Kind: cred.KindAPIKey})
	return NewHandler(Config{
		Pool: p, Upstream: up, APIKey: "sk-1", MaxRotate: 3,
		SoftCooldown: 50 * time.Millisecond, HardCooldown: 50 * time.Millisecond,
		ErrCooldown: 50 * time.Millisecond, ErrThreshold: 5,
		EnableAnthropicProtocol: true,
	})
}

// claudeStyleTools 构造 30 个 Anthropic 工具（模拟 Claude Code 的清单，
// 含 3 个无 properties 的，对齐日志 unresolved=[CronList EnterPlanMode TaskList]）。
func claudeStyleTools() []any {
	mk := func(name string, props map[string]any) map[string]any {
		return map[string]any{"name": name,
			"input_schema": map[string]any{"type": "object", "properties": props}}
	}
	out := []any{
		mk("Bash", map[string]any{
			"command":     map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"timeout":     map[string]any{"type": "number"},
		}),
		mk("Read", map[string]any{"file_path": map[string]any{"type": "string"}}),
		mk("Glob", map[string]any{"pattern": map[string]any{"type": "string"}}),
	}
	for _, n := range []string{"CronList", "EnterPlanMode", "TaskList"} {
		out = append(out, mk(n, map[string]any{}))
	}
	for i := len(out); i < 30; i++ {
		out = append(out, mk("Tool"+string(rune('A'+i)), map[string]any{"x": map[string]any{"type": "string"}}))
	}
	return out
}

// TestE2E_DumpAnthropicSSE 打印网关写回的完整 SSE，供逐字节对照协议。
func TestE2E_DumpAnthropicSSE(t *testing.T) {
	chunks := []string{
		// 84 字符正文（对齐日志 text_len=84）
		`{"id":"c1","created":1700000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"我先看看这个项目的目录结构和 git 历史，了解它是什么项目、用了什么技术栈，然后再阅读关键文件。"}}]}`,
		// 两个 tool_calls
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_00_TiJULQy2dfzR2PuYevJP6370","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"pwd && ls -la\",\"description\":\"Show current directory and contents\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"toolu_01_pvA5XgiqfbHg5zVqq28z3582","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"git log --oneline -10\",\"description\":\"Show recent git history\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7062,"completion_tokens":143,"total_tokens":7205}}`,
	}
	h := newToolHandler(t, fakeToolUpstream(t, chunks))

	body := map[string]any{
		"model":      "deepseek-v4.1-flash",
		"stream":     true,
		"max_tokens": 32000,
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 10000},
		"system":     "You are Claude Code.",
		"messages": []any{
			map[string]any{"role": "user", "content": "你好 阅读一下这个项目"},
		},
		"tools": claudeStyleTools(),
	}
	raw, _ := json.Marshal(body)
	w := do(t, h, "POST", "/v1/messages", "sk-1", string(raw))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	out := w.Body.String()
	t.Logf("========== 网关写回的完整 SSE（%d 字节）==========", len(out))
	// 按行打印，带行号，方便逐行核对
	for i, line := range strings.Split(out, "\n") {
		t.Logf("%3d| %s", i+1, line)
	}
	t.Logf("========== SSE 结束 ==========")

	// 结构化检查：data 行必须是合法 JSON
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Errorf("data 行不是合法 JSON: %v — %s", err, payload)
		}
	}
}
