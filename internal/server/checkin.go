// checkin.go —— 一键签到 HTTP 接口 + 当天签到状态查询（逻辑在 internal/checkin，调度器复用）。
package server

import (
	"net/http"

	"codebuddy2api/internal/checkin"
	"codebuddy2api/internal/cred"
)

// apiCheckin 一键签到：遍历 OAuth 登录账号各签一次，聚合返回，并记录当天状态。
// 多用户：非管理员只签到/返回自己名下的账号（防止替他人触发上游操作）。
func (h *Handler) apiCheckin(w http.ResponseWriter, r *http.Request) {
	items, err := checkin.All(h.cfg.AuthDir, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "加载凭证失败: " + err.Error()})
		return
	}
	owner := ownerOf(r)
	if owner != "" {
		kept := items[:0]
		for _, it := range items {
			if o, exists := h.cfg.Pool.OwnerOf(it.UID); exists && o == owner {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	signed := 0
	for _, it := range items {
		if it.Skipped {
			continue
		}
		// code==10001 说明今天已被签（网关或官方客户端），同样视为已签
		signedToday := it.OK || it.Code == 10001
		if it.OK {
			signed++
		}
		checkin.Record(h.cfg.AuthDir, it.UID, signedToday, it.Credit, it.StreakDays)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "signed": signed, "results": items})
}

// apiCheckinStatus 当天各 OAuth 账号的签到状态。
func (h *Handler) apiCheckinStatus(w http.ResponseWriter, r *http.Request) {
	date, byUID := checkin.Today(h.cfg.AuthDir)
	out := []map[string]any{}
	for _, st := range h.cfg.Pool.ListFor(ownerOf(r)) {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Kind != cred.KindToken {
			continue
		}
		rec, ok := byUID[st.UID]
		out = append(out, map[string]any{
			"uid":        st.UID,
			"nickname":   a.Nickname,
			"signed":     ok && rec.Signed,
			"recorded":   ok,
			"credit":     rec.Credit,
			"streak":     rec.StreakDays,
			"disabled":   st.Disabled,
			"date":       date,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "date": date, "accounts": out})
}
