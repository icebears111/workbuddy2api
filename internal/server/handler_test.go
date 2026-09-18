package server

import (
	"bufio"
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

// fakeUpstream 起一个假的 CodeBuddy 上游：
//   - POST /v2/chat/completions → 按 mode 返回 SSE / 401 / 429
//   - GET  /v2/models           → 返回 OpenAI 风格模型列表
func fakeUpstream(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","owned_by":"zhipu"},{"id":"auto"}]}`))
			return
		}
		if r.URL.Path != "/v2/chat/completions" {
			http.NotFound(w, r)
			return
		}
		// 校验认证头
		if r.Header.Get("Authorization") == "" && r.Header.Get("X-Api-Key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid_format"}`))
			return
		}
		// 上游只接受流式
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("upstream got invalid json: %v", err)
		}
		if got["stream"] != true {
			t.Errorf("upstream expected stream=true, got %v", got["stream"])
		}

		switch mode {
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid_format"}`))
		case "429":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			chunks := []string{
				`{"id":"c1","created":1700000000,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
				`{"id":"c1","created":1700000000,"model":"glm-5.2","choices":[{"index":0,"delta":{"content":"好"}}]}`,
				`{"id":"c1","created":1700000000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			}
			for _, c := range chunks {
				_, _ = io.WriteString(w, "data: "+c+"\n\n")
				fl.Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestHandler(t *testing.T, mode string, apiKey string) (*Handler, *pool.Pool) {
	t.Helper()
	up := upstream.NewWithBase(fakeUpstream(t, mode).URL)
	p := pool.New("") // 不落盘
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       apiKey,
		MaxRotate:    3,
		SoftCooldown: 50 * time.Millisecond,
		HardCooldown: 50 * time.Millisecond,
		ErrCooldown:  50 * time.Millisecond,
		ErrThreshold: 5,
	})
	return h, p
}

func do(t *testing.T, h *Handler, method, path, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthRequired(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "sk-1")
	if w := do(t, h, "GET", "/v1/models", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", w.Code)
	}
	if w := do(t, h, "GET", "/v1/models", "wrong-key", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: got %d, want 401", w.Code)
	}
	if w := do(t, h, "GET", "/v1/models", "sk-1", ""); w.Code != http.StatusOK {
		t.Fatalf("good key: got %d, want 200", w.Code)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	w := do(t, h, "GET", "/v1/models", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Object != "list" || len(out.Data) != 2 {
		t.Fatalf("unexpected payload: %s", w.Body.String())
	}
	if out.Data[0].ID != "auto" || out.Data[1].ID != "glm-5.2" {
		t.Fatalf("models not sorted: %+v", out.Data)
	}
}

func TestNonStreamAggregated(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	w := do(t, h, "POST", "/v1/chat/completions", "",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("object = %v", out["object"])
	}
	choices, _ := out["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("content = %v", msg["content"])
	}
	if out["usage"] == nil {
		t.Fatal("usage missing")
	}
}

func TestStreamPassthrough(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	w := do(t, h, "POST", "/v1/chat/completions", "",
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	var content strings.Builder
	sawDone := false
	sc := bufio.NewScanner(w.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if chunk["model"] != "glm-5.2" {
			t.Fatalf("chunk model = %v", chunk["model"])
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			d, _ := c.(map[string]any)["delta"].(map[string]any)
			if s, ok := d["content"].(string); ok {
				content.WriteString(s)
			}
		}
	}
	if !sawDone {
		t.Fatal("missing [DONE]")
	}
	if content.String() != "你好" {
		t.Fatalf("streamed content = %q", content.String())
	}
}

func TestUpstream401DisablesAccount(t *testing.T) {
	h, p := newTestHandler(t, "401", "")
	w := do(t, h, "POST", "/v1/chat/completions", "",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	st, ok := p.Status("acct-1")
	if !ok {
		t.Fatal("account missing")
	}
	if !st.Disabled {
		t.Fatalf("account should be disabled, got %+v", st)
	}
}

func TestUpstream429CooldownsAccount(t *testing.T) {
	h, p := newTestHandler(t, "429", "")
	w := do(t, h, "POST", "/v1/chat/completions", "",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	st, _ := p.Status("acct-1")
	if !st.Cooling {
		t.Fatalf("account should be cooling, got %+v", st)
	}
	if st.Disabled {
		t.Fatal("rate limit must not disable the account")
	}
}

func TestHealthzAndAdmin(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	if w := do(t, h, "GET", "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatalf("healthz = %d", w.Code)
	} else if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz body = %s", w.Body.String())
	}
	if w := do(t, h, "GET", "/admin", "", ""); w.Code != http.StatusOK {
		t.Fatalf("admin = %d", w.Code)
	} else if !strings.Contains(w.Body.String(), "CodeBuddy2API") {
		t.Fatal("admin page missing marker")
	}
}

func TestValidationErrors(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}]}`, // 缺 model
		`{"model":"glm-5.2","messages":[]}`,             // 缺 messages
		`not-json`,
	} {
		if w := do(t, h, "POST", "/v1/chat/completions", "", body); w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: got %d, want 400", body, w.Code)
		}
	}
}

// TestRetiredProtocolsGone 锁定「只提供 OpenAI Chat」这一对外契约：
// /v1/messages、/v1/messages/count_tokens、/v1/responses 必须返回 410 Gone
// 且提示迁移到转换层；未带 key 时仍是 401（不向扫描器泄露端点状态）。
func TestRetiredProtocolsGone(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "sk-1")
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/responses"} {
		w := do(t, h, "POST", path, "sk-1", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusGone {
			t.Fatalf("%s: got %d, want 410", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "endpoint_retired") {
			t.Fatalf("%s: body missing endpoint_retired marker: %s", path, w.Body.String())
		}
		// 未带 key：鉴权先行，仍是 401
		if w := do(t, h, "POST", path, "", `{}`); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s without key: got %d, want 401", path, w.Code)
		}
	}
}

// newProtoHandler 构造一个「非 OpenAI 协议已放开」的 handler，
// 用于验证开关打开后端点真的能干活（而不只是不返回 410）。
func newProtoHandler(t *testing.T) *Handler {
	t.Helper()
	up := upstream.NewWithBase(fakeUpstream(t, "ok").URL)
	p := pool.New("") // 不落盘
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	return NewHandler(Config{
		Pool:                    p,
		Upstream:                up,
		APIKey:                  "sk-1",
		MaxRotate:               3,
		SoftCooldown:            50 * time.Millisecond,
		HardCooldown:            50 * time.Millisecond,
		ErrCooldown:             50 * time.Millisecond,
		ErrThreshold:            5,
		EnableAnthropicProtocol: true,
		EnableResponsesProtocol: true,
	})
}

// TestProtocolsEnabledByConfig 锁定「开关打开后端点真正可用」：
// 配置里显式打开 EnableAnthropicProtocol / EnableResponsesProtocol 时，
// 三个端点必须注册真实 handler，不再 410 Gone，且返回对应协议的合法结构。
//
// 与 TestRetiredProtocolsGone（默认关闭 → 410）配对，两者一起保证
// 「默认只提供 OpenAI Chat、按需放开」这一行为不会被后续改动破坏。
func TestProtocolsEnabledByConfig(t *testing.T) {
	h := newProtoHandler(t)

	// 非流式：应得到 Anthropic message 对象。
	w := do(t, h, "POST", "/v1/messages", "sk-1",
		`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/messages: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if msg["type"] != "message" {
		t.Fatalf("type = %v, want message", msg["type"])
	}
	if id, _ := msg["id"].(string); !strings.HasPrefix(id, "msg_") {
		t.Fatalf("id = %q, want msg_ prefix", id)
	}
	blocks, _ := msg["content"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("content empty: %s", w.Body.String())
	}
	if first, _ := blocks[0].(map[string]any); first["type"] != "text" || first["text"] != "你好" {
		t.Fatalf("first block = %v, want text 你好", first)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn", msg["stop_reason"])
	}

	// 流式：必须是 Anthropic 事件序列，而不是 OpenAI 的 data: 帧。
	w = do(t, h, "POST", "/v1/messages", "sk-1",
		`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/messages stream: got %d, want 200: %s", w.Code, w.Body.String())
	}
	for _, ev := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(w.Body.String(), ev) {
			t.Fatalf("stream missing %q:\n%s", ev, w.Body.String())
		}
	}

	// count_tokens：给出正数估算值。
	w = do(t, h, "POST", "/v1/messages/count_tokens", "sk-1",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"你好世界"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/messages/count_tokens: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var cnt map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cnt); err != nil {
		t.Fatalf("decode count_tokens: %v", err)
	}
	if n, _ := cnt["input_tokens"].(float64); n <= 0 {
		t.Fatalf("input_tokens = %v, want > 0", cnt["input_tokens"])
	}

	// /v1/responses：不再 410，且能正常返回。
	w = do(t, h, "POST", "/v1/responses", "sk-1", `{"model":"glm-5.2","input":"hi"}`)
	if w.Code == http.StatusGone {
		t.Fatalf("/v1/responses still 410 after enabling: %s", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/responses: got %d, want 200: %s", w.Code, w.Body.String())
	}

	// 放开协议不改变鉴权顺序：未带 key 仍 401。
	if w := do(t, h, "POST", "/v1/messages", "", `{}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", w.Code)
	}
}

// TestProtocolFlagsAreIndependent 锁定两个开关互不牵连：
// 只开 Anthropic 时，/v1/responses 必须仍然 410。
func TestProtocolFlagsAreIndependent(t *testing.T) {
	up := upstream.NewWithBase(fakeUpstream(t, "ok").URL)
	p := pool.New("")
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	h := NewHandler(Config{
		Pool:                    p,
		Upstream:                up,
		APIKey:                  "sk-1",
		EnableAnthropicProtocol: true,
		// EnableResponsesProtocol 保持默认 false
	})

	if w := do(t, h, "POST", "/v1/messages", "sk-1",
		`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`); w.Code != http.StatusOK {
		t.Fatalf("/v1/messages: got %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := do(t, h, "POST", "/v1/responses", "sk-1", `{"model":"glm-5.2","input":"hi"}`); w.Code != http.StatusGone {
		t.Fatalf("/v1/responses: got %d, want 410 (flag off)", w.Code)
	}
}
