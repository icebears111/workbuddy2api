// Package usage 提供本地额度累计器。
// CodeBuddy 上游没有公开剩余额度 API，但每个对话响应会返回 usage.credit，
// 本包据此累计各账号及全局已用额度，并持久化到 JSON 文件。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codebuddy2api/internal/usagestat"
)

const defaultLimit = 500.0

// maxRecords 消费明细环形缓冲容量：只保留最近这么多次真实请求，
// 再多就丢最旧的（明细是「看最近烧了什么」，不是账本）。
const maxRecords = 200

// 按天账本：Daily 以本地日期（YYYY-MM-DD）为键累计当日消耗。
// 看板「今日使用额度」依赖它——环形缓冲只有 200 条，繁忙时覆盖不了
// 完整一天，只在明细里求和会少算。
const (
	dateFmt      = "2006-01-02"
	dailyKeep    = 62 // 保留最近约两个月的按天数据
	dailyPruneAt = dailyKeep + 15
)

// Tracker 线程安全的额度累计器。
type Tracker struct {
	mu   sync.RWMutex
	path string
	data persisted
}

type persisted struct {
	Total     float64                       `json:"total"`
	Limit     float64                       `json:"limit"`
	Accounts  map[string]float64            `json:"accounts"`
	Daily     map[string]float64            `json:"daily,omitempty"`
	DailyAcct map[string]map[string]float64 `json:"daily_accounts,omitempty"`
	Updated   time.Time                     `json:"updated"`
	Records   []Record                      `json:"records,omitempty"`
}

// Record 一次真实请求的消费明细（供看板「积分消费情况」表展示）。
type Record struct {
	At               time.Time `json:"at"`
	UID              string    `json:"uid,omitempty"`
	Model            string    `json:"model,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	// CacheReadTokens 命中缓存的输入 token（上游的 cache_read_tokens /
	// prompt_tokens_details.cached_tokens 等，见 usagestat.CacheRead）。
	// 缺失按 0 —— 「没上报」与「命中 0」在明细层面不做区分，
	// 命中率的窗口统计另有 usagestat 负责（它可以按窗口忽略空数据）。
	CacheReadTokens int     `json:"cache_read_tokens,omitempty"`
	Credit          float64 `json:"credit"`
	LatencyMS       int64   `json:"latency_ms"`
	Stream          bool    `json:"stream,omitempty"`
}

// Snapshot 只读快照。
type Snapshot struct {
	Total          float64            `json:"total"`
	Limit          float64            `json:"limit"`
	Remaining      float64            `json:"remaining"`
	Accounts       map[string]float64 `json:"accounts"`
	Today          float64            `json:"today"`
	TodayDate      string             `json:"today_date"`
	TodayByAccount map[string]float64 `json:"today_by_account,omitempty"`
	Updated        time.Time          `json:"updated"`
	Records        []Record           `json:"records,omitempty"`
}

// New 创建/加载 Tracker。limit 为 0 时使用默认 500。
func New(path string, limit float64) *Tracker {
	if path == "" {
		path = "usage.json"
	}
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	t := &Tracker{path: path}
	if limit <= 0 {
		limit = defaultLimit
	}
	t.data = persisted{Limit: limit, Accounts: map[string]float64{}}
	t.load()
	if t.data.Limit <= 0 {
		t.data.Limit = limit
	}
	if t.data.Accounts == nil {
		t.data.Accounts = map[string]float64{}
	}
	if t.data.Daily == nil {
		t.data.Daily = map[string]float64{}
	}
	if t.data.DailyAcct == nil {
		t.data.DailyAcct = map[string]map[string]float64{}
	}
	// 首次升级到带按天账本的版本时，用明细里的 credit 回填按天数据。
	// 明细只有 200 条（环形），回填值对较早的日期可能偏小，属尽力而为；
	// 之后每日累计由 Add 精确维护。
	if len(t.data.Daily) == 0 && len(t.data.Records) > 0 {
		for _, r := range t.data.Records {
			if r.At.IsZero() || r.Credit <= 0 {
				continue
			}
			d := r.At.Format(dateFmt)
			t.data.Daily[d] += r.Credit
			if r.UID != "" {
				if t.data.DailyAcct[d] == nil {
					t.data.DailyAcct[d] = map[string]float64{}
				}
				t.data.DailyAcct[d][r.UID] += r.Credit
			}
		}
	}
	// 老版本已有 Daily 但没有 DailyAcct 时，用明细回填当日分账号（尽力而为）
	if len(t.data.Daily) > 0 && len(t.data.DailyAcct) == 0 && len(t.data.Records) > 0 {
		for _, r := range t.data.Records {
			if r.At.IsZero() || r.Credit <= 0 || r.UID == "" {
				continue
			}
			d := r.At.Format(dateFmt)
			if _, ok := t.data.Daily[d]; !ok {
				continue
			}
			if t.data.DailyAcct[d] == nil {
				t.data.DailyAcct[d] = map[string]float64{}
			}
			t.data.DailyAcct[d][r.UID] += r.Credit
		}
	}
	return t
}

// Add 增加一次消耗。uid 为空时只累计全局。
func (t *Tracker) Add(uid string, credit float64) {
	if credit <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data.Total += credit
	if uid != "" {
		t.data.Accounts[uid] += credit
	}
	now := time.Now()
	day := now.Format(dateFmt)
	if t.data.Daily == nil {
		t.data.Daily = map[string]float64{}
	}
	t.data.Daily[day] += credit
	if uid != "" {
		if t.data.DailyAcct == nil {
			t.data.DailyAcct = map[string]map[string]float64{}
		}
		if t.data.DailyAcct[day] == nil {
			t.data.DailyAcct[day] = map[string]float64{}
		}
		t.data.DailyAcct[day][uid] += credit
	}
	t.data.Updated = now
	t.pruneDailyLocked(now)
	t.save()
}

// pruneDailyLocked 只保留最近 dailyKeep 天（调用方需持写锁）。
// 不在每次 Add 都排序：仅在键数超过 dailyPruneAt 时清理，摊薄开销。
func (t *Tracker) pruneDailyLocked(now time.Time) {
	if len(t.data.Daily) <= dailyPruneAt {
		return
	}
	cutoff := now.AddDate(0, 0, -dailyKeep)
	for k := range t.data.Daily {
		if d, err := time.ParseInLocation(dateFmt, k, time.Local); err == nil && d.Before(cutoff) {
			delete(t.data.Daily, k)
			delete(t.data.DailyAcct, k)
		}
	}
}

// Record 追加一条消费明细（不累计金额——金额仍由 Add 负责，
// 两条路径分开，避免改明细时动到账本口径）。
func (t *Tracker) Record(rec Record) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	t.data.Records = append(t.data.Records, rec)
	if len(t.data.Records) > maxRecords {
		// 环形裁剪：保留最近 maxRecords 条
		t.data.Records = append([]Record(nil), t.data.Records[len(t.data.Records)-maxRecords:]...)
	}
	t.save()
}

// Records 返回消费明细副本，最新在前。
func (t *Tracker) Records() []Record {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := len(t.data.Records)
	out := make([]Record, 0, n)
	for i := n - 1; i >= 0; i-- { // 倒序：最新在前
		out = append(out, t.data.Records[i])
	}
	return out
}

// Snapshot 返回当前快照。Today 为本地日期当天已累计的消耗
// （按天账本与明细互补：账本是权威值，与明细求和应一致或更大）。
func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	accs := make(map[string]float64, len(t.data.Accounts))
	for k, v := range t.data.Accounts {
		accs[k] = v
	}
	// 明细倒序内联构造，不复用 Records()——那会二次取 RLock，
	// 存在写者等待时读锁重入的死锁风险（Go RWMutex 不可递归读）。
	n := len(t.data.Records)
	recs := make([]Record, 0, n)
	for i := n - 1; i >= 0; i-- {
		recs = append(recs, t.data.Records[i])
	}
	today := time.Now().Format(dateFmt)
	todayAcc := make(map[string]float64, len(t.data.DailyAcct[today]))
	for k, v := range t.data.DailyAcct[today] {
		todayAcc[k] = v
	}
	return Snapshot{
		Total:          t.data.Total,
		Limit:          t.data.Limit,
		Remaining:      t.data.Limit - t.data.Total,
		Accounts:       accs,
		Today:          t.data.Daily[today],
		TodayDate:      today,
		TodayByAccount: todayAcc,
		Updated:        t.data.Updated,
		Records:        recs,
	}
}

// SetLimit 重新设置额度上限。
func (t *Tracker) SetLimit(limit float64) {
	if limit <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data.Limit = limit
	t.data.Updated = time.Now()
	t.save()
}

// ExtractCredit 从上游 usage 对象里提取 credit（额度消耗）。
func ExtractCredit(usage map[string]any) float64 {
	if usage == nil {
		return 0
	}
	switch v := usage["credit"].(type) {
	case float64:
		return v
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return 0
}

// TokenUsage 从上游 usage 对象里提取 token 统计。
// 上游同一响应里可能同时给 prompt_tokens/total_tokens 与
// cache_* / completion_thinking_tokens 等扩展字段；这里只取三项通用值，
// total 缺失时用 prompt+completion 兜底。
// CacheReadTokens 从上游 usage 里取缓存命中数（字段名兼容见 usagestat）。
// 单独一个函数而不是改 TokenUsage 的签名：那个函数有多个调用点，
// 改签名要一路改过去，而缓存是**附加**信息，缺了不该影响既有逻辑。
func CacheReadTokens(usage map[string]any) int {
	return int(usagestat.CacheRead(usage))
}

func TokenUsage(usage map[string]any) (prompt, completion, total int) {
	if usage == nil {
		return 0, 0, 0
	}
	prompt = intField(usage, "prompt_tokens")
	completion = intField(usage, "completion_tokens")
	total = intField(usage, "total_tokens")
	if total == 0 && (prompt > 0 || completion > 0) {
		total = prompt + completion
	}
	return
}

// intField 宽松读取整数字段（上游可能给 float64 / json.Number / 字符串）。
func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func (t *Tracker) load() {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &t.data)
}

func (t *Tracker) save() {
	_ = os.MkdirAll(filepath.Dir(t.path), 0o755)
	b, _ := json.MarshalIndent(t.data, "", "  ")
	_ = os.WriteFile(t.path, b, 0o644)
}

// FilterByAccounts 按账号子集过滤快照与明细（多用户视图）。
// keep 为允许的账号 uid 集合；返回的快照只含这些账号的汇总/账本/明细。
// 口径：Total/Today 重算为该子集的求和（不是全局值），避免泄露他人消耗规模。
func (t *Tracker) FilterByAccounts(keep map[string]bool) Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	accs := make(map[string]float64)
	var total float64
	for uid, v := range t.data.Accounts {
		if keep[uid] {
			accs[uid] = v
			total += v
		}
	}
	today := time.Now().Format(dateFmt)
	var todaySum float64
	todayAcc := make(map[string]float64)
	for uid, v := range t.data.DailyAcct[today] {
		if keep[uid] {
			todayAcc[uid] = v
			todaySum += v
		}
	}
	recs := make([]Record, 0, len(t.data.Records))
	for i := len(t.data.Records) - 1; i >= 0; i-- { // 倒序：最新在前
		if keep[t.data.Records[i].UID] {
			recs = append(recs, t.data.Records[i])
		}
	}
	return Snapshot{
		Total:          total,
		Limit:          t.data.Limit,
		Remaining:      t.data.Limit - total,
		Accounts:       accs,
		Today:          todaySum,
		TodayDate:      today,
		TodayByAccount: todayAcc,
		Updated:        t.data.Updated,
		Records:        recs,
	}
}
