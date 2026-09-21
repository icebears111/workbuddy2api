// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
//
// 选号策略：优先「临期优先」——healthy 中带额度且最早到期的账号先选
//（expireAt 越早越优先），避免套餐/额度到期作废；到期信息缺失或
// 并列时退化为「错误数升序 → 最久未使用优先」（LRU），仍然避开故障号、
// 在到期日相同的号间均匀分摊。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"codebuddy2api/internal/cred"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额/配额不足 → 长冷却
	CoolSoft                 // 429 / 404 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

// Status 单账号对外状态（脱敏）。
type Status struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Healthy  bool      `json:"healthy"`
	Cooling  bool      `json:"cooling"`
	Until    time.Time `json:"until,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Disabled bool      `json:"disabled"`
	ErrCount int       `json:"err_count,omitempty"`
	LastUsed time.Time `json:"last_used,omitempty"`
	LastOK   time.Time `json:"last_ok,omitempty"`
	ExpireAt time.Time `json:"expire_at,omitempty"` // 带额度套餐中最早到期时间（临期优先依据）

	// 连接探测结果（内存态，不持久化）。
	Connected bool `json:"connected,omitempty"` // 最近一次测试连接是否成功
	Models    int  `json:"models,omitempty"`    // 探测到的模型数量
	Tested    bool `json:"tested,omitempty"`    // 是否做过连接测试
}

type entry struct {
	c        *cred.Cred
	disabled bool
	reason   string
	until    time.Time
	errCount int
	lastUsed time.Time
	lastOK   time.Time
	expireAt time.Time // 带余额套餐最早到期；zero = 无/未知
	// 连接探测结果（内存态）。
	connected  bool
	modelCount int
	tested     bool
	// seq 每次被选中时递增到池计数器的当前值。
	// 时间戳相同时（Windows 上 time.Now() 分辨率较粗）用它做决胜，保证严格轮转。
	seq uint64
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

type acctState struct {
	Disabled bool      `json:"disabled"`
	Reason   string    `json:"reason,omitempty"`
	Until    time.Time `json:"until,omitempty"`
	ErrCount int       `json:"err_count,omitempty"`
	LastUsed time.Time `json:"last_used,omitempty"`
	LastOK   time.Time `json:"last_ok,omitempty"`
	ExpireAt time.Time `json:"expire_at,omitempty"`
}

type stateFile struct {
	Accounts map[string]acctState `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu          sync.RWMutex
	byUID       map[string]*entry
	savedStates map[string]acctState // 仅状态；必须与 auths 真实凭证合并才有意义
	stateFp     string
	seq         uint64
}

// New 构建；stateFp 非空时加载状态（注意：仅加载状态，不创建账号。
// 账号必须由 Add/SyncToDir 配合 auths 真实凭证加入，避免已删除账号被复活）。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:       map[string]*entry{},
		savedStates: map[string]acctState{},
		stateFp:     stateFp,
	}
	if stateFp != "" {
		p.load()
	}
	return p
}

// applySaved 把已保存的冷却/禁用状态合并到新 entry。
func (p *Pool) applySaved(e *entry, uid string) {
	if s, ok := p.savedStates[uid]; ok {
		e.disabled = s.Disabled
		e.reason = s.Reason
		e.until = s.Until
		e.errCount = s.ErrCount
		e.lastUsed = s.LastUsed
		e.lastOK = s.LastOK
		e.expireAt = s.ExpireAt
	}
}

// Add 加入账号；已存在则保留冷却状态，仅更新凭证。
func (p *Pool) Add(c *cred.Cred) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[c.UID]; ok {
		e.c = c
		return
	}
	e := &entry{c: c}
	p.applySaved(e, c.UID)
	p.byUID[c.UID] = e
}

// SyncToDir 对齐目录扫描结果：新账号加入、消失的剔除（状态保留在 state.json）。
func (p *Pool) SyncToDir(creds []*cred.Cred) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, c := range creds {
		seen[c.UID] = true
		if e, ok := p.byUID[c.UID]; ok {
			e.c = c
		} else {
			e := &entry{c: c}
			p.applySaved(e, c.UID)
			p.byUID[c.UID] = e
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回当前最合适的账号（错误最少 + 最久未用）。
func (p *Pool) Pick() *cred.Cred { return p.PickExcluding(nil) }

// PickExcluding 同上，跳过 tried（轮转重试用）。
func (p *Pool) PickExcluding(tried map[string]bool) *cred.Cred {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || pickPrefer(e, best) {
			best = e
		}
	}
	if best == nil || best.c == nil {
		return nil
	}
	best.lastUsed = now
	p.seq++
	best.seq = p.seq
	return best.c
}

// pickPrefer 临期优先：带到期信息的 healthy 号优先；到期越早越先。
// 同到期（或都无到期）时退化为 better()（错误数升序 → 最久未用）。
func pickPrefer(a, b *entry) bool {
	az, bz := a.expireAt.IsZero(), b.expireAt.IsZero()
	if az != bz {
		return !az // 有到期的优先于无/未知到期
	}
	if !az && !bz && !a.expireAt.Equal(b.expireAt) {
		return a.expireAt.Before(b.expireAt) // 到期更早的先烧
	}
	return better(a, b)
}

// better 排序规则：错误数少的优先；相同则最久未使用的优先；
// 时间戳相同时用 seq 决胜（seq 小 = 更久未被选中）。
func better(a, b *entry) bool {
	if a.errCount != b.errCount {
		return a.errCount < b.errCount
	}
	if !a.lastUsed.Equal(b.lastUsed) {
		return a.lastUsed.Before(b.lastUsed)
	}
	return a.seq < b.seq
}

// Cooldown 冷却账号并重置错误计数。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（凭证失效，需重新登录/换 Key）。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// Enable 手动解禁/解冻（管理接口用）。
func (p *Pool) Enable(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.reason = ""
	e.errCount = 0
	p.saveLocked()
	return true
}

// SetExpireAt 记录该账号带余额套餐的最早到期时间（临期优先选号依据）；
// 传入 zero time 表示「无可用/未知到期」，令其退化为 LRU 档。
func (p *Pool) SetExpireAt(uid string, at time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.expireAt = at
	p.saveLocked()
	return true
}

// Remove 从池中移除账号。
func (p *Pool) Remove(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byUID[uid]; !ok {
		return false
	}
	delete(p.byUID, uid)
	p.saveLocked()
	return true
}

// NoteError 记录一次错误；达阈值自动冷却。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if threshold > 0 && e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功重置错误计数并记录 lastOK。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
		e.lastOK = time.Now()
	}
}

// Status 查询单账号。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回完整凭证（给 scheduler 用）。
func (p *Pool) AuthByUID(uid string) *cred.Cred {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.c
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

// Count 返回账号总数与可用数。
func (p *Pool) Count() (total, healthy int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	total = len(p.byUID)
	for _, e := range p.byUID {
		if e.healthy(now) && e.c != nil {
			healthy++
		}
	}
	return
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	nick, kind := "", ""
	if e.c != nil {
		nick = e.c.Nickname
		kind = string(e.c.Kind)
	}
	cooling := !e.until.IsZero() && now.Before(e.until)
	return Status{
		UID:      uid,
		Nickname: nick,
		Kind:     kind,
		Healthy:  e.healthy(now) && e.c != nil,
		Cooling:  cooling,
		Until:    e.until,
		Reason:   e.reason,
		Disabled: e.disabled,
		ErrCount: e.errCount,
		LastUsed: e.lastUsed,
		LastOK:   e.lastOK,
		ExpireAt: e.expireAt,

		Connected: e.connected,
		Models:    e.modelCount,
		Tested:    e.tested,
	}
}

// MarkProbed 记录一次连接测试的结果（确保 token + 拉取模型列表）。
// ok=true 表示连通，models 为探测到的模型数量；失败则写入 reason。
func (p *Pool) MarkProbed(uid string, ok bool, models int, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok2 := p.byUID[uid]; ok2 {
		e.tested = true
		e.connected = ok
		e.modelCount = models
		if ok {
			e.lastOK = time.Now()
			e.reason = ""
			e.errCount = 0
			e.until = time.Time{}
		} else if reason != "" {
			e.reason = reason
		}
	}
}

// SaveState 显式保存状态。
func (p *Pool) SaveState() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked()
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

// load 仅读取已保存的冷却/禁用状态到 savedStates；不创建账号。
// 账号必须随后由 Add/SyncToDir 配合 auths 真实凭证加入，
// 否则已删除（auths 文件不存在）的账号会在重启后被错误复活。
func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	p.savedStates = sf.Accounts
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: make(map[string]acctState, len(p.byUID))}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = acctState{
			Disabled: e.disabled,
			Reason:   e.reason,
			Until:    e.until,
			ErrCount: e.errCount,
			LastUsed: e.lastUsed,
			LastOK:   e.lastOK,
			ExpireAt: e.expireAt,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
