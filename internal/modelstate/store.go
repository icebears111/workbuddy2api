// Package modelstate 模型启停表（看板「模型」页用）。
//
// ── 为什么存「禁用」而不是「启用」────────────────────────────
// 三个桥的模型清单都是**动态**的：要么来自上游目录（buddy/catpaw），
// 要么来自内置兜底表。若存「启用集合」，上游新增一个模型时它默认不在集合里
// → **新模型静默不可用**，而且没人会发现（页面不显示它，也不会报错）。
// 存「禁用集合」则相反：新模型默认可用，只有显式禁掉的才消失。
// 失败方向不同 —— 前者是「悄悄少了一个模型」，后者是「多了个能用的模型」。
//
// ── 与 apikey 包的关系 ──────────────────────────────────────
// 形状照 internal/apikey/store.go 抄（JSON 文件 + RWMutex + NewStore），
// 三个桥各一份**完全相同的实现** —— 与那套多 key 的取舍一致：
// 桥之间不共享代码，但实现要一致，改一处要同步另两处。
package modelstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store 禁用表。零值不可用，用 NewStore 构造。
type Store struct {
	mu       sync.RWMutex
	disabled map[string]bool // key = 小写模型 id
	filePath string
}

// fileShape 是落盘格式。带上版本号，将来若要改结构可识别旧文件。
type fileShape struct {
	Version  int      `json:"version"`
	Disabled []string `json:"disabled"`
}

// NewStore 加载或新建。filePath 为空时仅内存态（测试用）。
//
// **加载失败不返回错误** —— 与 apikey.NewStore 的取舍不同，这里刻意容错：
// 一个损坏的 models.json 不该让整个网关起不来（那会连转发都停）。
// 坏文件按「没有禁用项」处理，并把它改名留档，避免下次又读到同一个坏文件。
func NewStore(filePath string) *Store {
	s := &Store{disabled: map[string]bool{}, filePath: filePath}
	if filePath == "" {
		return s
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return s // 不存在 = 全新的表
	}
	var shape fileShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		// 坏文件留档后从空表开始。留档而不是删除：用户可能想看看为什么坏了。
		_ = os.Rename(filePath, filePath+".broken")
		return s
	}
	for _, id := range shape.Disabled {
		if k := normalize(id); k != "" {
			s.disabled[k] = true
		}
	}
	return s
}

// normalize 统一成小写去空格。模型 id 的大小写在各家不一致
// （客户端可能发 DeepSeek-V4.1-Flash），比较前统一。
func normalize(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// IsDisabled 该模型是否被禁用。
func (s *Store) IsDisabled(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.disabled[normalize(id)]
}

// List 当前被禁用的模型 id（原样返回存进去的写法，已排序）。
func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.disabled))
	for id := range s.disabled {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Count 禁用项数量。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.disabled)
}

// SetDisabled 写入一个模型的启停状态；返回是否有变化。
func (s *Store) SetDisabled(id string, disabled bool) bool {
	key := normalize(id)
	if key == "" {
		return false
	}
	s.mu.Lock()
	_, exists := s.disabled[key]
	if disabled == exists {
		s.mu.Unlock()
		return false // 状态没变，不落盘
	}
	if disabled {
		s.disabled[key] = true
	} else {
		delete(s.disabled, key)
	}
	s.mu.Unlock()
	s.save()
	return true
}

// save 落盘。失败只记不报 —— 内存里的状态已经生效，
// 最坏情况是重启后丢一次改动，不该为此让用户的写请求失败。
func (s *Store) save() {
	if s.filePath == "" {
		return
	}
	s.mu.RLock()
	list := make([]string, 0, len(s.disabled))
	for id := range s.disabled {
		list = append(list, id)
	}
	s.mu.RUnlock()
	sort.Strings(list)

	body, err := json.MarshalIndent(fileShape{Version: 1, Disabled: list}, "", "  ")
	if err != nil {
		return
	}
	// 先写临时文件再改名：直接覆盖时若进程被杀，会留下半截 JSON。
	// 这个文件每次启停都要写，崩在写一半的概率不为零。
	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	// **必须显式 Chmod**：os.WriteFile 的 mode 只在**新建**时生效，
	// 文件已存在时沿用旧权限 —— 与 apikey 那处踩过的坑同一个。
	_ = os.Chmod(tmp, 0o600)
	_ = os.Rename(tmp, s.filePath)
}
