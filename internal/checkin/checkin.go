// Package checkin —— CodeBuddy 每日签到（官方接口）。
// 仅 OAuth 登录型（KindToken）账号可签；ck_ 静态 API Key 会被官方拒绝（403），标记 Skipped。
//
//	POST <按 realm 选择签到域名>
//	Authorization: Bearer <登录 token>
//	code==0 → 签到成功（data.credit / data.streak_days）
//	code==10001 → 今天已签到（幂等）
//
// 签到域名必须与凭证的 realm 对应，两者边缘（APISIX）各自只认自己域的 token：
//   - CN 域（copilot.tencent.com 登录的 token）→ www.workbuddy.cn
//   - SaaS 域（www.codebuddy.ai 登录的 token）→ www.codebuddy.ai
//
// 用错域的表现是「边缘 401 + HTML 页面」（不是业务 JSON），见 doOne 里的说明。
//
// 服务器与调度器共用本包，避免两套签到逻辑。
package checkin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codebuddy2api/internal/cred"
)

// Endpoint 官方每日签到接口（CN 域 / copilot realm 凭证）。
const Endpoint = "https://www.workbuddy.cn/billing/meter/daily-checkin"

// saasEndpoint SaaS 域（www.codebuddy.ai 登录所得 OAuth token）的签到接口。
//
// 实测（2026-09）：同一枚 saas token 打两个域的结果完全不同——
//   - POST https://www.codebuddy.ai/v2/billing/meter/get-payment-type → 200
//   - POST https://www.workbuddy.cn/billing/meter/get-user-resource    → 401
//
// 且 www.workbuddy.cn 的边缘对任意路径都先鉴权后路由（不存在路径也回 401），
// 说明它只认 CN 域的 token；saas token 在那里必然被拒。
const saasEndpoint = "https://www.codebuddy.ai/v2/billing/meter/daily-checkin"

// endpointForRealm 按凭证所属域选择签到域名。未知/空 realm 走 CN 域（历史行为）。
func endpointForRealm(realm string) string {
	if realm == "saas" {
		return saasEndpoint
	}
	return Endpoint
}

// Item 单账号签到结果。
type Item struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname,omitempty"`
	Realm      string `json:"realm,omitempty"`
	Kind       string `json:"kind,omitempty"`
	OK         bool   `json:"ok"`
	Code       int    `json:"code"`
	Credit     int64  `json:"credit,omitempty"`
	StreakDays int    `json:"streak_days,omitempty"`
	Skipped    bool   `json:"skipped,omitempty"`
	Msg        string `json:"msg,omitempty"`
	// AuthDead 表示请求被上游以 401/403 拒绝（token 失效）。调度器据此自动禁用账号；不外发 JSON。
	AuthDead bool `json:"-"`
}

// All 对 authDir 下所有 OAuth 登录账号各签一次并聚合。
// skipDisabled 为非 nil 时，被禁用的账号（按 uid）直接跳过（调度器用）。
func All(authDir string, skipDisabled map[string]bool) ([]Item, error) {
	creds, err := cred.LoadDir(authDir)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(creds))
	for _, c := range creds {
		if c == nil {
			continue
		}
		if c.Kind != cred.KindToken || c.Token == "" {
			items = append(items, Item{
				UID: c.UID, Kind: string(c.Kind), Realm: c.Realm,
				Skipped: true, Msg: "仅 OAuth 登录账号可签到（ck_ 静态 API Key 不支持）",
			})
			continue
		}
		if skipDisabled != nil && skipDisabled[c.UID] {
			continue
		}
		it := doOne(c.Token, c.Realm)
		it.UID = c.UID
		it.Nickname = c.Nickname
		it.Realm = c.Realm
		it.Kind = string(c.Kind)
		items = append(items, it)
	}
	return items, nil
}

// One 对单个 OAuth 账号签到。authDir 下找不到或非登录型时 ok=false。
func One(authDir, uid string) (Item, bool) {
	creds, err := cred.LoadDir(authDir)
	if err != nil {
		return Item{}, false
	}
	for _, c := range creds {
		if c == nil || c.UID != uid || c.Kind != cred.KindToken || c.Token == "" {
			continue
		}
		it := doOne(c.Token, c.Realm)
		it.UID = c.UID
		it.Nickname = c.Nickname
		it.Realm = c.Realm
		it.Kind = string(c.Kind)
		return it, true
	}
	return Item{}, false
}

// AlreadySigned 报告上游是否表示「今天已签到」（幂等命中），
// 供调用方决定要不要把当天状态落成「已签」。
//
// 注意：code==10001 在不同域语义不同，**不能只看 code**：
//   - CN 域（www.workbuddy.cn）  ：10001 = 「今天已签到，请明天再来」→ 确属已签
//   - SaaS 域（www.codebuddy.ai）：10001 = 「签到活动未开启或已过期」→ 根本没签
//
// 实测（2026-09）：三个 saas 号全部返回 10001 + 未开启/已过期，
// 旧逻辑据此把「从未签到」的账号记成「已签」，后台角标因此长期失真。
// 这里用否定语义关键词排除掉「活动不可用」的情形。
func (it Item) AlreadySigned() bool {
	if it.OK {
		return true
	}
	if it.Code != 10001 {
		return false
	}
	for _, m := range []string{"未开启", "未开始", "已过期", "已结束"} {
		if strings.Contains(it.Msg, m) {
			return false
		}
	}
	return true
}

// ---------- 当天签到状态（持久化到 <authDir>/../data/checkin_state.json） ----------

// AccountState 单个账号某天的签到记录。
type AccountState struct {
	UID        string `json:"uid"`
	Signed     bool   `json:"signed"`
	Credit     int64  `json:"credit,omitempty"`
	StreakDays int    `json:"streak_days,omitempty"`
}

// State 每日签到记录文件内容。
type State struct {
	Date     string                  `json:"date"` // YYYY-MM-DD（本地）
	Accounts map[string]AccountState `json:"accounts"`
}

var stateMu sync.Mutex

func statePath(authDir string) string {
	return filepath.Join(filepath.Dir(authDir), "data", "checkin_state.json")
}

func loadState(p string) State {
	raw, err := os.ReadFile(p)
	if err != nil {
		return State{}
	}
	var st State
	_ = json.Unmarshal(raw, &st)
	if st.Accounts == nil {
		st.Accounts = map[string]AccountState{}
	}
	return st
}

func saveState(p string, st State) {
	raw, _ := json.MarshalIndent(st, "", "  ")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, raw, 0o644)
}

// Record 记录某账号今天已签到（或确认今天已由别处签到）。
func Record(authDir, uid string, signed bool, credit int64, streak int) {
	if uid == "" {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	today := time.Now().Format("2006-01-02")
	p := statePath(authDir)
	st := loadState(p)
	if st.Date != today {
		st.Date = today
		st.Accounts = map[string]AccountState{}
	}
	acct := st.Accounts[uid]
	if signed {
		acct.Signed = true
		if credit > 0 || !acct.Signed {
			acct.Credit = credit
			acct.StreakDays = streak
		}
	}
	acct.UID = uid
	st.Accounts[uid] = acct
	saveState(p, st)
}

// Today 返回当天日期与该日各账号签到记录。
// 每日 00:00（本地时区）重置：记录日期不是今天时视为“新的一天”，返回空记录，
// 避免把昨天遗留的“已签”误显示成今天已签（等到当天签到后再写入新记录）。
func Today(authDir string) (string, map[string]AccountState) {
	p := statePath(authDir)
	st := loadState(p)
	today := time.Now().Format("2006-01-02")
	if st.Date != today {
		return today, map[string]AccountState{}
	}
	return st.Date, st.Accounts
}

func doOne(token, realm string) Item {
	return doOneAt(endpointForRealm(realm), token)
}

// doOneAt 对指定签到端点执行一次签到（拆出来便于用 httptest 覆盖边缘返回 HTML 的场景）。
func doOneAt(endpoint, token string) Item {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return Item{Msg: "构造请求失败: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CodeBuddy/1.0.8 CLI")

	cl := &http.Client{Timeout: 25 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return Item{Msg: "请求失败: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// 401/403 必须先于 JSON 解析判定为「凭证失效」。
	//
	// 上游边缘（APISIX）在 token 不被接受时返回的是 HTML 错误页而非业务 JSON：
	//
	//	HTTP/1.1 401 Authorization Required
	//	WWW-Authenticate: Bearer realm="copilot", error="invalid_token"
	//	Server: APISIX/3.9.1
	//
	// 旧逻辑先 json.Unmarshal：解析 HTML 失败即提前 return，AuthDead 没被置位，
	// 于是 ① 报错退化成「解析响应失败(...)」这种指错方向的文案；
	// ② 调度器 signOne 拿不到 AuthDead，失效账号不会被自动停用，只会每天重复失败。
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Item{
			AuthDead: true,
			Msg:      "凭证被拒绝(HTTP " + resp.Status + ")，需重新登录",
		}
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit     int64 `json:"credit"`
			StreakDays int   `json:"streak_days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Item{Msg: "解析响应失败(HTTP " + resp.Status + "): " + truncate(string(body), 200)}
	}
	out := Item{Code: payload.Code, Credit: payload.Data.Credit, StreakDays: payload.Data.StreakDays}
	switch {
	case resp.StatusCode == http.StatusOK && payload.Code == 0:
		out.OK = true
	default:
		if payload.Msg != "" {
			out.Msg = payload.Msg
		} else {
			out.Msg = "HTTP " + resp.Status
		}
	}
	return out
}

// truncate 限长，避免把整页 HTML 塞进后台的报错单元格。
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
