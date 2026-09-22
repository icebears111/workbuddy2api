package apikey

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// 多用户 key 权限（2026-09-22 收紧）的契约测试。
//
// 起因：站长发现成员的 key 出现在自己的「API Key」页里，且能取到明文。
// 这组测试把三条规则钉死：
//
//	① 成员看到的只有自己的
//	② 管理员能看到成员的（审计需要），但 DTO 带 owner，且 editable=false
//	③ 管理员 **不能** reveal / 删除成员的 key
func newScopedHandler(t *testing.T) *Handler {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatalf("建 store: %v", err)
	}
	h := NewHandler(s)
	h.OwnerFrom = func(r *http.Request) (string, bool) {
		if r.Header.Get("X-Test-Admin") == "1" {
			return "", true
		}
		return r.Header.Get("X-Test-User"), false
	}
	return h
}

// 造两把 key：一把归成员 alice，一把是管理员的（无主）。
func seedTwoKeys(t *testing.T, h *Handler) (memberKey, adminKey *Key) {
	t.Helper()
	mk, err := h.store.Create("member-key", "", "alice")
	if err != nil {
		t.Fatalf("建成员 key: %v", err)
	}
	ak, err := h.store.Create("admin-key", "", "")
	if err != nil {
		t.Fatalf("建管理员 key: %v", err)
	}
	return mk, ak
}

func doReq(h *Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	switch {
	case method == "GET" && target == "/admin/api/keys":
		h.List(w, r)
	case method == "GET" && strings.HasSuffix(target, "/reveal"):
		h.Get(w, r)
	case method == "GET":
		h.Get(w, r)
	case method == "DELETE":
		h.Delete(w, r)
	}
	return w
}

func TestMemberSeesOnlyOwnKeys(t *testing.T) {
	h := newScopedHandler(t)
	seedTwoKeys(t, h)

	w := doReq(h, "GET", "/admin/api/keys", map[string]string{"X-Test-User": "alice"})
	if w.Code != 200 {
		t.Fatalf("成员列自己的 key 应 200，得到 %d", w.Code)
	}
	var resp struct {
		Keys []dto `json:"keys"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Keys) != 1 {
		t.Fatalf("成员应只看到 1 把，得到 %d 把", len(resp.Keys))
	}
	if resp.Keys[0].Name != "member-key" {
		t.Fatalf("成员看到的是 %q，应该是自己那把", resp.Keys[0].Name)
	}
	if !resp.Keys[0].Editable {
		t.Fatal("成员对自己的 key 应可编辑")
	}
}

// ★ 核心回归 ★：管理员能看到成员的 key（审计），但不该能改。
func TestAdminSeesMemberKeysButCannotTouchThem(t *testing.T) {
	h := newScopedHandler(t)
	mk, _ := seedTwoKeys(t, h)
	admin := map[string]string{"X-Test-Admin": "1"}

	// ① 列表：两把都看得到，且带 owner
	w := doReq(h, "GET", "/admin/api/keys", admin)
	var resp struct {
		Keys []dto `json:"keys"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Keys) != 2 {
		t.Fatalf("管理员应看到 2 把（含成员的），得到 %d", len(resp.Keys))
	}
	var sawMember bool
	for _, k := range resp.Keys {
		if k.Owner == "alice" {
			sawMember = true
			if k.Editable {
				t.Fatal("成员的 key 对管理员必须 editable=false（否则界面会给出可点的按钮）")
			}
		}
	}
	if !sawMember {
		t.Fatal("管理员的列表里应能看到成员的 key（审计需要），并带 owner 字段")
	}

	// ② reveal：必须被拒
	w = doReq(h, "GET", "/admin/api/keys/"+mk.ID+"/reveal", admin)
	if w.Code != http.StatusForbidden {
		t.Fatalf("管理员 reveal 成员的 key 应 403，得到 %d（body=%s）", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), mk.Key) {
		t.Fatal("★ 泄漏 ★ 响应体里出现了成员的明文 key")
	}

	// ③ delete：必须被拒，且 key 仍在
	w = doReq(h, "DELETE", "/admin/api/keys/"+mk.ID, admin)
	if w.Code != http.StatusForbidden {
		t.Fatalf("管理员删成员的 key 应 403，得到 %d", w.Code)
	}
	if _, ok := h.store.Get(mk.ID, ""); !ok {
		t.Fatal("★ 越权删除 ★ 成员的 key 被管理员删掉了")
	}

	// ④ 但管理员对自己的 key 一切照旧
	w = doReq(h, "GET", "/admin/api/keys/"+mk.ID+"/reveal", map[string]string{"X-Test-User": "alice"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), mk.Key) {
		t.Fatalf("成员 reveal 自己的 key 应拿到明文，得到 %d", w.Code)
	}
}

func TestAdminCanStillManageOwnKeys(t *testing.T) {
	h := newScopedHandler(t)
	_, ak := seedTwoKeys(t, h)
	admin := map[string]string{"X-Test-Admin": "1"}

	w := doReq(h, "GET", "/admin/api/keys/"+ak.ID+"/reveal", admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), ak.Key) {
		t.Fatalf("管理员 reveal 自己的 key 应 200 且带明文，得到 %d", w.Code)
	}
	w = doReq(h, "DELETE", "/admin/api/keys/"+ak.ID, admin)
	if w.Code != 200 {
		t.Fatalf("管理员删自己的 key 应 200，得到 %d", w.Code)
	}
}
