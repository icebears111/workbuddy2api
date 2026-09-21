// auth.go — API Key 认证中间件 + 身份（owner/admin）解析。
package server

import (
	"context"
	"net/http"
	"strings"

	"codebuddy2api/internal/apikey"
)

// Identity 一次请求的身份（多用户，2026-09-21）。
//
// Owner 语义：
//   - Owner == "" 且 Admin == true  → 管理员：可见/可选全部账号（共享池 + 各用户账号）
//   - Owner != ""                   → 该用户的私有作用域：只可见/可选自己的账号
//   - Owner == "" 且 Admin == false → 未识别（中间件会拒绝，不会进入处理器）
//
// 为什么管理员能看全部：这台网关的主人是站长（admin），用户账号出问题时需要
// 能代为处理；而普通用户之间互相隔离。管理员面板因此保留全部既有能力。
type Identity struct {
	Owner string
	Admin bool
}

type identCtxKey struct{}

// identFrom 取请求身份；ok=false 表示未经中间件（不应发生）。
func identFrom(r *http.Request) (Identity, bool) {
	v, ok := r.Context().Value(identCtxKey{}).(Identity)
	return v, ok
}

// ownerOf 快捷取归属；管理员返回 ""（无主作用域 = 共享池）。
func ownerOf(r *http.Request) string {
	id, _ := identFrom(r)
	if id.Admin {
		return ""
	}
	return id.Owner
}

// isAdminReq 快捷判管理员。
func isAdminReq(r *http.Request) bool {
	id, _ := identFrom(r)
	return id.Admin
}

func withIdent(r *http.Request, id Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identCtxKey{}, id))
}

// extractKey 从 Authorization: Bearer <key> 中提取 key。
// 同时兼容 x-api-key 头（部分客户端习惯用它）。
func extractKey(r *http.Request) string {
	if key := r.Header.Get("X-Api-Key"); key != "" {
		return strings.TrimSpace(key)
	}
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		return strings.TrimSpace(authz[7:])
	}
	return ""
}

// resolveIdentity 决定本次请求的身份。
//
// 顺序（先到先得）：
//  1. 可信身份头（nginx 注入，仅内网可达）：
//     - role=admin → 管理员（可见全部）
//     - 其他已登录角色 → 该用户的私有作用域
//  2. API key：
//     - 全局 key（config.api_key）→ 管理员（兼容既有部署与 sub2api 账号）
//     - key 表命中 → key.Owner；Owner 为空（历史 key）也算管理员级
//  3. 未识别 → 拒绝。
//
// 信任模型与 ops-agent 的 X-Auth-Verified 契约同款：只有 nginx 在鉴权
// location 注入的 X-Auth-User 可信；公开 location 必须显式清空它
// （proxy_set_header X-Auth-User ""），否则可被客户端伪造。
func resolveIdentity(r *http.Request, apiKey string, store *apikey.Store) (Identity, bool) {
	if user := strings.TrimSpace(r.Header.Get("X-Auth-User")); user != "" {
		role := strings.TrimSpace(r.Header.Get("X-Auth-Role"))
		if role == "admin" {
			return Identity{Owner: "", Admin: true}, true
		}
		return Identity{Owner: user, Admin: false}, true
	}
	got := extractKey(r)
	if apiKey != "" && got != "" && got == apiKey {
		return Identity{Owner: "", Admin: true}, true // 全局 key = 管理员
	}
	if store != nil {
		if k, hit := store.Verify(got); hit {
			return Identity{Owner: k.Owner, Admin: k.Owner == ""}, true
		}
	}
	return Identity{}, false
}

// requireAPIKey 校验请求携带的 key（或可信身份头），并把身份写进上下文。
//
// 两级校验（顺序重要）：
//  1. 全局 key（config.api_key）—— 兼容既有部署与 sub2api 账号，
//     空值表示不启用认证（仅限内网自用）。
//  2. 多 key 表（apikey.Store）—— 由看板创建、可按调用方独立吊销。
//
// 另外：带 nginx 注入的可信身份头（X-Auth-User）也放行——用户面板走 SSO、
// 不持有网关 key，需要以本人身份调用网关 API。
func requireAPIKey(apiKey string, store *apikey.Store, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 完全未配置任何鉴权（全局空 + 无 key 表）时放行（管理员作用域）
		if apiKey == "" && store == nil {
			next(w, withIdent(r, Identity{Admin: true}))
			return
		}
		id, ok := resolveIdentity(r, apiKey, store)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
				"missing or invalid API key")
			return
		}
		next(w, withIdent(r, id))
	}
}

// ownAccount 对象级越权校验（多用户核心）。
// 返回 true 表示调用方有权操作该账号：
//   - 管理员（owner==""）：可操作任何账号（含各用户的私有账号，便于运维介入）；
//   - 普通用户：只能操作自己名下的账号；对他人/无主账号一律 false。
//
// 账号不存在同样返回 false（不暴露存在性差异）。
func (h *Handler) ownAccount(r *http.Request, uid string) bool {
	id, ok := identFrom(r)
	if !ok {
		return false
	}
	if id.Admin {
		return true
	}
	owner, exists := h.cfg.Pool.OwnerOf(uid)
	if !exists {
		return false
	}
	return owner == id.Owner
}

// requireAdmin 只放行**管理员身份**——用于看板的管理写接口（如模型启停）。
//
// 与「只认全局 key」的取舍不同，这里判的是**身份是不是管理员**：
//
//	· 全局 key          → resolveIdentity 给 Admin=true，通过
//	· SSO 管理员头      → role=admin 给 Admin=true，通过
//	· 调用方 key（有主）→ Admin=false，拒绝
//
// 为什么不用「全局 key 才通过」：看板对管理员是**不注入 key、透传 SSO 身份**
// 的（见 dashboard-api 的 _relay），只认 key 会把管理员自己也挡在门外。
// 而调用方 key（带 owner）本来就该被挡 —— 那是发给「用模型」的凭证，
// 不该能改网关配置，否则任何一把外传的 key 都能把模型全禁掉（拒绝服务）。
func requireAdmin(apiKey string, store *apikey.Store, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" && store == nil {
			next(w, withIdent(r, Identity{Admin: true})) // 未配置鉴权 = 内网自用
			return
		}
		id, ok := resolveIdentity(r, apiKey, store)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
				"missing or invalid API key")
			return
		}
		if !id.Admin {
			writeOpenAIError(w, http.StatusForbidden, "forbidden",
				"admin privilege required")
			return
		}
		next(w, withIdent(r, id))
	}
}
