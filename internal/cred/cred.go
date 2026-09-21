// Package cred 管理 CodeBuddy 凭证（ck_ API Key 或 OAuth Bearer Token）。
//
// 凭证来源二选一，均落盘到 auth_dir 下的 json 文件：
//
//  1. ck_ API Key —— 在 CodeBuddy 控制台「API Key」页面生成（推荐，长期有效）：
//     中国版 https://copilot.tencent.com/profile/keys
//     海外版 https://www.codebuddy.ai/profile/keys
//
//  2. OAuth Bearer Token —— 通过 OAuth 授权拿到的 access token（可配 refresh_token 自动续期）。
//
// 与 qoderwork2api 不同：CodeBuddy 官方 API 本身就是 OpenAI 兼容格式，
// 不需要 COSY 签名、不需要机器指纹，只要一个 Bearer 串即可。
package cred

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Kind 凭证类型。
type Kind string

const (
	KindAPIKey Kind = "api_key" // ck_ 静态 Key，不会过期
	KindToken  Kind = "token"   // OAuth Bearer token，可能过期
)

// Cred 一个账号的凭证 + 运行时状态。
type Cred struct {
	UID      string
	Nickname string

	// Owner 账号归属（SSO 用户名，2026-09-21 多用户）。
	// 空 = 无主账号（历史账号 / 管理员共享池），只有管理员视图可见与调度。
	// 非空 = 该用户私有账号，选号时只在该 owner 的账号里挑。
	Owner string

	// Realm 凭证所属域："" 用全局上游（默认 CN copilot.tencent.com）；
	// "saas" 表示来自 CodeBuddy OAuth 登录、走 www.codebuddy.ai。
	Realm string

	// Token 实际放进 Authorization / X-Api-Key 的串。
	Token string
	Kind  Kind

	// RefreshToken 仅 KindToken 且上游支持 OAuth2 续期时有意义。
	RefreshToken string
	// ExpiresAt token 过期时间（unix 秒）；0 表示未知 / 永不过期。
	ExpiresAt int64

	mu sync.Mutex
}

// AuthInvalidError 凭证已失效（refresh 失败或 token 被吊销）——需要重新登录。
type AuthInvalidError struct{ Msg string }

func (e *AuthInvalidError) Error() string { return "auth_invalid: " + e.Msg }

// IsAuthInvalid 判断错误是否凭证失效。
func IsAuthInvalid(err error) bool {
	var pe *AuthInvalidError
	return errors.As(err, &pe)
}

// LoadFile 从磁盘加载凭证文件。
//
// 期望形态（推荐，addkey.sh 落盘）：
//
//	{"api_key":"ck_xxx","uid":"xxx","nickname":"xxx"}
//	{"token":"eyJ...","refresh_token":"...","expires_at":1700000000,"uid":"xxx"}
//
// 同时兼容 IDE 风格嵌套形态：
//
//	{"auth":{"accessToken":"...","refreshToken":"..."},"account":{"uid":"...","nickname":"..."}}
//
// 以及扁平别名：{"key":...} / {"access_token":...} / {"user_id":...}。
func LoadFile(path string) (*Cred, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	var c Cred
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				AccessKey    string `json:"access_key"`
				APIKey       string `json:"apiKey"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
			} `json:"auth"`
			Account struct {
				UID      string `json:"uid"`
				UserID   string `json:"user_id"`
				Nickname string `json:"nickname"`
				Name     string `json:"name"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		c.Token = firstNonEmpty(n.Auth.AccessToken, n.Auth.AccessKey, n.Auth.APIKey)
		c.RefreshToken = n.Auth.RefreshToken
		c.ExpiresAt = n.Auth.ExpiresAt
		c.UID = firstNonEmpty(n.Account.UID, n.Account.UserID)
		c.Nickname = firstNonEmpty(n.Account.Nickname, n.Account.Name)
	} else {
		var f struct {
			APIKey       string `json:"api_key"`
			Key          string `json:"key"`
			Token        string `json:"token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    int64  `json:"expires_at"`
			UID          string `json:"uid"`
			UserID       string `json:"user_id"`
			Nickname     string `json:"nickname"`
			Name         string `json:"name"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		c.Token = firstNonEmpty(f.APIKey, f.Key, f.Token, f.AccessToken)
		c.RefreshToken = f.RefreshToken
		c.ExpiresAt = f.ExpiresAt
		c.UID = firstNonEmpty(f.UID, f.UserID)
		c.Nickname = firstNonEmpty(f.Nickname, f.Name)
	}

	if c.Token == "" {
		return nil, fmt.Errorf("missing api_key/token in %s", path)
	}
	c.Token = strings.TrimSpace(c.Token)
	c.Kind = classifyKind(c.Token)

	if c.UID == "" {
		// 兜底 1：文件名 codebuddy-<uid>.json
		base := filepath.Base(path)
		base = strings.TrimSuffix(base, filepath.Ext(base))
		for _, prefix := range []string{"codebuddy-", "codebuddy_", "buddy-"} {
			if strings.HasPrefix(base, prefix) {
				c.UID = strings.TrimPrefix(base, prefix)
				break
			}
		}
	}
	if c.UID == "" {
		// 兜底 2：用 token 指纹作为稳定 ID（避免不同账号互相覆盖）
		c.UID = fingerprint(c.Token)
	}
	if c.Nickname == "" {
		c.Nickname = maskToken(c.Token)
	}
	// 读取顶层 realm（OAuth 登录写入的 saas 标记）
	if rb, ok := probe["realm"]; ok {
		var s string
		if json.Unmarshal(rb, &s) == nil && s != "" {
			c.Realm = s
		}
	}
	// 读取顶层 owner（多用户归属；管理员/历史凭证无此字段 = 无主）
	if ob, ok := probe["owner"]; ok {
		var s string
		if json.Unmarshal(ob, &s) == nil {
			c.Owner = strings.TrimSpace(s)
		}
	}
	return &c, nil
}

// LoadDir 扫描目录下 *.json，跳过解析失败的。
func LoadDir(dir string) ([]*Cred, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Cred
	for _, f := range files {
		c, err := LoadFile(f)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// SaveFile 把凭证落盘到 dir/<uid>.json。
func SaveFile(dir string, c *Cred) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "codebuddy-"+sanitize(c.UID)+".json")
	doc := map[string]any{
		"uid":      c.UID,
		"nickname": c.Nickname,
		"type":     string(c.Kind),
	}
	if c.Owner != "" {
		doc["owner"] = c.Owner
	}
	if c.Realm != "" {
		doc["realm"] = c.Realm
	}
	if c.Kind == KindAPIKey {
		doc["api_key"] = c.Token
	} else {
		doc["token"] = c.Token
	}
	if c.RefreshToken != "" {
		doc["refresh_token"] = c.RefreshToken
	}
	if c.ExpiresAt > 0 {
		doc["expires_at"] = c.ExpiresAt
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureToken 返回可直接用于上游请求的 token。
//   - ck_ Key：永不过期，原样返回。
//   - OAuth token：临近过期（<30min）时用 refresh_token 续期；失败返回 AuthInvalidError。
func (c *Cred) EnsureToken(tokenURL string, clientID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Kind == KindAPIKey || c.ExpiresAt == 0 {
		return c.Token, nil
	}
	if time.Now().Unix() < c.ExpiresAt-1800 {
		return c.Token, nil
	}
	if c.RefreshToken == "" {
		// 没有 refresh 手段，先冒险用一次，由上游 401 决定是否禁用。
		return c.Token, nil
	}
	if tokenURL == "" {
		return c.Token, nil
	}
	if err := c.refreshLocked(tokenURL, clientID); err != nil {
		return "", err
	}
	return c.Token, nil
}

func (c *Cred) refreshLocked(tokenURL, clientID string) error {
	form := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
	}
	if clientID != "" {
		form["client_id"] = clientID
	}
	body, err := json.Marshal(form)
	if err != nil {
		return err
	}
	raw, status, err := httpPostJSON(tokenURL, body)
	if err != nil {
		return err
	}
	if status == 401 || status == 403 {
		return &AuthInvalidError{Msg: fmt.Sprintf("refresh http %d", status)}
	}
	if status >= 400 {
		return fmt.Errorf("refresh http %d: %s", status, truncate(string(raw), 200))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("refresh parse: %w", err)
	}
	if out.AccessToken == "" {
		return fmt.Errorf("refresh: empty access_token")
	}
	c.Token = out.AccessToken
	c.Kind = classifyKind(c.Token)
	if out.RefreshToken != "" {
		c.RefreshToken = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ClassifyKind 按前缀推断凭证类型（ck_ 为静态 API Key，其余视为 OAuth token）。
func ClassifyKind(tok string) Kind {
	if strings.HasPrefix(tok, "ck_") {
		return KindAPIKey
	}
	return KindToken
}

func classifyKind(tok string) Kind { return ClassifyKind(tok) }

// UserID 取上游身份头 X-User-Id 应填的值。
//   - OAuth/JWT token：解析 JWT payload 的 sub 声明（官方桌面端/codebuddy2api 均用此 UUID 作为 X-User-Id），
//     与个人资料里的 uid 一致；sub 缺失时退化到文件里的 UID。
//   - ck_ 静态 Key：没有 JWT，直接用文件里的 UID（若为空则留空，由调用方决定）。
func (c *Cred) UserID() string {
	if c.Kind == KindAPIKey || c.Token == "" {
		return c.UID
	}
	parts := strings.Split(c.Token, ".")
	if len(parts) != 3 {
		return c.UID
	}
	raw := parts[1]
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return c.UID
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(b, &claims); err != nil {
		return c.UID
	}
	if claims.Sub != "" {
		return claims.Sub
	}
	return c.UID
}

// maskToken 脱敏展示：保留前 6 后 4。
func maskToken(tok string) string {
	n := len(tok)
	if n <= 12 {
		return "***"
	}
	return tok[:6] + "***" + tok[n-4:]
}

// fingerprint 生成 token 的稳定短 ID（无 uid 时兜底）。
func fingerprint(tok string) string {
	h := fnv1a64(tok)
	return fmt.Sprintf("k%013x", h)
}

func fnv1a64(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func sanitize(s string) string {
	rep := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return rep.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
