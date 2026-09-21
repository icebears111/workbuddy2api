// models.go 动态模型获取。
//
// 优先 GET /v2/models（OpenAI 风格），失败再 GET /v3/config（CodeBuddy 配置端点，
// 匿名访问时 data.models 为 null，必须带凭证）。两者都失败则回退静态表。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Model 一个可用模型。
type Model struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name,omitempty"`
	OwnedBy     string  `json:"owned_by,omitempty"`
	ContextLen  int64   `json:"context_length,omitempty"`
	Reasoning   bool    `json:"reasoning,omitempty"`
	Vision      bool    `json:"vision,omitempty"`
	CostFactor  float64 `json:"cost_factor,omitempty"` // 相对于基座的额度消耗系数
}

// FetchModels 拉取上游模型列表（默认 base + /v2/models → /v3/config 回退，带 15s 超时）。
func (c *Client) FetchModels(ctx context.Context, token string) ([]Model, error) {
	return c.FetchModelsRealm(ctx, c.Base, token, nil, ModelsPathV2, ConfigPathV3)
}

// FetchModelsRealm 在指定 base + 额外头下，依次尝试 paths 直到拿到非空模型列表。
func (c *Client) FetchModelsRealm(ctx context.Context, base, token string, hdr map[string]string, paths ...string) ([]Model, error) {
	if token == "" {
		return nil, fmt.Errorf("empty token")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	lastErr := fmt.Errorf("no models from upstream")
	for _, p := range paths {
		if ms, err := c.fetchPathBase(reqCtx, base, p, token, hdr); err == nil && len(ms) > 0 {
			return ms, nil
		} else if err != nil {
			lastErr = err
		}
	}
	return nil, lastErr
}

func (c *Client) fetchPath(ctx context.Context, path, token string) ([]Model, error) {
	return c.fetchPathBase(ctx, c.Base, path, token, nil)
}

func (c *Client) fetchPathBase(ctx context.Context, base, path, token string, hdr map[string]string) ([]Model, error) {
	resp, err := c.doReq(ctx, http.MethodGet, base+path, token, nil, hdr)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("models %s http %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	ms := parseModelList(raw)
	if len(ms) == 0 {
		return nil, fmt.Errorf("models %s: empty", path)
	}
	return ms, nil
}

// parseModelList 宽松解析：兼容
//   - {"data":[{"id":"x",...}]}                         OpenAI 风格
//   - {"data":{"models":[{"id":"x"}]}}                  /v3/config 数组形态
//   - {"data":{"models":{"x":{...}}}}                   /v3/config map 形态
//   - {"data":{"models":{"chat":[{"id":"x"}]}}}         /v3/config 按场景分组
//   - 裸数组 [{"id":"x"}]
func parseModelList(raw []byte) []Model {
	var out []Model
	seen := map[string]bool{}
	collect := func(items []any) {
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if mm := modelFromMap(m); mm != nil && !seen[mm.ID] {
				seen[mm.ID] = true
				out = append(out, *mm)
			}
		}
	}
	var collectFrom func(v any)
	collectFrom = func(v any) {
		switch t := v.(type) {
		case []any:
			collect(t)
		case map[string]any:
			// 可能是 map[id]detail：detail 里没有 id 字段时用 key 兜底（如 {"auto":{}}）
			for k, vv := range t {
				if d, ok := vv.(map[string]any); ok {
					mm := modelFromMap(d)
					if mm == nil {
						mm = &Model{ID: NormalizeModelName(k)}
					}
					if mm.ID != "" && !seen[mm.ID] {
						seen[mm.ID] = true
						out = append(out, *mm)
					}
					continue
				}
				collectFrom(vv)
			}
		}
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err == nil {
		if d, ok := probe["data"]; ok {
			var dv any
			if err := json.Unmarshal(d, &dv); err == nil {
				if arr, ok := dv.([]any); ok {
					collect(arr)
				} else if obj, ok := dv.(map[string]any); ok {
					// 仅在 data 含 "models" 字段时才解析模型；否则 data 是配置对象
					// （如 /v3/config 的 {"enterpriseId":...,"productFeatures":...}），
					// 不应被当成模型容器，返回空以触发 FallbackModels。
					if mv, ok := obj["models"]; ok {
						collectFrom(mv)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		var arr []any
		if err := json.Unmarshal(raw, &arr); err == nil {
			collect(arr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// modelFromMap 从任意形态的模型对象中提取字段；无法识别 id 时返回 nil。
func modelFromMap(m map[string]any) *Model {
	id := pickString(m, "id", "model", "key", "name", "model_id", "modelId", "display_name", "displayName")
	if id == "" {
		return nil
	}
	var out Model
	out.ID = NormalizeModelName(id)
	if dn := pickString(m, "display_name", "displayName", "title", "label", "name"); dn != "" {
		out.DisplayName = dn
	}
	if ob := pickString(m, "owned_by", "ownedBy", "provider", "vendor"); ob != "" {
		out.OwnedBy = ob
	}
	out.ContextLen = pickInt(m, "max_input_tokens", "maxInputTokens", "context_length", "contextLength", "max_context_tokens")
	out.Reasoning = pickBool(m, "is_reasoning", "reasoning", "isReasoning", "support_reasoning")
	out.Vision = pickBool(m, "is_vl", "vision", "isVL", "support_vision")
	if out.ID == "" {
		return nil
	}
	return &out
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func pickInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case float64:
				return int64(t)
			case json.Number:
				if n, err := t.Int64(); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func pickBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return false
}

// NormalizeModelName 把上游模型名转成 OpenAI 风格客户端名：
// 小写、空格/下划线转连字符、保留点号（版本号）、去重连字符。
// "DeepSeek-V4-Pro" → "deepseek-v4-pro"
func NormalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			b.WriteRune(r) // 中文等原样保留
			prevDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}

// DefaultFallbackModel 未知/不受支持的模型名回落到该模型。
const DefaultFallbackModel = "hy4-preview"

// supportedModelSet 上游实际可用的模型 ID 集合（取自 FallbackModels 这张实测清单）。
var supportedModelSet = func() map[string]bool {
	set := make(map[string]bool, 32)
	for _, m := range FallbackModels() {
		set[m.ID] = true
	}
	return set
}()

// IsSupportedModel 判断模型 ID 是否被上游支持。
func IsSupportedModel(id string) bool {
	return supportedModelSet[strings.ToLower(strings.TrimSpace(id))]
}

// MapModelName 把客户端传入的模型名映射为上游可接受的模型。
// 已支持的原样返回；不受支持的（如 claude-* / gpt-*）回落到 fallback，
// 避免把无效模型透传给上游导致 400（11102/11128）并把账号池拖入冷却。
func MapModelName(id, fallback string) (mapped string, rewritten bool) {
	n := NormalizeModelName(id)
	if IsSupportedModel(n) {
		return n, false
	}
	if strings.TrimSpace(fallback) == "" {
		fallback = DefaultFallbackModel
	}
	return fallback, true
}

// FallbackModels 静态回退表：上游没有对外开放的模型列表 API（实测 /v2/models 404、
// /v3/config 也不含模型），这里根据用户截图 + 对话接口实测得到的可用模型清单。
// cost_factor 来自 CodeBuddy UI 的「x」倍数；截图中未出现的模型不填。
// 注意：清单随时可能随 CodeBuddy 调整，新增/变更请以对话实测为准。
func FallbackModels() []Model {
	return []Model{
		{ID: "auto", DisplayName: "自动（Auto）"},
		{ID: "default", DisplayName: "默认（Default）"},
		{ID: "hy3", DisplayName: "混元 Hy3", CostFactor: 0.00},
		{ID: "hy4-preview", DisplayName: "混元 Hy4-Preview", CostFactor: 0.00},
		{ID: "deepseek-v4-flash", DisplayName: "DeepSeek-V4-Flash", CostFactor: 0.17},
		{ID: "deepseek-v4-pro", DisplayName: "DeepSeek-V4-Pro", CostFactor: 0.51},
		{ID: "deepseek-v3", DisplayName: "DeepSeek-V3"},
		{ID: "deepseek-v3.2", DisplayName: "DeepSeek-V3.2"},
		{ID: "deepseek-r1", DisplayName: "DeepSeek-R1", Reasoning: true},
		{ID: "glm-5.1", DisplayName: "GLM-5.1", CostFactor: 0.79},
		{ID: "glm-5.2", DisplayName: "GLM-5.2", CostFactor: 0.50},
		{ID: "glm-5.3", DisplayName: "GLM-5.3", CostFactor: 0.79},
		{ID: "glm-5.3-flash", DisplayName: "GLM-5.3-Flash", CostFactor: 0.06},
		{ID: "glm-5v-turbo", DisplayName: "GLM-5v-Turbo", CostFactor: 0.71},
		{ID: "kimi-k2.6", DisplayName: "Kimi-K2.6", CostFactor: 0.52},
		{ID: "kimi-k2.7", DisplayName: "Kimi-K2.7", CostFactor: 0.57},
		{ID: "kimi-k3", DisplayName: "Kimi-K3", CostFactor: 1.62},
		{ID: "minimax-m3", DisplayName: "MiniMax-M3", CostFactor: 0.25},
	}
}
