package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codebuddy2api/internal/apikey"
	"codebuddy2api/internal/cred"
	"codebuddy2api/internal/modelstate"
	"codebuddy2api/internal/pool"
	"codebuddy2api/internal/upstream"
)

// 带模型状态表的 handler（其余依赖同 newTestHandler）。
func newModelStateHandler(t *testing.T) (*Handler, *modelstate.Store) {
	t.Helper()
	h, _ := newTestHandler(t, "ok", "sk-global")
	store := modelstate.NewStore(filepath.Join(t.TempDir(), "models.json"))
	h.cfg.ModelState = store
	h.cfg.KeyStore = nil
	return h, store
}

// 构造时就带上 key 表与模型状态表。
//
// ⚠ 守卫是在 NewHandler 里 `requireAdmin(cfg.APIKey, cfg.KeyStore, ...)`
// 注册的 —— cfg 按值捕获，**事后改 h.cfg.KeyStore 不会影响已注册的闭包**。
// 第一版测试就是这么写的，于是调用方 key 一直命中「未知 key → 401」，
// 看起来像守卫太严，其实是测试没把 store 传进构造。
func newHandlerWithStores(t *testing.T) (*Handler, *apikey.Store, *modelstate.Store, *apikey.Key) {
	t.Helper()
	ks, err := apikey.NewStore(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	callerKey, err := ks.Create("caller", "test", "member-1")
	if err != nil {
		t.Fatal(err)
	}
	ms := modelstate.NewStore(filepath.Join(t.TempDir(), "models.json"))

	up := upstream.NewWithBase(fakeUpstream(t, "ok").URL)
	p := pool.New("")
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       "sk-global",
		KeyStore:     ks,
		ModelState:   ms,
		MaxRotate:    3,
		SoftCooldown: 50 * time.Millisecond,
		HardCooldown: 50 * time.Millisecond,
		ErrCooldown:  50 * time.Millisecond,
		ErrThreshold: 5,
	})
	return h, ks, ms, callerKey
}

func listedModelIDs(t *testing.T, h *Handler) map[string]bool {
	t.Helper()
	w := do(t, h, "GET", "/v1/models", "sk-global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range resp.Data {
		out[m.ID] = true
	}
	return out
}

// 被禁用的模型要从 /v1/models 消失。
func TestBuddyDisabledModelHidden(t *testing.T) {
	h, store := newModelStateHandler(t)
	before := listedModelIDs(t, h)
	if !before["glm-5.2"] {
		t.Skipf("fake upstream does not expose glm-5.2: %v", before)
	}

	store.SetDisabled("glm-5.2", true)
	after := listedModelIDs(t, h)
	if after["glm-5.2"] {
		t.Fatal("disabled model must not be listed")
	}
	if len(after) != len(before)-1 {
		t.Fatalf("want %d models, got %d", len(before)-1, len(after))
	}
}

// ★ 最关键的一条 ★
// buddy 的 MapModelName 会把不认识的模型**静默改写成 fallback** 继续转发。
// 所以「禁用」必须在改写之前拦住，否则会 200 + 用错模型 ——
// 账单与输出都对不上，而且毫无提示。
func TestBuddyDisabledModelRejectedNotSilentlyForwarded(t *testing.T) {
	h, store := newModelStateHandler(t)
	store.SetDisabled("glm-5.2", true)

	w := do(t, h, "POST", "/v1/chat/completions", "sk-global",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (must not silently fall back), got %d: %s",
			w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response must be json: %v", err)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("want error object, got %s", w.Body.String())
	}
	if code := errObj["code"]; code != "model_disabled" {
		t.Fatalf("want code=model_disabled, got %v (%s)", code, w.Body.String())
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "glm-5.2") || !strings.Contains(msg, "/v1/models") {
		t.Fatalf("message should name the model and point at /v1/models: %q", msg)
	}
}

// 大小写不敏感。
func TestBuddyDisabledModelCaseInsensitive(t *testing.T) {
	h, store := newModelStateHandler(t)
	store.SetDisabled("GLM-5.2", true)
	w := do(t, h, "POST", "/v1/chat/completions", "sk-global",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
	}
}

// 管理读接口必须**含禁用项** —— 否则页面上模型消失、开关找不回来。
func TestBuddyManageIncludesDisabled(t *testing.T) {
	h, store := newModelStateHandler(t)
	store.SetDisabled("glm-5.2", true)

	w := do(t, h, "GET", "/api/models/manage", "sk-global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/models/manage = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Models []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"models"`
		DisabledCount int `json:"disabled_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var seen, enabled bool
	for _, m := range resp.Models {
		if m.ID == "glm-5.2" {
			seen, enabled = true, m.Enabled
		}
	}
	if !seen {
		t.Fatal("manage endpoint must include disabled models")
	}
	if enabled {
		t.Fatal("disabled model must be reported enabled=false")
	}
	if resp.DisabledCount != 1 {
		t.Fatalf("want disabled_count=1, got %d", resp.DisabledCount)
	}
}

// 启停：写接口生效，且立刻反映到 /v1/models。
func TestBuddySetModelState(t *testing.T) {
	h, _ := newModelStateHandler(t)

	w := do(t, h, "POST", "/api/models/state", "sk-global", `{"id":"glm-5.2","enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/models/state = %d (%s)", w.Code, w.Body.String())
	}
	if listedModelIDs(t, h)["glm-5.2"] {
		t.Fatal("model should be hidden after disable")
	}

	w = do(t, h, "POST", "/api/models/state", "sk-global", `{"id":"glm-5.2","enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-enable = %d (%s)", w.Code, w.Body.String())
	}
	if !listedModelIDs(t, h)["glm-5.2"] {
		t.Fatal("model should be visible after re-enable")
	}
}

// 入参校验。
func TestBuddySetModelStateValidation(t *testing.T) {
	h, _ := newModelStateHandler(t)
	for _, tc := range []struct{ name, body string }{
		{"bad json", `{`},
		{"missing id", `{"enabled":true}`},
		{"blank id", `{"id":"  ","enabled":true}`},
		{"missing enabled", `{"id":"auto"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h, "POST", "/api/models/state", "sk-global", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// 未启用状态表时明确 503。
func TestBuddySetModelStateWithoutStore(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "sk-global")
	w := do(t, h, "POST", "/api/models/state", "sk-global", `{"id":"auto","enabled":false}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
}

// 没有全局 key / key 表时（内网自用模式）管理接口放行 —— 与既有的
// requireAPIKey 语义一致（未配置鉴权不等于拒绝一切）。
func TestBuddyAdminGuardOpenWhenNoAuthConfigured(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "") // 空 key = 不鉴权
	store := modelstate.NewStore(filepath.Join(t.TempDir(), "models.json"))
	h.cfg.ModelState = store

	w := do(t, h, "POST", "/api/models/state", "", `{"id":"auto","enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 in no-auth mode, got %d (%s)", w.Code, w.Body.String())
	}
}

// 调用方 key（有 owner）不得改模型状态 —— 那只是「用模型」的凭证。
// 否则任何一把外传的 key 都能把模型全禁掉（拒绝服务）。
func TestBuddyCallerKeyCannotChangeModelState(t *testing.T) {
	h, _, store, callerKey := newHandlerWithStores(t)

	w := do(t, h, "POST", "/api/models/state", callerKey.Key, `{"id":"auto","enabled":false}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("caller key must be rejected (403), got %d (%s)", w.Code, w.Body.String())
	}
	if store.IsDisabled("auto") {
		t.Fatal("caller key must not have changed model state")
	}
}

// 同上，但读接口也要挡住（信息面）。
func TestBuddyCallerKeyCannotReadManage(t *testing.T) {
	h, _, _, callerKey := newHandlerWithStores(t)

	w := do(t, h, "GET", "/api/models/manage", callerKey.Key, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("caller key must not read manage endpoint (403), got %d", w.Code)
	}
}
