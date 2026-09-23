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
	disabled map[string]bool // key = 小写模型 id（显式禁用）
	enabled  map[string]bool // 显式启用 —— 用于覆盖「默认禁用」
	filePath string
}

// fileShape 是落盘格式。带上版本号，将来若要改结构可识别旧文件。
type fileShape struct {
	Version  int      `json:"version"`
	Disabled []string `json:"disabled"`
	// Enabled 显式启用的模型（版本 2 新增）。
	//
	// ── 为什么需要「显式启用」这一档 ──────────────────────────
	// 有些模型是**默认禁用**的（如只在国际域可见的那些 —— 国内账号
	// 根本服务不了它们，列出来只会让人点名请求然后失败）。
	// 「默认禁用」得能被用户改回来，否则那不叫默认、叫写死。
	//
	// 三档状态：
	//   都没出现在 Enabled/Disabled → 用调用方给的默认值
	//   出现在 Disabled             → 禁用（用户显式关的）
	//   出现在 Enabled              → 启用（用户显式开的，压过默认禁用）
	//
	// 只存禁用集合 + 一个「默认」是不够的：那样用户点开启后，
	// 下次刷新又会被默认值按回去。
	Enabled []string `json:"enabled,omitempty"`
}

// NewStore 加载或新建。filePath 为空时仅内存态（测试用）。
//
// **加载失败不返回错误** —— 与 apikey.NewStore 的取舍不同，这里刻意容错：
// 一个损坏的 models.json 不该让整个网关起不来（那会连转发都停）。
// 坏文件按「没有禁用项」处理，并把它改名留档，避免下次又读到同一个坏文件。
func NewStore(filePath string) *Store {
	s := &Store{disabled: map[string]bool{}, enabled: map[string]bool{}, filePath: filePath}
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
	// Enabled 是版本 2 才有的字段；旧文件没有这一项 → 空，行为与旧版一致。
	for _, id := range shape.Enabled {
		if k := normalize(id); k != "" {
			s.enabled[k] = true
		}
	}
	return s
}

// normalize 统一成小写去空格。模型 id 的大小写在各家不一致
// （客户端可能发 DeepSeek-V4.1-Flash），比较前统一。
func normalize(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// IsDisabled 该模型是否被禁用（显式禁用者）。
//
// ⚠ 只看显式禁用表。带「默认值」的判定用 Resolve —— 绝大多数调用方
// 要的是 Resolve，因为模型清单里有一批是默认禁用的（国际域独有）。
func (s *Store) IsDisabled(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.disabled[normalize(id)]
}

// Resolve 解析一个模型最终该不该被禁用。
//
// def 是调用方给的**默认值**（如「这个模型只在国际域可见 → 默认禁用」）。
// 用户的显式选择永远压过默认值：
//
//	显式禁用 → true        （不管默认是什么）
//	显式启用 → false       （哪怕默认是禁用）
//	都没设   → def
//
// 少了「显式启用」这一档，默认禁用就会变成写死 —— 用户点开启后
// 下次刷新被默认值按回去，看起来像开关坏了。
func (s *Store) Resolve(id string, def bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k := normalize(id)
	if k == "" {
		return def
	}
	if s.disabled[k] {
		return true
	}
	if s.enabled[k] {
		return false
	}
	return def
}

// SetDisabled 写入一个模型的启停状态；返回是否有变化。
//
// ⚠ 这里的「变化」必须按**存储层**判，不能按「最终是否禁用」判。
// 因为存储要表达的是三档状态（未表态 / 显式禁用 / 显式启用）：
//
//	SetDisabled(id, false) 对一个**未表态**的 id **是有变化的** ——
//	它把状态从「未表态」变成「显式启用」。若只比较 disabled[key]，
//	会得出「本来就是 false、没变化」而跳过写入，于是**默认禁用永远
//	开不起来**（这个 bug 被 TestExplicitEnablePersists 抓到了）。
//
// 写入时会清掉另一档：两档不能同时存在，否则文件里留着矛盾状态，
// 换个人读会得出不同结论。
func (s *Store) SetDisabled(id string, disabled bool) bool {
	key := normalize(id)
	if key == "" {
		return false
	}
	s.mu.Lock()
	_, inDisabled := s.disabled[key]
	_, inEnabled := s.enabled[key]
	if disabled {
		if inDisabled && !inEnabled {
			s.mu.Unlock()
			return false // 已经是「显式禁用」，无变化
		}
		s.disabled[key] = true
		delete(s.enabled, key)
	} else {
		if inEnabled && !inDisabled {
			s.mu.Unlock()
			return false // 已经是「显式启用」，无变化
		}
		s.enabled[key] = true
		delete(s.disabled, key)
	}
	s.mu.Unlock()
	s.save()
	return true
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
	enabledList := make([]string, 0, len(s.enabled))
	for id := range s.enabled {
		enabledList = append(enabledList, id)
	}
	s.mu.RUnlock()
	sort.Strings(list)
	sort.Strings(enabledList)

	// Version 2：加了 enabled 一档（见 fileShape 的说明）。
	body, err := json.MarshalIndent(
		fileShape{Version: 2, Disabled: list, Enabled: enabledList}, "", "  ")
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
