// Package scheduler 定时任务：
//  1) 保活探活（CheckHours 整点）：失效(401/403)或 token 到期的账号自动禁用；
//  2) 每日自动签到（AutoCheckin）：在 [CheckinStartHour, CheckinEndHour) 本地窗口内
//     给每个 OAuth 登录账号分配**不同的随机时刻**逐个签到（错开、不整点齐发）；
//     签到被上游 401/403 拒绝（token 失效）的账号自动禁用。
package scheduler

import (
	"context"
	"log"
	"math/rand"
	"sort"
	"time"

	"codebuddy2api/internal/checkin"
	"codebuddy2api/internal/cred"
	"codebuddy2api/internal/pool"
	"codebuddy2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool              *pool.Pool
	Upstream          *upstream.Client
	AuthDir           string // 凭证目录（自动签到读取）
	CheckHours        []int  // 保活探活整点
	AutoCheckin       bool   // 是否开启每日自动签到
	CheckinStartHour  int    // 自动签到窗口起点小时（本地，含）
	CheckinEndHour    int    // 自动签到窗口终点小时（本地，不含）
	TokenURL          string // OAuth 续期地址
	ClientID          string // OAuth 客户端 id
	CooldownOnFail    time.Duration
}

// Scheduler 调度器。
type Scheduler struct{ cfg Config }

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckHours) == 0 {
		cfg.CheckHours = []int{0, 6, 12, 18}
	}
	if cfg.AutoCheckin {
		if cfg.CheckinStartHour < 0 || cfg.CheckinStartHour >= 24 {
			cfg.CheckinStartHour = 8
		}
		if cfg.CheckinEndHour <= cfg.CheckinStartHour || cfg.CheckinEndHour > 24 {
			cfg.CheckinEndHour = cfg.CheckinStartHour + 1
		}
	}
	if cfg.CooldownOnFail <= 0 {
		cfg.CooldownOnFail = 10 * time.Minute
	}
	return &Scheduler{cfg: cfg}
}

func contains(hs []int, h int) bool {
	for _, x := range hs {
		if x == h {
			return true
		}
	}
	return false
}

// nextFire 返回 now 之后最近的一个整点触发时间；无小时配置时返回零值。
func nextFire(now time.Time, hours []int) time.Time {
	if len(hours) == 0 {
		return time.Time{}
	}
	var next time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	return next
}

// sleepUntil 睡到 t；ctx 取消返回 false。
func sleepUntil(ctx context.Context, t time.Time) bool {
	if !t.After(time.Now()) {
		return true
	}
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Run 主循环：保活任务 + 自动签到（各自独立）。
func (s *Scheduler) Run(ctx context.Context) {
	if s.cfg.AutoCheckin {
		log.Printf("auto checkin enabled: daily %02d:00-%02d:00 window, random & staggered per account",
			s.cfg.CheckinStartHour, s.cfg.CheckinEndHour)
		go s.runCheckinLoop(ctx)
	}
	for {
		next := nextFire(time.Now(), s.cfg.CheckHours)
		if next.IsZero() {
			next = time.Now().Add(24 * time.Hour)
		}
		if !sleepUntil(ctx, next) {
			return
		}
		h := time.Now().Hour()
		if contains(s.cfg.CheckHours, h) {
			s.RunCheckNow()
		}
	}
}

// planItem 一条「某账号在某时刻签到」的计划。
type planItem struct {
	uid string
	at  time.Time
}

// planFor 为窗口 [start,end) 内的每个 OAuth 账号分配一个随机、互不相同（错开）的时刻。
func (s *Scheduler) planFor(start, end time.Time) []planItem {
	var uids []string
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a != nil && a.Kind == cred.KindToken && a.Token != "" {
			uids = append(uids, st.UID)
		}
	}
	if len(uids) == 0 {
		return nil
	}
	minutes := int(end.Sub(start) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	perm := rng.Perm(minutes) // 把窗口内分钟随机打散，保证账号时刻互不相同
	now := time.Now()
	var items []planItem
	for i, uid := range uids {
		at := start.Add(time.Duration(perm[i%minutes]) * time.Minute)
		if at.Before(now) {
			at = now.Add(time.Duration(1+i) * time.Second)
		}
		items = append(items, planItem{uid: uid, at: at})
	}
	sort.Slice(items, func(a, b int) bool { return items[a].at.Before(items[b].at) })
	return items
}

// runCheckinLoop 每日在窗口内随机时刻逐个签到。
func (s *Scheduler) runCheckinLoop(ctx context.Context) {
	for {
		now := time.Now()
		loc := now.Location()
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		start := day.Add(time.Duration(s.cfg.CheckinStartHour) * time.Hour)
		end := day.Add(time.Duration(s.cfg.CheckinEndHour) * time.Hour)
		if !now.Before(end) {
			// 今天窗口已过 → 睡到明天窗口起点
			if !sleepUntil(ctx, start.Add(24*time.Hour)) {
				return
			}
			continue
		}
		plan := s.planFor(start, end)
		if len(plan) == 0 {
			if !sleepUntil(ctx, end) {
				return
			}
			continue
		}
		for _, p := range plan {
			if !sleepUntil(ctx, p.at) {
				return
			}
			s.signOne(p.uid)
		}
		// 全部签完，睡到下一个窗口（明天）再计划
		if !sleepUntil(ctx, start.Add(24*time.Hour)) {
			return
		}
	}
}

// signOne 对单个账号签到；token 被拒（401/403）则自动禁用该账号。
func (s *Scheduler) signOne(uid string) {
	disabled := false
	if st, ok := s.cfg.Pool.Status(uid); ok && st.Disabled {
		disabled = true
	}
	if disabled || s.cfg.AuthDir == "" {
		return
	}
	it, ok := checkin.One(s.cfg.AuthDir, uid)
	if !ok {
		return
	}
	switch {
	case it.AuthDead:
		if st, ok := s.cfg.Pool.Status(uid); ok && !st.Disabled {
			s.cfg.Pool.Disable(uid, "checkin: credential rejected (re-login required)")
		}
		log.Printf("auto checkin %s: token dead -> disabled", uid)
	case it.OK:
		log.Printf("auto checkin %s: +%d credit (streak %d)", uid, it.Credit, it.StreakDays)
	default:
		log.Printf("auto checkin %s: %s", uid, it.Msg)
	}
	// 记录当天签到状态：含「今天已签到」的幂等命中；
	// saas 域的 code==10001 是「签到活动未开启」，不算已签（见 checkin.Item.AlreadySigned）。
	if it.AlreadySigned() {
		checkin.Record(s.cfg.AuthDir, uid, true, it.Credit, it.StreakDays)
	}
	s.cfg.Pool.SaveState()
}

// RunCheckNow 立即对所有非禁用账号做一次保活探活；token 过期/鉴权失效自动禁用。
func (s *Scheduler) RunCheckNow() {
	total, healthy := s.cfg.Pool.Count()
	log.Printf("scheduler: checking %d account(s), %d healthy", total, healthy)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Token == "" {
			continue
		}
		// token 过期：按 ExpiresAt 提前禁用，不等下次请求才暴露
		if a.ExpiresAt > 0 && time.Now().Unix() >= a.ExpiresAt {
			log.Printf("keepalive %s: token expired at %d, disabling", st.UID, a.ExpiresAt)
			s.cfg.Pool.Disable(st.UID, "token expired (re-login required)")
			continue
		}
		token, err := a.EnsureToken(s.cfg.TokenURL, s.cfg.ClientID)
		if err != nil {
			log.Printf("keepalive %s: ensure token: %v", st.UID, err)
			if cred.IsAuthInvalid(err) {
				s.cfg.Pool.Disable(st.UID, "auth invalid (re-login required)")
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = s.cfg.Upstream.Probe(ctx, token)
		cancel()
		if err == nil {
			s.cfg.Pool.NoteSuccess(st.UID)
			continue
		}
		log.Printf("keepalive %s: probe failed: %v", st.UID, err)
		switch {
		case upstream.IsKind(err, upstream.ErrSessionDead):
			s.cfg.Pool.Disable(st.UID, "credential rejected")
		case upstream.IsKind(err, upstream.ErrHardCredit):
			s.cfg.Pool.Cooldown(st.UID, pool.CoolHard, s.cfg.CooldownOnFail, "quota exhausted")
		default:
			s.cfg.Pool.NoteError(st.UID, 3, s.cfg.CooldownOnFail)
		}
	}
	s.cfg.Pool.SaveState()
}
