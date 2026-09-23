package modelstate

import (
	"path/filepath"
	"testing"
)

// 端到端：默认禁用 → 用户开启 → 重启后仍是开启（不被默认值按回去）。
func TestDefaultDisabledCanBeOverriddenAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	s := NewStore(path)

	// 初始：gpt-5.5 靠默认禁用（存储里没表态）
	if s.Resolve("gpt-5.5", true) != true {
		t.Fatal("初始应默认禁用")
	}
	// 用户在模型页点「开启」
	if !s.SetDisabled("gpt-5.5", false) {
		t.Fatal("开启应报告有变化")
	}
	if s.Resolve("gpt-5.5", true) != false {
		t.Fatal("开启后不该再被默认值关掉")
	}
	// 桥刷新清单（默认值仍是 true）→ 仍然是开
	if s.Resolve("gpt-5.5", true) != false {
		t.Fatal("刷新后又被默认关掉了")
	}
	// 重启
	again := NewStore(path)
	if again.Resolve("gpt-5.5", true) != false {
		t.Fatal("重启后又被默认关掉了 —— 用户的选择没落盘")
	}
}
