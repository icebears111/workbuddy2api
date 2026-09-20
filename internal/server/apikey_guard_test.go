package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"codebuddy2api/internal/apikey"
)

// 管理面（key 的发放/吊销）必须只认全局 key。
//
// 回归背景：多 key 表刚加进来时，所有路由共用一个守卫，导致
// **拿到调用方 key 的人可以给自己签发新 key**（无限提权），还能删别人的。
func TestAdminKeyRoutesRejectCallerKeys(t *testing.T) {
	store, err := apikey.NewStore(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	callerKey, err := store.Create("caller", "")
	if err != nil {
		t.Fatal(err)
	}

	const globalKey = "global-admin-key"
	handler := requireGlobalKey(globalKey, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name string
		key  string
		want int
	}{
		{"全局 key", globalKey, http.StatusOK},
		{"调用方 key 必须被拒", callerKey.Key, http.StatusUnauthorized},
		{"用户表风格的 usr_ key 也必须被拒", "usr_whatever", http.StatusUnauthorized},
		{"空 key", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/admin/api/keys", nil)
			if c.key != "" {
				r.Header.Set("Authorization", "Bearer "+c.key)
			}
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != c.want {
				t.Fatalf("想要 %d，实际 %d", c.want, w.Code)
			}
		})
	}
}

// 未配置全局 key 时放行（内网自用模式），与其它桥一致。
func TestAdminKeyRoutesOpenWhenNoGlobalKey(t *testing.T) {
	handler := requireGlobalKey("", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("GET", "/admin/api/keys", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("未配置全局 key 时应放行，实际 %d", w.Code)
	}
}
