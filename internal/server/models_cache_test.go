package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"codebuddy2api/internal/cred"
	"codebuddy2api/internal/pool"
	"codebuddy2api/internal/upstream"
)

// modelPathsPerAttempt 一轮 allModels() 里桥会试的模型路径数
// （upstream.ModelsPathV2 = /v2/models，ConfigPathV3 = /v3/config）。
// 假上游全部返回空，所以一轮就是 2 次请求 —— 断言要按这个常数写，
// 而不是写死 1（第一版写死 1，把「路径数」误当成了「重复请求」）。
const modelPathsPerAttempt = 2

// 上游 /v2/models 恒返回空的假上游，用于复现「上游拉不到模型」这一真实状况
// （本网关 2026-09-22 实测就是 `models /v3/config: empty`）。
// 计数器用来断言**请求次数** —— 这正是本次改动的判据。
func emptyModelsUpstream(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt64(hits, 1)
			w.Header().Set("Content-Type", "application/json")
			// 空 data → 触发桥的「拉空」分支
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
}

// ★ 回归 ★ 上游拉空时，兜底结果也必须进缓存。
//
// 修之前：缓存只在「成功拉到非空」时写，拉空时每次请求都重轮 3 个账号，
// 单次 /v1/models 约 500ms（实测），而看板每次刷新都调它 → 整页变慢。
func TestFallbackModelsAreCached(t *testing.T) {
	var hits int64
	up := emptyModelsUpstream(t, &hits)
	defer up.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	h := NewHandler(Config{
		Pool: p, Upstream: upstream.NewWithBase(up.URL),
		APIKey: "sk-global", MaxRotate: 3,
	})

	// 连打 3 次 /v1/models
	for i := 0; i < 3; i++ {
		w := do(t, h, "GET", "/v1/models", "sk-global", "")
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 /v1/models = %d", i+1, w.Code)
		}
		if !containsStr(w.Body.String(), "hy4-preview") && !containsStr(w.Body.String(), "auto") {
			t.Fatalf("第 %d 次没返回兜底模型表：%s", i+1, w.Body.String()[:200])
		}
	}

	// 关键断言：**只第一轮打上游**，后两次走兜底缓存。
	//
	// 一轮 = 桥按顺序试两个模型路径（/v2/models 与 /v3/config），
	// 每个都返回空 → 共 2 次上游请求（实测日志确认）。
	// 修之前 3 次 /v1/models 会变成 6 次（每轮都重试），
	// 且真实环境还要乘上 MaxRotate（3 个账号）→ 单次约 500ms。
	if got := atomic.LoadInt64(&hits); got != modelPathsPerAttempt {
		t.Fatalf("上游被请求 %d 次，want %d（兜底结果没进缓存 → 每次请求都重打上游）",
			got, modelPathsPerAttempt)
	}
}

// 兜底缓存要用**短 TTL** 过期：上游恢复后必须能重试，
// 不能像成功结果那样缓存一小时。
func TestFallbackCacheExpiresQuickly(t *testing.T) {
	var hits int64
	up := emptyModelsUpstream(t, &hits)
	defer up.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	h := NewHandler(Config{
		Pool: p, Upstream: upstream.NewWithBase(up.URL),
		APIKey: "sk-global", MaxRotate: 3,
	})

	_ = do(t, h, "GET", "/v1/models", "sk-global", "")
	if got := atomic.LoadInt64(&hits); got != modelPathsPerAttempt {
		t.Fatalf("首次调用后上游请求数 = %d, want %d", got, modelPathsPerAttempt)
	}

	// 把 fetchedAt 往回拨到超过兜底 TTL → 应重新拉
	h.modelsMu.Lock()
	h.fetchedAt = time.Now().Add(-modelsFallbackTTL - time.Second)
	h.modelsMu.Unlock()

	// TTL 过期后：请求**立即拿到旧值**（不阻塞），刷新在后台跑。
	_ = do(t, h, "GET", "/v1/models", "sk-global", "")
	// 等后台刷新落地（异步，所以轮询而不是立即断言）
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&hits) < 2*modelPathsPerAttempt && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&hits); got != 2*modelPathsPerAttempt {
		t.Fatalf("TTL 过期后上游请求数 = %d, want %d（后台刷新没触发）",
			got, 2*modelPathsPerAttempt)
	}

	// 而未过期时不该再打
	_ = do(t, h, "GET", "/v1/models", "sk-global", "")
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&hits); got != 2*modelPathsPerAttempt {
		t.Fatalf("TTL 内又打了上游（共 %d 次），缓存没生效", got)
	}
}

// 成功结果仍按长 TTL 缓存（别把这次改动改坏了原有行为）。
func TestSuccessCacheStillLongLived(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt64(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2"},{"id":"auto"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	h := NewHandler(Config{
		Pool: p, Upstream: upstream.NewWithBase(srv.URL),
		APIKey: "sk-global", MaxRotate: 3,
	})

	// 假上游对所有 GET 都返回同一份非空清单，所以第一个路径就命中、只打 1 次
	for i := 0; i < 3; i++ {
		_ = do(t, h, "GET", "/v1/models", "sk-global", "")
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("成功结果的上游请求数 = %d, want 1", got)
	}
	// 把时间拨到「超过兜底 TTL 但不到 modelsTTL」→ 成功结果仍不该重拉
	h.modelsMu.Lock()
	h.fetchedAt = time.Now().Add(-modelsFallbackTTL - time.Second)
	h.modelsMu.Unlock()
	_ = do(t, h, "GET", "/v1/models", "sk-global", "")
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("成功结果被按短 TTL 过期了（上游请求数 %d, want 1）", got)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// ★ 核心契约 ★：缓存过期时，请求**不能等**上游。
//
// 2026-09-23 的改动要点：拉清单要串行打上游（实测约 400ms），
// 而 /v1/models 是看板每次刷新都调的接口 —— 同步拉等于每轮刷新白等。
// 所以有旧值时立即返回旧值，刷新丢到后台。
//
// 这个测试用「上游故意睡 1 秒」来放大差异：若实现是同步的，
// 请求耗时就接近 1 秒；不阻塞的话应当在几十毫秒内返回。
func TestStaleModelsReturnImmediately(t *testing.T) {
	var hits int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		// 第一次立刻回（让缓存先有值），之后的刷新卡住 1 秒
		if n > 1 {
			select {
			case <-release:
			case <-time.After(time.Second):
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.3","maxOutputTokens":24000}]}`))
	}))
	defer srv.Close()
	defer close(release)

	p := pool.New("")
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	h := NewHandler(Config{
		Pool: p, Upstream: upstream.NewWithBase(srv.URL),
		APIKey: "sk-global", MaxRotate: 1,
	})

	// 首次：拿到清单
	_ = do(t, h, "GET", "/v1/models", "sk-global", "")
	// 把缓存拨到过期
	h.modelsMu.Lock()
	h.fetchedAt = time.Now().Add(-modelsTTL - time.Second)
	h.modelsMu.Unlock()

	// 过期后请求：必须立刻返回（不能等上游那 1 秒）
	t0 := time.Now()
	w := do(t, h, "GET", "/v1/models", "sk-global", "")
	elapsed := time.Since(t0)
	if w.Code != http.StatusOK {
		t.Fatalf("过期后 /v1/models = %d", w.Code)
	}
	if !containsStr(w.Body.String(), "glm-5.3") {
		t.Fatalf("过期时应返回旧清单，得到 %s", w.Body.String()[:150])
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("过期后请求耗时 %v —— 像是同步等上游了（应当立即返回旧值）", elapsed)
	}
}
