package modelstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 「默认禁用」要能被用户显式开启（2026-09-23）。
//
// 三档状态的契约：
//
//	都没设   → 用调用方给的默认值
//	显式禁用 → true（不管默认）
//	显式启用 → false（哪怕默认是禁用）
//
// 只有「禁用集合 + 默认值」两档是不够的：用户点开启后会被默认值按回去，
// 看起来就像开关坏了。
func TestResolveWithDefault(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "m.json"))

	// ① 没表态 → 用默认
	if got := s.Resolve("gpt-5.5", true); got != true {
		t.Fatalf("未表态 + 默认禁用 → %v, want true", got)
	}
	if got := s.Resolve("glm-5.3", false); got != false {
		t.Fatalf("未表态 + 默认启用 → %v, want false", got)
	}

	// ② 显式开启一个默认禁用的 → 压过默认
	s.SetDisabled("gpt-5.5", false)
	if got := s.Resolve("gpt-5.5", true); got != false {
		t.Fatalf("显式启用应压过默认禁用，得到 %v", got)
	}

	// ③ 再显式禁用 → 压过「显式启用」
	s.SetDisabled("gpt-5.5", true)
	if got := s.Resolve("gpt-5.5", false); got != true {
		t.Fatalf("显式禁用应压过默认启用，得到 %v", got)
	}

	// ④ 大小写不敏感
	s.SetDisabled("GPT-5.6-Luna", false)
	if got := s.Resolve("gpt-5.6-luna", true); got != false {
		t.Fatalf("大小写应归一，得到 %v", got)
	}
}

// 显式启用要落盘，重启后仍然生效 —— 否则「默认禁用」会每次重启复原，
// 用户得反复去点。
func TestExplicitEnablePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	s := NewStore(path)
	s.SetDisabled("gpt-5.5", false) // 显式开启
	s.SetDisabled("glm-4.6", true)  // 显式禁用

	again := NewStore(path)
	if again.Resolve("gpt-5.5", true) != false {
		t.Fatal("显式启用没落盘：重启后又变回默认禁用")
	}
	if again.Resolve("glm-4.6", false) != true {
		t.Fatal("显式禁用没落盘")
	}
	// 文件里两档都不该同时出现同一个 id
	raw, _ := os.ReadFile(path)
	var shape fileShape
	_ = json.Unmarshal(raw, &shape)
	both := map[string]bool{}
	for _, id := range shape.Disabled {
		both[id] = true
	}
	for _, id := range shape.Enabled {
		if both[id] {
			t.Fatalf("id %q 同时出现在 disabled 与 enabled：状态矛盾", id)
		}
	}
}

// 切换方向时「是否有变化」要判净效果。
//
// 例：某 id 已在 disabled 里，再 SetDisabled(id, true) 是无变化（不该落盘）；
// 但若它同时在 enabled 里（理论上不该发生），也应当被清掉。
func TestSetDisabledReportsRealChange(t *testing.T) {
	s := NewStore("")
	// 首次调用（无论哪个方向）都从「未表态」变成「已表态」→ 有变化
	if !s.SetDisabled("a", true) {
		t.Fatal("首次禁用应报告有变化")
	}
	if s.SetDisabled("a", true) {
		t.Fatal("重复禁用不该报告有变化")
	}
	if !s.SetDisabled("a", false) {
		t.Fatal("从显式禁用到显式启用应报告有变化")
	}
	if s.SetDisabled("a", false) {
		t.Fatal("重复启用不该报告有变化")
	}
	// ★ 关键：对一个**从未表态**的 id 调 SetDisabled(id,false) 也必须有变化 ★
	// 它把状态从「未表态」改成「显式启用」—— 少了这一步，默认禁用的模型
	// 永远开不起来（写入被当成 no-op 跳过）。
	if !s.SetDisabled("b", false) {
		t.Fatal("对未表态的 id 显式启用必须有变化（否则默认禁用改不回来）")
	}
	if s.Resolve("b", true) != false {
		t.Fatal("显式启用后不该再受默认禁用影响")
	}
}

// 旧版本文件（version 1，只有 disabled）必须还能读 —— 升级不能丢用户设置。
func TestLoadsVersion1File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	// 手写一份 v1 格式（没有 enabled 字段）
	if err := os.WriteFile(path, []byte(`{"version":1,"disabled":["legacy-model"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if s.Resolve("legacy-model", false) != true {
		t.Fatal("v1 文件里的禁用项没读进来")
	}
	// 新写入应升级为 v2。
	// 注意用一个**会真的改变状态**的调用：SetDisabled(id,false) 对
	// 一个本来就没被禁用的 id 是 no-op（净效果没变 → 不落盘），
	// 所以这里用 true（新增一条禁用）。
	s.SetDisabled("new-model", true)
	raw, _ := os.ReadFile(path)
	var shape fileShape
	_ = json.Unmarshal(raw, &shape)
	if shape.Version != 2 {
		t.Fatalf("写入后 version = %d, want 2", shape.Version)
	}
}
