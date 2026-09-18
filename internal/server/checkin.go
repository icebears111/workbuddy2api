// checkin.go —— 一键签到 HTTP 接口 + 当天签到状态查询（逻辑在 internal/checkin，调度器复用）。
package server

import (
	"net/http"

	"codebuddy2api/internal/checkin"
	"codebuddy2api/internal/cred"
)

// apiCheckin 一键签到：遍历 auths 下所有 OAuth 登录账号各签一次，聚合返回，并记录当天状态。
func (h *Handler) apiCheckin(w http.ResponseWriter, r *http.Request) {
	items, err := checkin.All(h.cfg.AuthDir, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "加载凭证失败: " + err.Error()})
		return
	}
	signed := 0
	for _, it := range items {
		if it.Skipped {
			continue
		}
		// 已签：本次签成功，或上游表示「今天已签到」。
		// 注意不能只看 code==10001——saas 域的 10001 是「签到活动未开启或已过期」，
		// 详见 checkin.Item.AlreadySigned。
		signedToday := it.AlreadySigned()
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
	for _, st := range h.cfg.Pool.List() {
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
