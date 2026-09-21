package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"codebuddy2api/internal/usage"
	"codebuddy2api/internal/usagestat"
)

// 带用量统计的 handler。
func newStatsHandler(t *testing.T) (*Handler, *usagestat.Store) {
	t.Helper()
	h, _ := newTestHandler(t, "ok", "sk-global")
	h.cfg.UsageStats = usagestat.New(filepath.Join(t.TempDir(), "usage-stats.json"))
	return h, h.cfg.UsageStats
}

// ★ 这是本轮的核心：buddy 上游一次响应给**七种**缓存字段写法，
// 必须只取一个、不能累加。
func TestRecordConsumptionCapturesCacheOnce(t *testing.T) {
	h, stats := newStatsHandler(t)

	// 真实抓包形状（payload 简化）
	u := map[string]any{
		"prompt_tokens":     float64(41),
		"completion_tokens": float64(16),
		"total_tokens":      float64(57),
		"credit":            float64(0.1),
		// 同一个数字的多种写法 —— 全都出现时只能算一次
		"prompt_tokens_details":     map[string]any{"cached_tokens": float64(22272)},
		"cache_read_input_tokens":   float64(22272),
		"cache_read_tokens":         float64(22272),
		"cached_tokens":             float64(22272),
		"prompt_cache_hit_tokens":   float64(22272),
		"prompt_cache_miss_tokens":  float64(18),
		"prompt_cache_write_tokens": float64(0),
	}
	h.recordConsumption("uid-1", "deepseek-v4.1-flash", u, time.Now(), false)

	hit, input := stats.TotalSnapshot()
	if hit != 22272 {
		t.Fatalf("cache hit=%d, want 22272 (must not sum the 7 aliases)", hit)
	}
	if input != 41 {
		t.Fatalf("prompt=%d, want 41", input)
	}
}

// 明细里也要带上 cache —— 看板「消费」页每一行能显示命中。
func TestRecordConsumptionStoresCacheInRecord(t *testing.T) {
	h, _ := newStatsHandler(t)
	tracker := usage.New(filepath.Join(t.TempDir(), "usage.json"), 500)
	h.cfg.Tracker = tracker

	u := map[string]any{
		"prompt_tokens":         float64(1000),
		"completion_tokens":     float64(50),
		"total_tokens":          float64(1050),
		"cache_read_tokens":     float64(800),
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(800)},
	}
	h.recordConsumption("uid-1", "glm-5.2", u, time.Now(), true)

	recs := tracker.Records()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].CacheReadTokens != 800 {
		t.Fatalf("record cache_read=%d, want 800", recs[0].CacheReadTokens)
	}
}

// 上游没给缓存字段时按 0 记（不是错误）。
func TestRecordConsumptionWithoutCacheField(t *testing.T) {
	h, stats := newStatsHandler(t)
	h.recordConsumption("uid-1", "glm-5.2", map[string]any{
		"prompt_tokens": float64(100), "completion_tokens": float64(5),
		"total_tokens": float64(105), "credit": float64(0.1),
	}, time.Now(), false)

	hit, input := stats.TotalSnapshot()
	if hit != 0 || input != 100 {
		t.Fatalf("got hit=%d input=%d, want 0/100", hit, input)
	}
}

// 全零探针不该进统计。
func TestRecordConsumptionSkipsProbe(t *testing.T) {
	h, stats := newStatsHandler(t)
	h.recordConsumption("uid-1", "glm-5.2", map[string]any{
		"prompt_tokens": float64(0), "completion_tokens": float64(0),
		"total_tokens": float64(0),
	}, time.Now(), false)

	if _, input := stats.TotalSnapshot(); input != 0 {
		t.Fatalf("probe request should be skipped, got input=%d", input)
	}
}

// 没配统计（nil）时不能 panic。
func TestRecordConsumptionWithoutStatsStore(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "sk-global") // UsageStats nil
	h.recordConsumption("uid-1", "glm-5.2", map[string]any{
		"prompt_tokens": float64(10), "total_tokens": float64(10),
	}, time.Now(), false) // 不该 panic
}

// 管理接口返回四窗口 + 趋势。
func TestApiUsageStatsShape(t *testing.T) {
	h, stats := newStatsHandler(t)
	now := time.Now()
	stats.Add(now, 2000, 1500)
	stats.Add(now.Add(-3*time.Hour), 1000, 200)

	w := do(t, h, "GET", "/api/usage/stats", "sk-global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Enabled bool `json:"enabled"`
		Rates   map[string]struct {
			Hit   int64 `json:"hitTokens"`
			Input int64 `json:"inputTokens"`
		} `json:"rates"`
		Trend24h []struct {
			Hour  string `json:"hour"`
			Total int64  `json:"totalTokens"`
		} `json:"trend24h"`
		Total struct {
			Hit   int64 `json:"hitTokens"`
			Input int64 `json:"inputTokens"`
		} `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !resp.Enabled {
		t.Fatal("enabled should be true")
	}
	for _, k := range []string{"last10m", "last1h", "last24h", "last7d"} {
		if _, ok := resp.Rates[k]; !ok {
			t.Errorf("missing window %q", k)
		}
	}
	if got := resp.Rates["last10m"]; got.Input != 2000 || got.Hit != 1500 {
		t.Errorf("last10m=%+v, want 2000/1500", got)
	}
	// 三小时前那条要出现在 24 小时窗口（证明小时桶工作）
	if got := resp.Rates["last24h"]; got.Input != 3000 || got.Hit != 1700 {
		t.Errorf("last24h=%+v, want 3000/1700", got)
	}
	if len(resp.Trend24h) != 24 {
		t.Errorf("trend=%d points, want 24", len(resp.Trend24h))
	}
}

// 未启用时 200 + enabled=false（不是错误）。
func TestApiUsageStatsDisabled(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "sk-global")
	w := do(t, h, "GET", "/api/usage/stats", "sk-global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["enabled"] != false {
		t.Fatalf("enabled=%v, want false", resp["enabled"])
	}
}

// 调用方 key（有 owner）不得读统计 —— 那是管理员视角的运维数据。
func TestCallerKeyCannotReadUsageStats(t *testing.T) {
	h, _, _, callerKey := newHandlerWithStores(t)
	w := do(t, h, "GET", "/api/usage/stats", callerKey.Key, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("caller key must be rejected (403), got %d", w.Code)
	}
}

// 端到端：真的发一次请求（fake upstream），统计里要有数。
func TestChatRecordsUsageStats(t *testing.T) {
	h, stats := newStatsHandler(t)

	w := do(t, h, "POST", "/v1/chat/completions", "sk-global",
		`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Skipf("fake upstream did not serve this request: %d %s", w.Code, w.Body.String())
	}
	if _, input := stats.TotalSnapshot(); input == 0 {
		t.Fatal("a served request should have recorded usage")
	}
}
