// auth.go — API Key 认证中间件。
package server

import (
	"net/http"
	"strings"

	"codebuddy2api/internal/apikey"
)

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

// requireAPIKey 校验请求携带的 key。
//
// 两级校验（顺序重要）：
//  1. 全局 key（config.api_key）—— 兼容既有部署与 sub2api 账号，
//     空值表示不启用认证（仅限内网自用）。
//  2. 多 key 表（apikey.Store）—— 由看板创建、可按调用方独立吊销。
//
// 全局 key 保留的原因：sub2api 账号里存的就是它，改掉会连带改 sub2api 配置；
// 且存量部署升级后不该突然全站 401。
func requireAPIKey(apiKey string, store *apikey.Store, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 完全未配置任何鉴权（全局空 + 无 key 表）时放行
		if apiKey == "" && store == nil {
			next(w, r)
			return
		}
		got := extractKey(r)

		if apiKey != "" && got == apiKey {
			next(w, r)
			return
		}
		if store != nil {
			if _, ok := store.Verify(got); ok {
				next(w, r)
				return
			}
		}
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
			"missing or invalid API key")
	}
}
