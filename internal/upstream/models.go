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
	"strconv"
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
	CostFactor  float64 `json:"cost_factor,omitempty"` // 旧字段：上游已不再返回，保留兼容
	// Credits 上游给的**额度消耗倍数**（字段名 credits，字符串 "x0.21"）。
	//
	// ⚠ 这是 CodeBuddy **自己的**计价口径，与 Qoder 的 PriceFactor 不是
	// 一个体系 —— 两家的「1.0」不表示同一个价格。前端因此分成各自的列，
	// 不合并成一个「倍率」（合并会让用户拿不同厂商的数字互相比较）。
	Credits float64 `json:"credits,omitempty"`
	// HasCredits 上游是否真的给了这个字段。
	//
	// 为什么要单独一个布尔：倍率可以是 **0.00**（实测 hy3 就是 "x0.00"，
	// 表示不消耗额度），用 `Credits != 0` 判「有没有」会把这种合法的 0
	// 当成「没有」，界面上就显示成「—」了 —— 那是两回事。
	HasCredits bool `json:"has_credits,omitempty"`
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
	// /v3/config 给的是**混合目录**（对话 + 补全 + 图像 + 小参数试验），
	// 实测 51 个里只有 33 个能用于 /v1/chat/completions。只对这一条路径做
	// 准入过滤 —— 其它形状（OpenAI 风格 /v2/models）本来就是纯对话清单，
	// 而且不一定带 maxOutputTokens，用它当判据会把正常模型全滤掉。
	if path == ConfigPathV3 {
		chat := filterChatModels(raw, ms)
		if len(chat) == 0 {
			// 一条都没剩 → 按「拿不到清单」处理（上层会走兜底），
			// 而不是返回空清单：空清单会让「缓存只在非空时写」的逻辑失效。
			return nil, fmt.Errorf("models %s: 0 chat models after filter (upstream gave %d)", path, len(ms))
		}
		return chat, nil
	}
	return ms, nil
}

// filterChatModels 从已解析的清单里剔除非对话模型。
//
// 必须回原始 JSON 再判一次：IsChatModel 要看 maxOutputTokens 与
// supportsExtra，而 Model 结构体不搬这两个字段（它们只用于准入判断）。
// 用 id 把两边对齐 —— parseModelList 已按 NormalizeModelName 归一，
// 这里对原目录项做同样的归一，才能对上。
func filterChatModels(raw []byte, parsed []Model) []Model {
	allow := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, it := range t {
				if m, ok := it.(map[string]any); ok {
					if IsChatModel(m) {
						if id := pickString(m, "id", "model", "key", "name", "model_id", "modelId"); id != "" {
							allow[NormalizeModelName(id)] = true
						}
					}
				}
			}
		case map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err == nil {
		if d, ok := probe["data"]; ok {
			var dv any
			if json.Unmarshal(d, &dv) == nil {
				walk(dv)
			}
		}
	}
	if len(allow) == 0 {
		var arr any
		if json.Unmarshal(raw, &arr) == nil {
			walk(arr)
		}
	}
	out := make([]Model, 0, len(parsed))
	for _, m := range parsed {
		if allow[m.ID] {
			out = append(out, m)
		}
	}
	return out
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
			// 准入过滤（非对话模型剔除）见 parseModelListChatOnly ——
			// 这里**不做**：本函数是纯形状解析，要能被单测独立验证，
			// 也不该假设每个上游都带 maxOutputTokens（那是 CodeBuddy
			// 目录专有的字段）。判据必须在这一层做的话就会伤到
			// 其它形状的解析（实测：加了之后 TestParseModelList 全挂）。
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
	// 倍率：上游叫 credits，值是**字符串** "x0.21"（不是数字）。
	// 见 CreditMultiplier 的说明 —— 这是 CodeBuddy 独有的口径，
	// Qoder 的 price_factor 是另一回事，不要混。
	if v, ok := pickMultiplier(m, "credits"); ok {
		out.Credits = v
		out.HasCredits = true
	}
	if out.ID == "" {
		return nil
	}
	return &out
}

// CreditMultiplier 上游 credits 字符串 → 数字。
//
// 上游给的是 **"x0.21" 这种带 x 前缀的字符串**（也可能是 "x2.00"），
// 所以不能当数字直接解析。实测 51 个模型里 32 个带这个字段。
//
// 语义：这是 CodeBuddy 自己的**额度消耗倍数**（相对基座），
// 与 Qoder 的 price_factor 不是一个体系 —— 两家各自标各自的，
// 放在一列里比较是错的（前端已拆成各自的列）。
func CreditMultiplier(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimPrefix(s, "X")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// pickMultiplier 从若干键名里取第一个能解析成倍率的值。
// 兼容字符串（"x0.21"）与数字（0.21）两种形态 —— 上游将来若改成数字，
// 这里不用跟着改。
func pickMultiplier(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			if f, ok := CreditMultiplier(t); ok {
				return f, true
			}
		case float64:
			return t, true
		case json.Number:
			if f, err := t.Float64(); err == nil {
				return f, true
			}
		}
	}
	return 0, false
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

// FallbackModels 静态回退表 —— 只在**所有域都拉不到**时用。
//
// ⚠ 2026-09-23 起上游的模型清单是实时拉取的（/v3/config，见 ModelsUserAgent），
// 这张表已经**不是主要来源**了，只作兜底。因此它天然会过期 ——
// 实测它的 kimi-k3 与上游当前的 kimi-k3-1 就对不上（好在旧名仍可用）。
// 别把它当真值修；真值以上游为准。
//
// credits 是 CodeBuddy 自己的额度倍数口径（上游字段名 credits，字符串 "x0.21"）。
func FallbackModels() []Model {
	return []Model{
		{ID: "auto", DisplayName: "自动（Auto）"},
		{ID: "default", DisplayName: "默认（Default）"},
		{ID: "hy3", DisplayName: "混元 Hy3", Credits: 0.00, HasCredits: true},
		{ID: "hy4-preview", DisplayName: "混元 Hy4-Preview", Credits: 0.00, HasCredits: true},
		{ID: "deepseek-v4-flash", DisplayName: "DeepSeek-V4-Flash", Credits: 0.17, HasCredits: true},
		{ID: "deepseek-v4.1-flash", DisplayName: "DeepSeek-V4.1-Flash", Credits: 0.03, HasCredits: true},
		{ID: "deepseek-v4-pro", DisplayName: "DeepSeek-V4-Pro", Credits: 0.51, HasCredits: true},
		{ID: "deepseek-v3", DisplayName: "DeepSeek-V3"},
		{ID: "deepseek-v3.2", DisplayName: "DeepSeek-V3.2"},
		{ID: "deepseek-r1", DisplayName: "DeepSeek-R1", Reasoning: true},
		{ID: "glm-5.1", DisplayName: "GLM-5.1", Credits: 0.79, HasCredits: true},
		{ID: "glm-5.2", DisplayName: "GLM-5.2", Credits: 0.50, HasCredits: true},
		{ID: "glm-5.3", DisplayName: "GLM-5.3", Credits: 0.79, HasCredits: true},
		{ID: "glm-5.3-flash", DisplayName: "GLM-5.3-Flash", Credits: 0.06, HasCredits: true},
		{ID: "glm-5v-turbo", DisplayName: "GLM-5v-Turbo", Credits: 0.71, HasCredits: true},
		{ID: "kimi-k2.6", DisplayName: "Kimi-K2.6", Credits: 0.52, HasCredits: true},
		{ID: "kimi-k2.7", DisplayName: "Kimi-K2.7", Credits: 0.57, HasCredits: true},
		{ID: "kimi-k3", DisplayName: "Kimi-K3", Credits: 1.62, HasCredits: true},
		{ID: "minimax-m3", DisplayName: "MiniMax-M3", Credits: 0.25, HasCredits: true},
	}
}

// ── 对话模型过滤（2026-09-23）────────────────────────────────
//
// 上游 /v3/config 返回的是一份**混合目录**：除了对话模型，还有代码补全
// （codewise-*）、补全基座（completion-*）、图像模型（hunyuan-image*）、
// 小参数试验模型（hunyuan-3b/7b）等。实测 51 个里只有 33 个是能用于
// /v1/chat/completions 的对话模型。
//
// 直接把 51 个透出去，客户端的下拉框里会出现 codewise-completions 这种
// 选了也没法聊天的项。所以按下面三条过滤。
//
// 规则**照抄 agent2api**（core/models/mod.rs 的 is_chat_model），
// 不自创 —— 它是在真实目录上迭代出来的，包括 maxOutputTokens>=16000 那条
// 经验值（实测把 hunyuan-chat 排除掉了，那个只支持 8192 输出）。
//
// 排除前缀（agent2api 同名常量 CHAT_MODEL_EXCLUDE_PREFIXES）
var chatModelExcludePrefixes = []string{
	"completion-",
	"codewise-",
	"hunyuan-image",
	"hunyuan-3b",
	"hunyuan-7b",
	"deepseek-r1-0528",
	"deepseek-v3-0324",
	"kimi-k2-instruct",
	"default-1.",
}

// IsChatModel 判断一条目录项是否可用于对话。
//
// 三条规则（全部满足才算对话模型）：
//  1. id 不以排除前缀开头；
//  2. 没有 supportsExtra 真值（那是补全类的附加通道）；
//  3. maxOutputTokens 存在且 >= 16000。
//
// 第 3 条最容易被忽略：**缺字段也算不合格**（不是"没限制"）——
// 上游对非对话项压根不给这个字段。
func IsChatModel(m map[string]any) bool {
	id := pickString(m, "id", "model", "key", "name", "model_id", "modelId")
	for _, p := range chatModelExcludePrefixes {
		if strings.HasPrefix(id, p) {
			return false
		}
	}
	if v, ok := m["supportsExtra"]; ok && jsTruthy(v) {
		return false
	}
	mo := pickInt(m, "maxOutputTokens", "max_output_tokens")
	return mo >= 16000
}

// jsTruthy 对应 JS 的真值判定（agent2api 用 js_truthy，语义要对齐：
// 非零数字、非空字符串、true 都算真）。
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case json.Number:
		f, err := t.Float64()
		return err == nil && f != 0
	case string:
		return strings.TrimSpace(t) != ""
	case nil:
		return false
	default:
		return true
	}
}
