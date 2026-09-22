// Package apikey 多 API Key 管理：为桥生成、校验、吊销独立的调用凭证。
//
// ── 为什么需要它 ─────────────────────────────────────────────
// 改造前每座桥只有一个全局 key（写在 config.json 或环境变量里），后果：
//
//	· 无法给不同调用方发不同的 key，一个人泄漏就得全站轮换；
//	· 无法单独吊销某个调用方；
//	· 无法知道"是谁在调"。
//
// 本包提供按 key 粒度的凭证表，与全局 key **并存**：全局 key 继续有效
// （兼容既有部署与 sub2api 账号），新建的 key 走同一张表校验。
//
// ── 存储 ────────────────────────────────────────────────────
// JSON 文件（默认 data/keys.json，0600），进程内加读写锁。单据量在几十到
// 几百之间，不值得引入数据库。
//
// ── 设计取舍 ─────────────────────────────────────────────────
//
//	· key 明文存盘：桥需要拿它做常数时间比较，存哈希意味着每次鉴权都要
//	  哈希（或维护第二个索引）。文件本身是 0600 且只在服务器上，
//	  与全局 key 的存放等级一致。**不要把这个文件提交进仓库。**
//	· 前缀 `sk-`：与 OpenAI 生态一致，客户端不需要特殊配置；
//	  与三座桥既有的全局 key 风格也一致。
//	· 吊销删除而不是标记位：桥没有审计需求，删掉最省事。
package apikey

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Key 一个 API Key 记录。
type Key struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"` // 明文，见包注释的取舍说明
	Note      string    `json:"note,omitempty"`
	Owner     string    `json:"owner,omitempty"` // 归属用户（SSO 用户名，多用户）；空 = 管理员/历史 key
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

// Store key 表的持久化层。
type Store struct {
	mu       sync.RWMutex
	keys     map[string]*Key // key = ID
	bySecret map[string]*Key // key = 明文 key
	filePath string
	// onUsed 在鉴权命中时被调用（用于回写 LastUsed），可为 nil。
	// 刻意做成回调而不是在 Store 里直接写盘：鉴权是热路径，
	// 每次都落盘会把磁盘拖垮。调用方自己决定节流策略。
	onUsed func(id string)
}

// NewStore 加载或新建 key 表。filePath 为空时仅内存态（测试用）。
func NewStore(filePath string) (*Store, error) {
	s := &Store{
		keys:     map[string]*Key{},
		bySecret: map[string]*Key{},
		filePath: filePath,
	}
	if filePath != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetUsedCallback 注册"key 被使用"的回调。
func (s *Store) SetUsedCallback(fn func(id string)) {
	s.mu.Lock()
	s.onUsed = fn
	s.mu.Unlock()
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次运行，空表
		}
		return err
	}
	var keys []*Key
	if err := json.Unmarshal(raw, &keys); err != nil {
		return fmt.Errorf("parse %s: %w", s.filePath, err)
	}
	for _, k := range keys {
		if k == nil || k.ID == "" || k.Key == "" {
			continue
		}
		s.keys[k.ID] = k
		s.bySecret[k.Key] = k
	}
	return nil
}

func (s *Store) save() error { // 调用方须持锁
	if s.filePath == "" {
		return nil
	}
	list := make([]*Key, 0, len(s.keys))
	for _, k := range s.keys {
		list = append(list, k)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.filePath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	// os.WriteFile 的 mode 只在**新建**时生效：文件已存在时它被忽略。
	// 上一版没有这一步，实测在服务器上会留下 0644（甚至 0666）的密钥文件。
	// 显式 Chmod 保证每次落盘都是 0600。
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath)
}

// Create 生成一个新 key。name 只用于展示；owner 为空表示管理员/通用 key。
// 名称唯一性按 owner 隔离：不同用户可以各自叫 "cc"。
func (s *Store) Create(name, note, owner string) (*Key, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("名称不能为空")
	}
	owner = strings.TrimSpace(owner)
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, k := range s.keys {
		if k.Name == name && k.Owner == owner {
			return nil, fmt.Errorf("名称 %q 已存在", name)
		}
	}

	secret, err := generate()
	if err != nil {
		return nil, err
	}
	k := &Key{
		ID:        newID(),
		Name:      name,
		Key:       secret,
		Note:      strings.TrimSpace(note),
		Owner:     owner,
		CreatedAt: time.Now().UTC(),
	}
	s.keys[k.ID] = k
	s.bySecret[k.Key] = k
	if err := s.save(); err != nil {
		delete(s.keys, k.ID)
		delete(s.bySecret, k.Key)
		return nil, err
	}
	return k, nil
}

// Delete 吊销一个 key。owner 非空时要求 key 归属匹配（普通用户只能删自己的）。
func (s *Store) Delete(id, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return fmt.Errorf("key %q 不存在", id)
	}
	if owner != "" && k.Owner != owner {
		return fmt.Errorf("key %q 不存在", id) // 不暴露他人 key 的存在性
	}
	delete(s.keys, id)
	delete(s.bySecret, k.Key)
	return s.save()
}

// Get 按 ID 取 key；owner 非空时要求归属匹配。
func (s *Store) Get(id, owner string) (*Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, false
	}
	if owner != "" && k.Owner != owner {
		return nil, false
	}
	cp := *k
	return &cp, true
}

// Verify 校验明文 key 是否有效。命中时触发 onUsed 回调。
//
// 用常数时间比较：虽然 key 空间足够大、时序攻击不现实，但这里是鉴权路径，
// 保持与既有全局 key 校验（subtle.ConstantTimeCompare）一致的写法。
func (s *Store) Verify(secret string) (*Key, bool) {
	if secret == "" {
		return nil, false
	}
	s.mu.RLock()
	k, ok := s.bySecret[secret]
	var cb func(string)
	if ok {
		cb = s.onUsed
	}
	s.mu.RUnlock()

	// 即使未命中，也做一次等长比较以模糊时序（表里没东西时跳过）
	if !ok {
		s.mu.RLock()
		for _, cand := range s.bySecret {
			subtle.ConstantTimeCompare([]byte(secret), []byte(cand.Key))
			break
		}
		s.mu.RUnlock()
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(k.Key)) != 1 {
		return nil, false
	}
	if cb != nil {
		cb(k.ID)
	}
	return k, true
}

// List 返回全部 key（按创建时间正序）。owner 非空时只返回该 owner 的 key。
func (s *Store) List(owner string) []*Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Key, 0, len(s.keys))
	for _, k := range s.keys {
		if owner != "" && k.Owner != owner {
			continue
		}
		cp := *k
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	return list
}

// Touch 更新某个 key 的最后使用时间（调用方负责节流）。
func (s *Store) Touch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[id]; ok {
		k.LastUsed = time.Now().UTC()
	}
}

// Count 返回 key 数量。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Flush 把内存态落盘（用于 Touch 之后）。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

// MaskedSecret 返回用于展示的脱敏形式：sk-abc…wxyz。
func (k *Key) MaskedSecret() string {
	if k == nil || len(k.Key) < 12 {
		return ""
	}
	return k.Key[:7] + "…" + k.Key[len(k.Key)-4:]
}

// ── 内部 ──

func generate() (string, error) {
	buf := make([]byte, 24) // 192 位
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机密钥失败: %w", err)
	}
	return "sk-" + hex.EncodeToString(buf), nil
}

func newID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("k_%d", time.Now().UnixNano())
	}
	return "k_" + hex.EncodeToString(buf)
}
