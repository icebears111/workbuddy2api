package upstream

import (
	"strings"
	"testing"
)

// 非对话模型的过滤契约（2026-09-23）。
//
// 起因：上游 /v3/config 返回 51 个，其中 18 个不是对话模型
// （补全、图像、小参数试验），透出去会让客户端下拉框里出现
// codewise-completions 这类选了也没法聊天的项。
//
// 判据照抄 agent2api 的 is_chat_model（core/models/mod.rs），
// 这三条一起才等价 —— 尤其第三条「缺 maxOutputTokens 也算不合格」，
// 上游对非对话项压根不给这个字段。
func TestIsChatModel(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want bool
		why  string
	}{
		{"正常对话模型", map[string]any{"id": "glm-5.3", "maxOutputTokens": 24000.0}, true, ""},
		{"边界 16000", map[string]any{"id": "x", "maxOutputTokens": 16000.0}, true, "等于阈值应通过"},
		{"输出太小", map[string]any{"id": "hunyuan-chat", "maxOutputTokens": 8192.0}, false,
			"实测 hunyuan-chat 是 8192，agent2api 也按这条排除"},
		{"缺 maxOutputTokens", map[string]any{"id": "x"}, false,
			"缺字段 = 非对话项（不是「无限制」）"},
		{"补全前缀", map[string]any{"id": "completion-gf", "maxOutputTokens": 24000.0}, false, "前缀排除"},
		{"codewise 前缀", map[string]any{"id": "codewise-rewrite", "maxOutputTokens": 24000.0}, false, "前缀排除"},
		{"图像前缀", map[string]any{"id": "hunyuan-image-alpha", "maxOutputTokens": 24000.0}, false, "前缀排除"},
		{"小参数前缀", map[string]any{"id": "hunyuan-3b", "maxOutputTokens": 24000.0}, false, "前缀排除"},
		{"旧版 deepseek", map[string]any{"id": "deepseek-v3-0324", "maxOutputTokens": 24000.0}, false, "前缀排除"},
		{"supportsExtra=true", map[string]any{"id": "x", "maxOutputTokens": 24000.0, "supportsExtra": true}, false,
			"真值 → 排除"},
		{"supportsExtra=false", map[string]any{"id": "x", "maxOutputTokens": 24000.0, "supportsExtra": false}, true,
			"假值不该误伤"},
		{"supportsExtra=非零数", map[string]any{"id": "x", "maxOutputTokens": 24000.0, "supportsExtra": 1.0}, false,
			"JS 真值语义：非零数字算真"},
		{"supportsExtra=空串", map[string]any{"id": "x", "maxOutputTokens": 24000.0, "supportsExtra": ""}, true,
			"JS 真值语义：空串算假"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsChatModel(c.in); got != c.want {
				t.Fatalf("IsChatModel(%v) = %v, want %v（%s）", c.in, got, c.want, c.why)
			}
		})
	}
}

// ★ 回归 ★：老 UA 拿不到模型清单。
//
// 上游按 UA 里的 CLI 版本段决定下发哪一档清单。桥原来用
// "Coding Copilot/1.106.1 CodeBuddy/1.106.1" → **data 里根本没有 models 字段**
// （不是报错，所以藏了很久，看起来像"上游不再提供模型"）。
// 这个测试钉住「必须带 CLI 段」这件事，防止有人把它改回旧串。
func TestUserAgentHasCLISegment(t *testing.T) {
	if !strings.Contains(UserAgent, "CLI/") {
		t.Fatalf("UserAgent 缺少 CLI/<版本> 段，上游会拒发模型清单：%q", UserAgent)
	}
	// 版本段必须非空（"CLI/" 后面得有东西）
	i := strings.Index(UserAgent, "CLI/")
	if i < 0 || strings.TrimSpace(UserAgent[i+len("CLI/"):]) == "" {
		t.Fatalf("CLI/ 后面没有版本号：%q", UserAgent)
	}
}

// 过滤只作用于 /v3/config 这条混合目录，不碰其它形状。
//
// 实测教训：把过滤放进 collect（所有形状共用）时，
// TestParseModelList 的 4 个用例全挂 —— 那些 fixture 没有
// maxOutputTokens 字段，被当成"非对话模型"滤掉了。
func TestFilterOnlyOnV3Config(t *testing.T) {
	openai := []byte(`{"object":"list","data":[{"id":"glm-5.2"},{"id":"auto"}]}`)
	ms := parseModelList(openai)
	if len(ms) != 2 {
		t.Fatalf("OpenAI 形状解析出 %d 个，want 2", len(ms))
	}
	// 该形状不该被对话过滤影响（它本来就是纯对话清单）
	for _, m := range ms {
		if m.ID == "" {
			t.Fatal("解析出了空 id")
		}
	}
}

// filterChatModels 从混合目录里挑出对话模型。
func TestFilterChatModels(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"models":[
		{"id":"glm-5.3","maxOutputTokens":24000},
		{"id":"codewise-rewrite","maxOutputTokens":24000},
		{"id":"completion-gf","maxOutputTokens":24000},
		{"id":"hunyuan-chat","maxOutputTokens":8192},
		{"id":"deepseek-v4.1-flash","maxOutputTokens":24000},
		{"id":"hunyuan-image-alpha","maxOutputTokens":24000}
	]}}`)
	parsed := parseModelList(raw)
	if len(parsed) != 6 {
		t.Fatalf("解析出 %d 个，want 6（parseModelList 是纯形状解析，不该过滤）", len(parsed))
	}
	kept := filterChatModels(raw, parsed)
	got := map[string]bool{}
	for _, m := range kept {
		got[m.ID] = true
	}
	if len(kept) != 2 || !got["glm-5.3"] || !got["deepseek-v4.1-flash"] {
		t.Fatalf("过滤后 = %v, want 只有 glm-5.3 与 deepseek-v4.1-flash", got)
	}
}

// credits 是**字符串** "x0.21"，不是数字 —— 这是本轮踩到的点。
//
// 原来前端认的是 cost_factor（数字），而上游早已改成 credits（带 x 前缀的
// 字符串），于是那个「倍率」列对 buddy 全空。这个测试钉住解析规则。
func TestCreditMultiplier(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"x0.21", 0.21, true},
		{"x1.20", 1.20, true},
		{"x2.00", 2.00, true},
		{"x0.00", 0, true}, // ★ 0 是合法值（hy3 不消耗额度），不能被当成「没有」
		{"0.5", 0.5, true}, // 没有 x 前缀也能解
		{"X0.3", 0.3, true},
		{" x0.4 ", 0.4, true},
		{"", 0, false},
		{"x", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := CreditMultiplier(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("CreditMultiplier(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// modelFromMap 要从上游条目里解析出 credits（含 0.00 的 has 标记）。
func TestModelFromMapParsesCredits(t *testing.T) {
	m := map[string]any{"id": "glm-5.3", "name": "GLM-5.3", "credits": "x0.79"}
	got := modelFromMap(m)
	if got == nil {
		t.Fatal("解析失败")
	}
	if !got.HasCredits || got.Credits != 0.79 {
		t.Fatalf("credits 解析错：has=%v val=%v", got.HasCredits, got.Credits)
	}

	// 0.00 必须也标 has=true —— 否则界面上那个合法的 0 会显示成「—」
	zero := modelFromMap(map[string]any{"id": "hy3", "credits": "x0.00"})
	if !zero.HasCredits || zero.Credits != 0 {
		t.Fatalf("credits=0.00 应标 has=true，得到 has=%v val=%v", zero.HasCredits, zero.Credits)
	}

	// 没有这个字段 → has=false（与「值是 0」区分开）
	none := modelFromMap(map[string]any{"id": "deepseek-v3"})
	if none.HasCredits {
		t.Fatal("上游没给 credits 时 HasCredits 应为 false")
	}
}
