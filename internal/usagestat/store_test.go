package usagestat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAddAndRates(t *testing.T) {
	s := New("")
	now := time.Now()

	s.Add(now, 1000, 800) // 80% 命中
	s.Add(now, 1000, 400)

	r := s.Rates(now)
	w := r["last10m"]
	if w.Hit != 1200 || w.Input != 2000 {
		t.Fatalf("last10m hit=%d input=%d, want 1200/2000", w.Hit, w.Input)
	}
	if got := w.Rate; got < 0.59 || got > 0.61 {
		t.Fatalf("rate=%v, want ~0.6", got)
	}
}

// 全零的请求不该进统计（探针类请求）。
func TestZeroIgnored(t *testing.T) {
	s := New("")
	s.Add(time.Now(), 0, 0)
	hit, input := s.TotalSnapshot()
	if hit != 0 || input != 0 {
		t.Fatalf("zero usage should not be recorded, got hit=%d input=%d", hit, input)
	}
}

// 负值夹成 0，不能把统计带偏。
func TestNegativeClamped(t *testing.T) {
	s := New("")
	now := time.Now()
	s.Add(now, -100, -50)
	if _, input := s.TotalSnapshot(); input != 0 {
		t.Fatalf("negative prompt should clamp to 0, got %d", input)
	}
}

// 窗口边界：10 分钟窗口不该收进 20 分钟前的数据。
func TestWindowExcludesOld(t *testing.T) {
	s := New("")
	now := time.Now()
	s.Add(now.Add(-20*time.Minute), 1000, 1000) // 窗口外
	s.Add(now.Add(-2*time.Minute), 1000, 100)   // 窗口内

	if w := s.Rates(now)["last10m"]; w.Input != 1000 || w.Hit != 100 {
		t.Fatalf("last10m should only include the recent bucket, got %+v", w)
	}
	// 但 24 小时窗口要能看见它（走小时桶）
	if w := s.Rates(now)["last24h"]; w.Input != 2000 {
		t.Fatalf("last24h should include both, got %+v", w)
	}
}

// 7 天窗口用小时桶，24 小时内的数据必须都在。
func TestLast7dIncludesHours(t *testing.T) {
	s := New("")
	now := time.Now()
	s.Add(now.Add(-3*24*time.Hour), 500, 250)
	s.Add(now.Add(-1*time.Hour), 500, 250)

	w := s.Rates(now)["last7d"]
	if w.Input != 1000 || w.Hit != 500 {
		t.Fatalf("last7d got %+v, want 1000/500", w)
	}
}

// 没有数据时 rate 是 0 但 Input 也是 0 —— 调用方据此区分
// 「没有请求」与「命中率为 0」。
func TestEmptyWindowIsDistinguishable(t *testing.T) {
	s := New("")
	w := s.Rates(time.Now())["last10m"]
	if w.Input != 0 || w.Hit != 0 {
		t.Fatalf("empty window should be all zero, got %+v", w)
	}
	if w.Rate != 0 {
		t.Fatalf("empty rate should be 0, got %v", w.Rate)
	}
}

// ★ 关键：buddy 一次响应同时给七种写法，只取一个，不能累加。
func TestCacheReadDoesNotSumAliases(t *testing.T) {
	// 真实抓包（简化）：同一个数字出现在多个字段里
	usage := map[string]any{
		"prompt_tokens": float64(41),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(22272),
		},
		"cache_read_input_tokens":   float64(22272),
		"cache_read_tokens":         float64(22272),
		"cached_tokens":             float64(22272),
		"prompt_cache_hit_tokens":   float64(22272),
		"prompt_cache_miss_tokens":  float64(18),
		"prompt_cache_write_tokens": float64(0),
	}
	got := CacheRead(usage)
	if got != 22272 {
		t.Fatalf("CacheRead=%d, want 22272 (must not sum aliases)", got)
	}
}

func TestCacheReadFieldFallbacks(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		want  int64
	}{
		{"openai standard", map[string]any{
			"prompt_tokens_details": map[string]any{"cached_tokens": float64(100)}},
			100},
		{"anthropic style", map[string]any{"cache_read_tokens": float64(200)}, 200},
		{"anthropic input", map[string]any{"cache_read_input_tokens": float64(300)}, 300},
		{"top-level cached", map[string]any{"cached_tokens": float64(400)}, 400},
		{"tencent hit tokens", map[string]any{"prompt_cache_hit_tokens": float64(500)}, 500},
		{"none", map[string]any{"prompt_tokens": float64(10)}, 0},
		{"nil map", nil, 0},
		{"string value ignored", map[string]any{"cache_read_tokens": "abc"}, 0},
		{"float value", map[string]any{"cache_read_tokens": 12.9}, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CacheRead(tc.usage); got != tc.want {
				t.Fatalf("CacheRead=%d, want %d", got, tc.want)
			}
		})
	}
}

// 上游把缓存读与输入分开上报时，命中率可以合法超过 100% —— 不夹上限。
func TestRateCanExceedOne(t *testing.T) {
	s := New("")
	now := time.Now()
	s.Add(now, 10, 100) // 命中 100 / 输入 10
	if w := s.Rates(now)["last10m"]; w.Rate <= 1 {
		t.Fatalf("rate=%v should be allowed to exceed 1 (cache_read counted separately)", w.Rate)
	}
}

// 落盘 + 重载。
func TestPersistAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	s := New(path)
	now := time.Now()
	s.Add(now, 1000, 250)
	s.Flush()

	again := New(path)
	w := again.Rates(now)["last10m"]
	if w.Input != 1000 || w.Hit != 250 {
		t.Fatalf("after reload got %+v, want 1000/250", w)
	}
}

// 坏文件容错：不能因为一个坏 JSON 让网关起不来。
func TestBrokenFileTolerated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-stats.json")
	_ = os.WriteFile(path, []byte("{ not json"), 0o600)

	s := New(path) // 不该 panic
	if _, input := s.TotalSnapshot(); input != 0 {
		t.Fatalf("broken file should be treated as empty, got %d", input)
	}
	if _, err := os.Stat(path + ".broken"); err != nil {
		t.Errorf("broken file should be archived: %v", err)
	}
	// 之后仍能正常工作
	s.Add(time.Now(), 5, 5)
	s.Flush()
	if again := New(path); func() bool { _, i := again.TotalSnapshot(); return i == 5 }() == false {
		t.Fatal("store should work after recovering from broken file")
	}
}

// 趋势是连续 24 个整点，缺的补零。
func TestTrend24hShape(t *testing.T) {
	s := New("")
	now := time.Now()
	s.Add(now, 100, 60)

	pts := s.Trend24h(now)
	if len(pts) != 24 {
		t.Fatalf("want 24 points, got %d", len(pts))
	}
	last := pts[len(pts)-1]
	if last.Hit != 60 || last.Input != 100 {
		t.Fatalf("last point got %+v, want 100/60", last)
	}
	if last.Total != 160 {
		t.Fatalf("totalTokens=%d, want prompt+hit=160", last.Total)
	}
	// 首点（23 小时前）应为零
	if pts[0].Input != 0 {
		t.Fatalf("oldest point should be zero, got %+v", pts[0])
	}
	// 时间必须连续递增（前端按它画轴）
	for i := 1; i < len(pts); i++ {
		a, _ := time.Parse(time.RFC3339, pts[i-1].Hour)
		b, _ := time.Parse(time.RFC3339, pts[i].Hour)
		if !b.After(a) {
			t.Fatalf("points must be increasing: %s then %s", pts[i-1].Hour, pts[i].Hour)
		}
	}
}

// 桶数上限：超过就丢掉最旧的，长期跑不会无限涨。
func TestPruneKeepsBound(t *testing.T) {
	s := New("")
	base := time.Now()
	// 灌 400 个不同的分钟
	for i := 0; i < 400; i++ {
		s.Add(base.Add(-time.Duration(i)*time.Minute), 10, 5)
	}
	s.mu.RLock()
	n := len(s.data.Minutes)
	s.mu.RUnlock()
	if n > keepMinutes {
		t.Fatalf("minutes buckets=%d, should be capped at %d", n, keepMinutes)
	}
}

// 节流写盘：连续 Add 不该每次都落盘，但 Flush 必须能补上。
func TestFlushWritesAfterThrottle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	s := New(path)
	now := time.Now()
	s.Add(now, 1, 1)
	// 立刻再 Add（离 saveEvery 还很远）—— 内存里有、盘上可能还没有
	s.Add(now, 1, 1)
	s.Flush() // 显式落盘
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Flush should have written the file: %v", err)
	}
	again := New(path)
	if _, input := again.TotalSnapshot(); input != 2 {
		t.Fatalf("after Flush+reload got input=%d, want 2", input)
	}
}

// 落盘格式必须可读（运维要能直接看）。
func TestPersistedShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	s := New(path)
	s.Add(time.Now(), 10, 4)
	s.Flush()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("persisted must be valid json: %v", err)
	}
	if p.Version != 1 {
		t.Errorf("version=%d, want 1", p.Version)
	}
	if len(p.Minutes) == 0 || len(p.Hours) == 0 {
		t.Error("both minute and hour buckets should be persisted")
	}
}
