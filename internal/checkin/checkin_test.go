package checkin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEndpointForRealm 签到域名必须与凭证域对应：
// saas 域走 codebuddy.ai，其余（cn / 空）保持历史行为走 workbuddy.cn。
func TestEndpointForRealm(t *testing.T) {
	cases := map[string]string{
		"saas": saasEndpoint,
		"cn":   Endpoint,
		"":     Endpoint,
	}
	for realm, want := range cases {
		if got := endpointForRealm(realm); got != want {
			t.Errorf("endpointForRealm(%q) = %q, want %q", realm, got, want)
		}
	}
}

// apisix401HTML 是上游边缘拒绝 token 时真实返回的响应体形态（HTML，非业务 JSON）。
const apisix401HTML = `<html>
<head><title>401 Authorization Required</title></head>
<body><center><h1>401 Authorization Required</h1></center>
<hr><center>openresty</center></body>
</html>`

// TestDoOneAtAuthDead 覆盖回归：边缘返回 401/403 + HTML 时，
// 必须标记 AuthDead（调度器据此自动停用账号），而不是退化成「解析响应失败」。
func TestDoOneAtAuthDead(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("WWW-Authenticate", `Bearer realm="copilot", error="invalid_token"`)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(apisix401HTML))
		}))
		it := doOneAt(srv.URL, "dummy")
		srv.Close()

		if !it.AuthDead {
			t.Errorf("status %d: AuthDead = false, want true", status)
		}
		if it.OK {
			t.Errorf("status %d: OK = true, want false", status)
		}
		if !strings.Contains(it.Msg, "重新登录") {
			t.Errorf("status %d: Msg = %q, want 提示重新登录", status, it.Msg)
		}
		if strings.Contains(it.Msg, "解析响应失败") {
			t.Errorf("status %d: Msg 仍为误导性的解析失败文案: %q", status, it.Msg)
		}
	}
}

// TestAlreadySigned 覆盖回归：code==10001 在两个域的语义不同，
// 不能只看 code，否则 saas 域「活动未开启」会被误记成「已签」。
func TestAlreadySigned(t *testing.T) {
	cases := []struct {
		name string
		it   Item
		want bool
	}{
		{"签到成功", Item{OK: true, Code: 0, Credit: 100}, true},
		{"CN 域：今天已签到", Item{Code: 10001, Msg: "今天已签到，请明天再来"}, true},
		{"SaaS 域：签到活动未开启或已过期", Item{Code: 10001, Msg: "签到活动未开启或已过期"}, false},
		{"SaaS 域：签到活动未开始", Item{Code: 10001, Msg: "签到活动未开始"}, false},
		{"业务报错", Item{Code: 500, Msg: "服务繁忙"}, false},
		{"其他 10001 文案按已签处理", Item{Code: 10001, Msg: "今日已领取"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.it.AlreadySigned(); got != c.want {
				t.Errorf("AlreadySigned() = %v, want %v (code=%d msg=%q)", got, c.want, c.it.Code, c.it.Msg)
			}
		})
	}
}

// TestDoOneAtResults 覆盖正常业务响应：成功、今天已签、业务报错。
func TestDoOneAtResults(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantOK     bool
		wantCode   int
		wantCredit int64
	}{
		{
			name:       "签到成功",
			status:     http.StatusOK,
			body:       `{"code":0,"msg":"ok","data":{"credit":100,"streak_days":3}}`,
			wantOK:     true,
			wantCode:   0,
			wantCredit: 100,
		},
		{
			name:     "今天已签到（幂等）",
			status:   http.StatusOK,
			body:     `{"code":10001,"msg":"今天已签到，请明天再来"}`,
			wantOK:   false,
			wantCode: 10001,
		},
		{
			name:     "业务报错",
			status:   http.StatusOK,
			body:     `{"code":500,"msg":"服务繁忙"}`,
			wantOK:   false,
			wantCode: 500,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			it := doOneAt(srv.URL, "dummy")
			if it.OK != c.wantOK {
				t.Errorf("OK = %v, want %v (msg=%q)", it.OK, c.wantOK, it.Msg)
			}
			if it.Code != c.wantCode {
				t.Errorf("Code = %d, want %d", it.Code, c.wantCode)
			}
			if it.Credit != c.wantCredit {
				t.Errorf("Credit = %d, want %d", it.Credit, c.wantCredit)
			}
			if it.AuthDead {
				t.Errorf("AuthDead = true, want false")
			}
		})
	}
}
