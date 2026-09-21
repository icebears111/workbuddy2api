package apikey

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestCreateAndVerify(t *testing.T) {
	s := newTestStore(t)

	k, err := s.Create("测试 key", "给 a 用", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(k.Key) < 20 || k.Key[:3] != "sk-" {
		t.Fatalf("key 格式不对: %q", k.Key)
	}

	got, ok := s.Verify(k.Key)
	if !ok {
		t.Fatal("刚创建的 key 应该校验通过")
	}
	if got.ID != k.ID {
		t.Fatalf("返回了错误的记录: %s != %s", got.ID, k.ID)
	}
}

func TestVerifyRejectsUnknown(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create("a", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "sk-不存在", "not-required", "sk-"} {
		if _, ok := s.Verify(bad); ok {
			t.Errorf("不该通过: %q", bad)
		}
	}
}

func TestDeleteRevokesImmediately(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("临时", "", "")

	if _, ok := s.Verify(k.Key); !ok {
		t.Fatal("删除前应该有效")
	}
	if err := s.Delete(k.ID, ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Verify(k.Key); ok {
		t.Fatal("删除后必须立刻失效")
	}
}

func TestDuplicateNameRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create("重名", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("重名", "", ""); err == nil {
		t.Fatal("同名应该被拒绝")
	}
	if _, err := s.Create("  ", "", ""); err == nil {
		t.Fatal("空名应该被拒绝")
	}
}

// 持久化：重启后 key 仍有效（桥重启不该让所有人的 key 失效）
func TestPersistenceAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")

	s1, _ := NewStore(path)
	k, err := s1.Create("持久", "", "")
	if err != nil {
		t.Fatal(err)
	}

	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("重新加载: %v", err)
	}
	if _, ok := s2.Verify(k.Key); !ok {
		t.Fatal("重启后 key 应该仍然有效")
	}
}

func TestFilePermissionsAre0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 的 os.Chmod 只能切只读位，没有 Unix 权限模型。
		// 这条断言的真实验证在 Linux（部署目标）上做 —— 见包文档。
		t.Skip("Windows 无 Unix 权限模型，跳过")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	s, _ := NewStore(path)
	if _, err := s.Create("x", "", ""); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("权限应为 0600，实际 %o", perm)
	}
}

// 覆盖文件已存在的情况：os.WriteFile 的 mode 对已存在的文件不生效，
// 必须靠显式 Chmod —— 这条测试防的正是那个回归。
func TestPermissionsStay0600OnRerite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 Unix 权限模型，跳过")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	s, _ := NewStore(path)
	if _, err := s.Create("a", "", ""); err != nil {
		t.Fatal(err)
	}
	// 人为放宽权限，模拟"文件已存在且 0644"
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	// 再写一次（走 tmp + rename 路径）
	if _, err := s.Create("b", "", ""); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("重写后权限应回到 0600，实际 %o", perm)
	}
}

func TestUniqueKeysGenerated(t *testing.T) {
	s := newTestStore(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		k, err := s.Create(string(rune('a'+i%26))+string(rune('0'+i/26)), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if seen[k.Key] {
			t.Fatal("生成了重复的 key")
		}
		seen[k.Key] = true
	}
}

func TestUsedCallbackFires(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("cb", "", "")
	var fired string
	s.SetUsedCallback(func(id string) { fired = id })

	s.Verify(k.Key)
	if fired != k.ID {
		t.Fatalf("回调未触发或 id 不对: %q", fired)
	}
	// 未命中的不该触发
	fired = ""
	s.Verify("sk-假的")
	if fired != "" {
		t.Fatal("未命中时不该触发回调")
	}
}

func TestMaskedSecret(t *testing.T) {
	k := &Key{Key: "sk-abcdefghijklmnopqrstuvwxyz"}
	m := k.MaskedSecret()
	if m == k.Key {
		t.Fatal("脱敏后不该等于原文")
	}
	if len(m) > len(k.Key) {
		t.Fatal("脱敏结果不该更长")
	}
	// 短 key 不崩
	if (&Key{Key: "sk-"}).MaskedSecret() != "" {
		t.Fatal("过短的 key 应返回空")
	}
}

func TestListSortedByCreatedAt(t *testing.T) {
	s := newTestStore(t)
	for _, n := range []string{"c", "a", "b"} {
		if _, err := s.Create(n, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	list := s.List("")
	if len(list) != 3 {
		t.Fatalf("应有 3 条，实际 %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].CreatedAt.Before(list[i-1].CreatedAt) {
			t.Fatal("列表应按创建时间正序")
		}
	}
	// List 返回副本，改动不该影响内部状态
	list[0].Name = "被篡改"
	if s.List("")[0].Name == "被篡改" {
		t.Fatal("List 应返回副本")
	}
}

// ── 多用户归属（2026-09-21）──

func TestOwnerIsolation(t *testing.T) {
	s, _ := NewStore("")

	alice, err := s.Create("cc", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Create("cc", "", "bob") // 同名不同 owner：允许
	if err != nil {
		t.Fatalf("same name under different owner should be allowed: %v", err)
	}
	if _, err := s.Create("cc", "", "alice"); err == nil {
		t.Fatal("duplicate name within same owner should fail")
	}

	// 列表按 owner 隔离
	if got := s.List("alice"); len(got) != 1 || got[0].Owner != "alice" {
		t.Fatalf("alice list = %+v", got)
	}
	if got := s.List("bob"); len(got) != 1 || got[0].ID != bob.ID {
		t.Fatalf("bob list = %+v", got)
	}
	if got := s.List(""); len(got) != 2 {
		t.Fatalf("admin list should see all, got %d", len(got))
	}

	// 删除：跨 owner 不可删（报 not exist，不泄露存在性）
	if err := s.Delete(alice.ID, "bob"); err == nil {
		t.Fatal("bob must not delete alice's key")
	}
	if err := s.Delete(alice.ID, "alice"); err != nil {
		t.Fatalf("alice should delete own key: %v", err)
	}
	// 管理员（owner=""）可删任意
	if err := s.Delete(bob.ID, ""); err != nil {
		t.Fatalf("admin should delete any key: %v", err)
	}

	// Verify 带回 owner
	k, _ := s.Create("k2", "", "carol")
	got, ok := s.Verify(k.Key)
	if !ok || got.Owner != "carol" {
		t.Fatalf("verify should return owner, got %+v ok=%v", got, ok)
	}
}
