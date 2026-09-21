// Package usagestat 按时间桶累计 token 用量与缓存命中（看板「缓存命中」用）。
//
// ── 为什么需要这个包 ────────────────────────────────────────
// 上游**一直**在上报缓存字段（2026-09-21 实测三家都给了，见 CacheRead 的
// 字段清单），但三个桥此前只把 prompt/completion/total 记下来，
// 缓存那部分用完就丢。要做「近 10 分钟 / 1 小时 / 24 小时 / 7 天」
// 四个窗口的命中率，就得把这两个数字**按时间留下来**。
//
// ── 为什么是时间桶，不是逐条明细 ─────────────────────────────
// 逐条明细要覆盖 7 天就得留几万条（本网关繁忙时一天上千请求），
// 而命中率只是个**比值**——分子分母都可加，所以按桶累加即可，
// 存储量与流量无关：
//
//	· 分钟桶 200 个（约 3.3 小时）→ 服务 10 分钟 / 1 小时的窗口
//	· 小时桶 200 个（8 天多）      → 服务 24 小时 / 7 天窗口与 24 小时趋势
//
// 400 个桶，每个两个整数。整天满流量也不会超过几十 KB。
//
// ── 与 internal/usage（buddy 的积分账本）的分工 ──────────────
// 那个管**钱**（credit / 额度），这个管**token**（用量 / 缓存）。
// 两者口径独立：钱是权威账本，token 是观测值，混在一起以后改不动。
//
// ── 三个桥共用同一份实现 ────────────────────────────────────
// 与 internal/modelstate 同一取舍：桥之间不共享代码，但实现要逐字节
// 一致，改一处要同步另两处。
package usagestat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 桶的时间格式：分钟到分、小时到点。用**本地时区**——
// 界面上「近 24 小时」是按用户时钟理解的，用 UTC 会在跨时区时错位。
const (
	minuteFmt = "200601021504"
	hourFmt   = "2006010215"
)

const (
	keepMinutes = 200 // 约 3.3 小时
	keepHours   = 200 // 8 天多（7 天窗口 + 余量）
)

// bucket 一个时间桶的累计值。字段名取短的：这个文件每分钟都在写。
type bucket struct {
	Prompt int64 `json:"p"` // prompt_tokens 合计（命中率的**分母**）
	Hit    int64 `json:"h"` // cache_read_tokens 合计（**分子**）
}

type persisted struct {
	Version int               `json:"version"`
	Minutes map[string]bucket `json:"minutes,omitempty"`
	Hours   map[string]bucket `json:"hours,omitempty"`
}

// Store 线程安全的按桶累计器。
type Store struct {
	mu   sync.RWMutex
	path string
	data persisted
	// dirty 自上次落盘以来是否变过 —— 用于节流写盘（见 save）
	dirty bool
	// lastSave 上次落盘时间
	lastSave time.Time
}

// saveEvery 写盘节流间隔。
//
// 为什么不每次 Add 都写：用量是**观测值**，丢几秒数据不影响结论，
// 而每个请求都落盘在繁忙时是纯浪费（buddy 的积分账本每次写是因为
// 那是钱，口径不同）。进程退出时由 Flush 补一次。
const saveEvery = 20 * time.Second

// New 创建/加载。path 为空时仅内存态（测试用）。
//
// **不返回 error**：用量统计坏掉不该让网关起不来（与 modelstate 同款取舍）。
func New(path string) *Store {
	s := &Store{path: path, data: persisted{Version: 1}}
	if path == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s // 不存在 = 全新的表
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		// 坏文件留档后从空表开始，别让它每次启动都报同一个错
		_ = os.Rename(path, path+".broken")
		return s
	}
	if p.Minutes == nil {
		p.Minutes = map[string]bucket{}
	}
	if p.Hours == nil {
		p.Hours = map[string]bucket{}
	}
	p.Version = 1
	s.data = p
	return s
}

// Add 记一次请求。prompt 与 cacheRead 都 <= 0 时不记
// （全零的探针请求不值得进统计）。
func (s *Store) Add(at time.Time, prompt, cacheRead int64) {
	if prompt <= 0 && cacheRead <= 0 {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	// 负值夹成 0：上游字段异常时不该把统计带偏
	if prompt < 0 {
		prompt = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}

	s.mu.Lock()
	if s.data.Minutes == nil {
		s.data.Minutes = map[string]bucket{}
	}
	mk := at.Format(minuteFmt)
	mb := s.data.Minutes[mk]
	mb.Prompt += prompt
	mb.Hit += cacheRead
	s.data.Minutes[mk] = mb

	if s.data.Hours == nil {
		s.data.Hours = map[string]bucket{}
	}
	hk := at.Format(hourFmt)
	hb := s.data.Hours[hk]
	hb.Prompt += prompt
	hb.Hit += cacheRead
	s.data.Hours[hk] = hb

	s.pruneLocked(at)
	s.dirty = true
	needSave := time.Since(s.lastSave) >= saveEvery
	s.mu.Unlock()

	if needSave {
		s.Flush()
	}
}

// pruneLocked 丢掉过期的桶（调用方需持写锁）。
// 只在超过上限时整理，摊薄排序开销。
func (s *Store) pruneLocked(now time.Time) {
	if len(s.data.Minutes) > keepMinutes {
		s.dropOldest(s.data.Minutes, len(s.data.Minutes)-keepMinutes)
	}
	if len(s.data.Hours) > keepHours {
		s.dropOldest(s.data.Hours, len(s.data.Hours)-keepHours)
	}
}

func (s *Store) dropOldest(m map[string]bucket, n int) {
	if n <= 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 键是「年到分」的定长数字串，字典序即时间序，不必 parse 成 time
	sort.Strings(keys)
	for i := 0; i < n && i < len(keys); i++ {
		delete(m, keys[i])
	}
}

// Flush 把内存状态落盘（进程退出前调一次）。
func (s *Store) Flush() {
	s.mu.Lock()
	if !s.dirty || s.path == "" {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	s.lastSave = time.Now()
	body, err := json.Marshal(s.data)
	s.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	// 临时文件 + rename：直接覆盖时若进程被杀会留下半截 JSON
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	// 必须显式 Chmod：os.WriteFile 的 mode 只在新建时生效
	_ = os.Chmod(tmp, 0o600)
	_ = os.Rename(tmp, s.path)
}

// Window 一个时间窗口的命中率。
type Window struct {
	Hit   int64   `json:"hitTokens"`
	Input int64   `json:"inputTokens"`
	Rate  float64 `json:"rate"`
}

// Point 趋势图上的一个整点。
type Point struct {
	Hour  string  `json:"hour"` // RFC3339（本地时区的整点，前端按它取 %H）
	Hit   int64   `json:"hitTokens"`
	Input int64   `json:"inputTokens"`
	Total int64   `json:"totalTokens"`
	Rate  float64 `json:"rate"`
}

// Rates 四个窗口的命中率。
//
// 窗口边界用「当前时刻往回推」，桶按整分钟/整点归档，
// 所以最外层那个桶可能只覆盖窗口的一部分 —— 对**比值**没有影响
// （分子分母同源），这也是选比值累加而不是分别算率再平均的原因。
func (s *Store) Rates(now time.Time) map[string]Window {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]Window{
		"last10m": s.windowMinutesLocked(now, 10),
		"last1h":  s.windowMinutesLocked(now, 60),
		"last24h": s.windowHoursLocked(now, 24),
		"last7d":  s.windowHoursLocked(now, 24*7),
	}
}

func (s *Store) windowMinutesLocked(now time.Time, minutes int) Window {
	from := now.Add(-time.Duration(minutes) * time.Minute)
	var hit, input int64
	for k, b := range s.data.Minutes {
		t, err := time.ParseInLocation(minuteFmt, k, time.Local)
		if err != nil || t.Before(from) {
			continue
		}
		hit += b.Hit
		input += b.Prompt
	}
	return Window{Hit: hit, Input: input, Rate: safeRate(hit, input)}
}

func (s *Store) windowHoursLocked(now time.Time, hours int) Window {
	from := now.Add(-time.Duration(hours) * time.Hour)
	var hit, input int64
	for k, b := range s.data.Hours {
		t, err := time.ParseInLocation(hourFmt, k, time.Local)
		if err != nil || t.Before(from) {
			continue
		}
		hit += b.Hit
		input += b.Prompt
	}
	return Window{Hit: hit, Input: input, Rate: safeRate(hit, input)}
}

// Trend24h 近 24 个整点的序列（缺的补零，保证前端画得出连续的轴）。
func (s *Store) Trend24h(now time.Time) []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cur := now.Truncate(time.Hour)
	out := make([]Point, 0, 24)
	for i := 23; i >= 0; i-- {
		t := cur.Add(-time.Duration(i) * time.Hour)
		b := s.data.Hours[t.Format(hourFmt)]
		out = append(out, Point{
			Hour:  t.Format(time.RFC3339),
			Hit:   b.Hit,
			Input: b.Prompt,
			Total: b.Prompt + b.Hit, // 见 Daily 的说明：总量 = 输入 + 命中
			Rate:  safeRate(b.Hit, b.Prompt),
		})
	}
	return out
}

// TotalSnapshot 累计总量（跨全部保留桶），用于页面显示「统计了多少」。
func (s *Store) TotalSnapshot() (hit, input int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.data.Minutes {
		hit += b.Hit
		input += b.Prompt
	}
	return
}

// safeRate 命中率。分母 <= 0 或分子 <= 0 时返回 0（没有数据 ≠ 0% 命中，
// 但调用方会用 inputTokens 是否为 0 来区分两种显示）。
//
// 不夹到 1：上游把「缓存读」与「输入」分开上报时，cache_read 可能大于
// prompt_tokens（命中部分不计入输入），此时命中率合法地超过 100%。
// 夹上限等于谎报数据（与 agent2api 的取舍一致）。
func safeRate(hit, input int64) float64 {
	if input <= 0 || hit <= 0 {
		return 0
	}
	rate := float64(hit) / float64(input)
	if rate != rate || rate > 1e9 { // NaN / Inf 防护
		return 0
	}
	return rate
}

// ── 上游字段解析 ──────────────────────────────────────────────

// CacheRead 从上游的 usage 对象里取「缓存读取」的 token 数。
//
// ★ 字段名为什么这么多种 ★
// 2026-09-21 拿三家上游的真实响应逐个看过，同一个语义有这些写法：
//
//	CodeBuddy（copilot.tencent.com）一次响应里**同时**给了七种：
//	  prompt_tokens_details.cached_tokens   （OpenAI 标准）
//	  cache_read_input_tokens               （Anthropic 风格）
//	  cache_read_tokens
//	  cached_tokens                         （顶层）
//	  prompt_cache_hit_tokens               （命中数）
//	  prompt_cache_miss_tokens              （未命中数，不是命中）
//	  prompt_cache_write_tokens / cache_creation_input_tokens（写缓存，另算）
//	Qoder：prompt_tokens_details.cached_tokens
//	CatPaw：contextInfo.usage.cache_read_tokens
//
// 取值顺序按「最标准 → 最方言」，命中即返回。**不累加多个字段** ——
// 它们是同一份数据的多种写法，加起来会翻倍（buddy 一家就给了七个）。
//
// 找不到返回 0，由调用方按「无数据」显示（不是「命中率为 0%」）。
func CacheRead(usage map[string]any) int64 {
	if usage == nil {
		return 0
	}
	// 1. OpenAI 标准：prompt_tokens_details.cached_tokens
	if d, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := numberField(d["cached_tokens"]); ok {
			return v
		}
	}
	// 2. Anthropic 风格 / 通用别名
	for _, k := range []string{
		"cache_read_tokens",
		"cache_read_input_tokens",
		"cached_tokens",           // 顶层（部分实现直接放这）
		"prompt_cache_hit_tokens", // 命中的绝对数
	} {
		if v, ok := numberField(usage[k]); ok {
			return v
		}
	}
	return 0
}

// numberField 宽松读一个非负整数：上游给 int / float / json.Number 都有先例。
func numberField(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case float32:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}
