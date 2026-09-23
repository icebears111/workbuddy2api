package modelstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSetAndQuery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	s := NewStore(path)

	if s.Count() != 0 {
		t.Fatalf("new store should be empty, got %d", s.Count())
	}
	if s.IsDisabled("glm-5.2") {
		t.Fatal("nothing should be disabled yet")
	}

	if !s.SetDisabled("glm-5.2", true) {
		t.Fatal("first disable should report a change")
	}
	if !s.IsDisabled("glm-5.2") {
		t.Fatal("glm-5.2 should be disabled")
	}
	// 重复写同一状态不该报「有变化」（页面会据此判断要不要重绘）
	if s.SetDisabled("glm-5.2", true) {
		t.Fatal("redundant disable should report no change")
	}
	if s.Count() != 1 {
		t.Fatalf("want 1 disabled, got %d", s.Count())
	}

	if !s.SetDisabled("glm-5.2", false) {
		t.Fatal("re-enable should report a change")
	}
	if s.IsDisabled("glm-5.2") {
		t.Fatal("glm-5.2 should be enabled again")
	}
	if s.Count() != 0 {
		t.Fatalf("want 0 disabled, got %d", s.Count())
	}
}

// 模型 id 的大小写在各家上游不一致，客户端可能发 DeepSeek-V4.1-Flash。
func TestCaseInsensitive(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "models.json"))
	s.SetDisabled("DeepSeek-V4.1-Flash", true)

	for _, probe := range []string{
		"deepseek-v4.1-flash", "DEEPSEEK-V4.1-FLASH", "  deepseek-v4.1-flash  ",
	} {
		if !s.IsDisabled(probe) {
			t.Errorf("%q should be disabled (case/space insensitive)", probe)
		}
	}
	if s.IsDisabled("deepseek-v4.1-pro") {
		t.Error("a different model must not be affected")
	}
}

// 空 id 不该被写进表里 —— 否则会有一条谁也对不上的禁用记录。
func TestEmptyIDIgnored(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "models.json"))
	if s.SetDisabled("", true) {
		t.Fatal("empty id should not change anything")
	}
	if s.SetDisabled("   ", true) {
		t.Fatal("whitespace-only id should not change anything")
	}
	if s.Count() != 0 {
		t.Fatalf("want 0 disabled, got %d", s.Count())
	}
}

// 落盘 + 重新加载：改动要能跨进程存活。
func TestPersistAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	s := NewStore(path)
	s.SetDisabled("kimi-k3", true)
	s.SetDisabled("glm-5.2", true)
	s.SetDisabled("auto", true)
	s.SetDisabled("glm-5.2", false) // 再启用一个

	again := NewStore(path)
	if !again.IsDisabled("kimi-k3") || !again.IsDisabled("auto") {
		t.Fatal("disabled models should survive reload")
	}
	if again.IsDisabled("glm-5.2") {
		t.Fatal("re-enabled model should stay enabled after reload")
	}
	if again.Count() != 2 {
		t.Fatalf("want 2 disabled after reload, got %d", again.Count())
	}

	// 落盘格式可读（便于运维直接看/手改）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var shape fileShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("persisted file must be valid json: %v", err)
	}
	// Version 2 = 加了 enabled 一档（2026-09-23：默认禁用的模型要能被显式开启）
	if shape.Version != 2 {
		t.Errorf("want version 2, got %d", shape.Version)
	}
}

// 坏文件不能让网关起不来 —— 这是刻意的容错取舍（见包注释）。
func TestBrokenFileIsTolerated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path) // 不该 panic，也不该返回 error
	if s.Count() != 0 {
		t.Fatalf("broken file should be treated as empty, got %d", s.Count())
	}
	// 原文件留档，便于排查
	if _, err := os.Stat(path + ".broken"); err != nil {
		t.Errorf("broken file should be archived, stat err: %v", err)
	}
	// 坏文件之后仍能正常工作
	s.SetDisabled("x", true)
	if !NewStore(path).IsDisabled("x") {
		t.Fatal("store should work normally after recovering from a broken file")
	}
}

// nil 之外的空路径 = 仅内存态（测试/未配置时用）。
func TestMemoryOnlyWhenNoPath(t *testing.T) {
	s := NewStore("")
	s.SetDisabled("auto", true)
	if !s.IsDisabled("auto") {
		t.Fatal("memory-only store should still work")
	}
}

func TestListIsSortedAndLowercased(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "models.json"))
	s.SetDisabled("Zebra", true)
	s.SetDisabled("alpha", true)
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0] != "alpha" || got[1] != "zebra" {
		t.Fatalf("want sorted lowercase [alpha zebra], got %v", got)
	}
}
