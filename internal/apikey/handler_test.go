package apikey

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newTestMux(t *testing.T) (*http.ServeMux, *Store) {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewHandler(s).Register(mux, "/admin/api/keys")
	return mux, s
}

func do(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestCreateReturnsPlaintextOnce(t *testing.T) {
	mux, _ := newTestMux(t)
	w := do(t, mux, "POST", "/admin/api/keys", `{"name":"测试","note":"备注"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	key, _ := resp["key"].(string)
	if !strings.HasPrefix(key, "sk-") {
		t.Fatalf("新建响应应含完整 key，实际 %v", resp["key"])
	}
	if resp["name"] != "测试" || resp["note"] != "备注" {
		t.Fatalf("名称/备注未回显: %v", resp)
	}
}

// ★ 安全断言：列表接口绝不能带明文
func TestListNeverLeaksPlaintext(t *testing.T) {
	mux, store := newTestMux(t)
	w := do(t, mux, "POST", "/admin/api/keys", `{"name":"a"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	secret, _ := created["key"].(string)
	if secret == "" {
		t.Fatal("创建失败")
	}

	w = do(t, mux, "GET", "/admin/api/keys", "")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, secret) {
		t.Fatal("列表响应里出现了明文 key —— 严重泄漏")
	}
	if !strings.Contains(body, "…") {
		t.Fatal("列表应返回脱敏形式")
	}
	// 脱敏形式要能认出是哪把
	var listed struct{ Keys []dto }
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Keys) != 1 || listed.Keys[0].Masked == "" {
		t.Fatalf("脱敏字段缺失: %+v", listed.Keys)
	}
	_ = store
}

func TestRevealNeedsExplicitCall(t *testing.T) {
	mux, _ := newTestMux(t)
	w := do(t, mux, "POST", "/admin/api/keys", `{"name":"r"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id, _ := created["id"].(string)
	secret, _ := created["key"].(string)

	// 不带 /reveal 的详情也不含明文
	w = do(t, mux, "GET", "/admin/api/keys/"+id, "")
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("详情接口不该带明文")
	}
	// 显式 reveal 才给
	w = do(t, mux, "GET", "/admin/api/keys/"+id+"/reveal", "")
	if !strings.Contains(w.Body.String(), secret) {
		t.Fatal("reveal 应返回明文")
	}
}

func TestDeleteEndpoint(t *testing.T) {
	mux, store := newTestMux(t)
	w := do(t, mux, "POST", "/admin/api/keys", `{"name":"待删"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id, _ := created["id"].(string)
	secret, _ := created["key"].(string)

	if w = do(t, mux, "DELETE", "/admin/api/keys/"+id, ""); w.Code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d", w.Code)
	}
	if _, ok := store.Verify(secret); ok {
		t.Fatal("删除后 key 必须失效")
	}
	// 重复删除 → 404
	if w = do(t, mux, "DELETE", "/admin/api/keys/"+id, ""); w.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实际 %d", w.Code)
	}
}

func TestBadRequests(t *testing.T) {
	mux, _ := newTestMux(t)
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/admin/api/keys", `{`, http.StatusBadRequest},
		{"POST", "/admin/api/keys", `{"name":""}`, http.StatusBadRequest},
		{"GET", "/admin/api/keys/nonexistent", "", http.StatusNotFound},
		{"GET", "/admin/api/keys/nonexistent/reveal", "", http.StatusNotFound},
		{"GET", "/admin/api/keys/nonexistent/bogus", "", http.StatusNotFound},
	}
	for _, c := range cases {
		if w := do(t, mux, c.method, c.path, c.body); w.Code != c.want {
			t.Errorf("%s %s → 想要 %d，实际 %d (%s)", c.method, c.path, c.want, w.Code, w.Body.String())
		}
	}
}

func TestOnCreateHookFires(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "k.json"))
	h := NewHandler(s)
	var got string
	h.OnCreate = func(k *Key) { got = k.Name }
	h.OnDelete = func(string) { got = "deleted" }

	mux := http.NewServeMux()
	h.Register(mux, "/admin/api/keys")

	w := do(t, mux, "POST", "/admin/api/keys", `{"name":"钩子"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if got != "钩子" {
		t.Fatalf("OnCreate 未触发: %q", got)
	}
	do(t, mux, "DELETE", "/admin/api/keys/"+created["id"].(string), "")
	if got != "deleted" {
		t.Fatalf("OnDelete 未触发: %q", got)
	}
}
