// Package server HTTP 路由：OpenAI 兼容 API + 管理 API + 前端页面。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"codebuddy2api/internal/authcb"
	"codebuddy2api/internal/cred"
	"codebuddy2api/internal/pool"
	"codebuddy2api/internal/upstream"
	"codebuddy2api/internal/usage"
)

//go:embed admin.html
var adminHTML embed.FS

// RealmConfig 某个账号域的上游配置。
type RealmConfig struct {
	Base        string            // 上游 base，如 https://copilot.tencent.com
	ChatPath    string            // 对话端点路径，如 /v2/chat/completions
	ModelsPaths []string          // 模型列表候选路径（按顺序尝试）
	Headers     map[string]string // 该域额外请求头（如 SaaS 的 X-Domain 等）
}

// Config handler 配置。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	APIKey   string
	// FallbackModel 上游不受支持的模型名（如 claude-* / gpt-*）回落到该模型。
	// 留空时用 upstream.DefaultFallbackModel。
	FallbackModel string
	MaxRotate     int
	HardCooldown  time.Duration
	SoftCooldown  time.Duration
	ErrThreshold  int
	ErrCooldown   time.Duration
	AuthDir       string
	OnReload      func() (int, error)
	OnAdd         func(c *cred.Cred) (string, error)
	OnRemove      func(uid string) error
	TokenURL      string
	ClientID      string
	Tracker       *usage.Tracker

	// Realms 各账号域的上游配置；key 为 Cred.Realm（"" 与 "cn" 走默认上游）。
	Realms map[string]RealmConfig
}

// realmFor 返回某账号应使用上游配置；缺省回落到全局上游。
func (h *Handler) realmFor(c *cred.Cred) RealmConfig {
	if c != nil && c.Realm != "" {
		if r, ok := h.cfg.Realms[c.Realm]; ok {
			hdr := r.Headers
			if c.Realm == "saas" && c.UID != "" {
				// SaaS 域需要按账号带上 X-User-Id
				hdr = make(map[string]string, len(r.Headers)+1)
				for k, v := range r.Headers {
					hdr[k] = v
				}
				hdr["X-User-Id"] = c.UID
			} else if c.Realm == "cn" {
				// 对齐 codebuddy2api：CN 域带上桌面身份头，避免上游判为
				// "unapproved channel"（业务码 11128）。实测缺这些头时简单请求也能 200，
				// 但带工具/长上下文的 agent 请求容易被上游安全策略拦截 → 11128。
				hdr = cnBaseHeaders(c)
			}
			return RealmConfig{Base: r.Base, ChatPath: r.ChatPath, ModelsPaths: r.ModelsPaths, Headers: hdr}
		}
	}
	return RealmConfig{
		Base:        h.cfg.Upstream.Base,
		ChatPath:    upstream.ChatPath,
		ModelsPaths: []string{upstream.ModelsPathV2, upstream.ConfigPathV3},
		Headers:     nil,
	}
}

// cnBaseHeaders 构造 CN 域（copilot.tencent.com）请求应带的桌面身份头，对齐 codebuddy2api：
//   - X-Domain：CodeBuddy CN 控制台域，与 cn token 的 issuer（www.codebuddy.cn）一致；
//   - X-User-Id：JWT 里的真实用户 UUID（sub），上游据此识别账号所属渠道。
//
// 缺省走 token 的 issuer 域；若账号 token 的 issuer 是其它域（如 workbuddy），
// 以 issuer 的 host 为准更稳妥，这里只处理最常见的 codebuddy.cn 场景。
func cnBaseHeaders(c *cred.Cred) map[string]string {
	const cnDomain = "www.codebuddy.cn"
	hdr := map[string]string{"X-Domain": cnDomain}
	if c != nil {
		if uid := c.UserID(); uid != "" {
			hdr["X-User-Id"] = uid
		}
	}
	return hdr
}

// storeModels 写入全局模型缓存（供 /v1/models 返回）。
func (h *Handler) storeModels(ms []upstream.Model) {
	h.modelsMu.Lock()
	h.models = ms
	h.fetchedAt = time.Now()
	h.modelsSeen = map[string]bool{}
	for _, m := range ms {
		h.modelsSeen[m.ID] = true
	}
	h.modelsMu.Unlock()
}

// Handler HTTP 路由处理器。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	modelsMu   sync.RWMutex
	models     []upstream.Model
	modelsSeen map[string]bool
	fetchedAt  time.Time
}

const modelsTTL = time.Hour

// NewHandler 构建路由。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.FallbackModel == "" {
		cfg.FallbackModel = upstream.DefaultFallbackModel
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), modelsSeen: map[string]bool{}}

	// OpenAI 兼容 API
	h.mux.HandleFunc("POST /v1/chat/completions", requireAPIKey(cfg.APIKey, h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", requireAPIKey(cfg.APIKey, h.listModels))

	// OpenAI Responses（Codex CLI 走这个）
	h.mux.HandleFunc("POST /v1/responses", requireAPIKey(cfg.APIKey, h.responsesAPI))
	// Anthropic Messages（Claude Code / CC Switch 走这个）
	h.mux.HandleFunc("POST /v1/messages", requireAPIKey(cfg.APIKey, h.anthropicMessages))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", requireAPIKey(cfg.APIKey, h.anthropicCountTokens))

	// 健康检查（不认证，给容器 healthcheck 用）
	h.mux.HandleFunc("GET /healthz", h.healthz)

	// 管理 API
	h.mux.HandleFunc("GET /api/status", requireAPIKey(cfg.APIKey, h.apiStatus))
	h.mux.HandleFunc("GET /api/accounts", requireAPIKey(cfg.APIKey, h.apiAccounts))
	h.mux.HandleFunc("POST /api/accounts", requireAPIKey(cfg.APIKey, h.apiAddAccount))
	h.mux.HandleFunc("POST /api/accounts/test", requireAPIKey(cfg.APIKey, h.apiTestAccount))
	h.mux.HandleFunc("POST /api/accounts/retest", requireAPIKey(cfg.APIKey, h.apiRetest))
	h.mux.HandleFunc("POST /api/accounts/reload", requireAPIKey(cfg.APIKey, h.apiReload))
	h.mux.HandleFunc("GET /api/auth/start", requireAPIKey(cfg.APIKey, h.apiAuthStart))
	h.mux.HandleFunc("POST /api/auth/poll", requireAPIKey(cfg.APIKey, h.apiAuthPoll))
	h.mux.HandleFunc("POST /api/accounts/enable", requireAPIKey(cfg.APIKey, h.apiEnable))
	h.mux.HandleFunc("POST /api/accounts/disable", requireAPIKey(cfg.APIKey, h.apiDisable))
	h.mux.HandleFunc("DELETE /api/accounts", requireAPIKey(cfg.APIKey, h.apiRemove))
	h.mux.HandleFunc("GET /api/models", requireAPIKey(cfg.APIKey, h.apiModels))
	h.mux.HandleFunc("GET /api/quota", requireAPIKey(cfg.APIKey, h.apiQuota))
	h.mux.HandleFunc("GET /api/quota/all", requireAPIKey(cfg.APIKey, h.apiQuotaAll))
	h.mux.HandleFunc("GET /api/quota/usage", requireAPIKey(cfg.APIKey, h.apiQuotaUsage))
	h.mux.HandleFunc("POST /api/quota/limit", requireAPIKey(cfg.APIKey, h.apiQuotaLimit))
	h.mux.HandleFunc("POST /api/checkin", requireAPIKey(cfg.APIKey, h.apiCheckin))
	h.mux.HandleFunc("GET /api/checkin/status", requireAPIKey(cfg.APIKey, h.apiCheckinStatus))

	// 前端
	h.mux.HandleFunc("GET /admin", h.serveAdmin)
	h.mux.HandleFunc("GET /admin/", h.serveAdmin)
	h.mux.HandleFunc("/", h.serveIndex)

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// OpenAI 兼容接口
// ---------------------------------------------------------------------------

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	ms := h.effectiveModels()
	data := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		entry := map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": firstNonEmpty(m.OwnedBy, "codebuddy"),
		}
		if m.DisplayName != "" {
			entry["display_name"] = m.DisplayName
		}
		if m.Reasoning {
			entry["reasoning"] = true
		}
		if m.Vision {
			entry["vision"] = true
		}
		if m.ContextLen > 0 {
			entry["context_length"] = m.ContextLen
		}
		if m.CostFactor != 0 {
			entry["cost_factor"] = m.CostFactor
		}
		data = append(data, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) apiModels(w http.ResponseWriter, r *http.Request) {
	h.listModels(w, r)
}

// apiQuota 拉取指定（或首个健康）账号的上游套餐/额度信息，并合并本地累计消耗。
func (h *Handler) apiQuota(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	var acct *cred.Cred
	if uid != "" {
		acct = h.cfg.Pool.AuthByUID(uid)
	} else {
		acct = h.cfg.Pool.Pick()
	}
	if acct == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no account available"})
		return
	}
	rlm := h.realmFor(acct)
	token, err := acct.EnsureToken(h.cfg.TokenURL, h.cfg.ClientID)
	if err != nil {
		if cred.IsAuthInvalid(err) {
			h.cfg.Pool.Disable(acct.UID, "auth invalid (re-login required)")
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}
	info, err := h.cfg.Upstream.FetchQuota(r.Context(), rlm.Base, token, rlm.Headers)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	snap := usage.Snapshot{}
	if h.cfg.Tracker != nil {
		snap = h.cfg.Tracker.Snapshot()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"uid":          acct.UID,
		"realm":        acct.Realm,
		"payment_type": info.PaymentType,
		"upgrade":      info.Upgrade,
		"account":      info.Account,
		"raw":          info.Raw,
		"resource":     info.Resource,
		"usage":        snap,
	})
}

func (h *Handler) effectiveModels() []upstream.Model {
	h.modelsMu.RLock()
	if len(h.models) > 0 && time.Since(h.fetchedAt) < modelsTTL {
		ms := h.models
		h.modelsMu.RUnlock()
		return ms
	}
	h.modelsMu.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return upstream.FallbackModels()
	}
	rlm := h.realmFor(acct)
	token, err := acct.EnsureToken(h.cfg.TokenURL, h.cfg.ClientID)
	if err != nil {
		if cred.IsAuthInvalid(err) {
			h.cfg.Pool.Disable(acct.UID, "auth invalid (re-login required)")
		}
		return upstream.FallbackModels()
	}
	ms, err := h.cfg.Upstream.FetchModelsRealm(context.Background(), rlm.Base, token, rlm.Headers, rlm.ModelsPaths...)
	if err != nil || len(ms) == 0 {
		log.Printf("models: fetch failed (%v), using fallback table", err)
		return upstream.FallbackModels()
	}
	h.modelsMu.Lock()
	h.models = ms
	h.fetchedAt = time.Now()
	h.modelsSeen = map[string]bool{}
	for _, m := range ms {
		h.modelsSeen[m.ID] = true
	}
	h.modelsMu.Unlock()
	return ms
}

type chatRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Stream   bool             `json:"stream"`
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var probe chatRequest
	if err := json.Unmarshal(body, &probe); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse json: "+err.Error())
		return
	}
	if probe.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if len(probe.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "messages is required")
		return
	}

	// 保留客户端全部字段做透传（temperature/top_p/tools/... ）
	var clientReq map[string]any
	if err := json.Unmarshal(body, &clientReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse json: "+err.Error())
		return
	}

	// 上游没有 Claude / GPT 等模型，若把未知模型名透传过去，上游会返回 400
	// （11102 model not found / 11128 unapproved channel），不仅本次请求失败，
	// 还会累计错误把账号逐个拖入冷却，最终整个服务变成
	// "all accounts unavailable (cooling/disabled)"。
	// 因此这里把不受支持的模型统一回落到可用模型，让客户端能正常对话。
	model, rewritten := upstream.MapModelName(probe.Model, h.cfg.FallbackModel)
	if rewritten {
		log.Printf("chat: model %q not supported by upstream, fallback to %q", probe.Model, model)
	}
	clientReq["model"] = model

	// 工具能力探测短路：部分客户端网关（claude-code-router 类）用 max_tokens=1 的
	// 小请求探测模型是否支持 tool calling。真实上游在 max_tokens=1 下永远只能返回
	// finish=length 且无 tool_calls，探测必然失败——客户端据此禁用工具或反复重试，
	// 且每个工具组都要跑一次真实上游（几十秒空白）。这里直接合成探测期望的
	// tool_calls 响应，瞬间通过。max_tokens<=2 的请求本身也得不到有意义输出，无副作用。
	if mt, _ := clientReq["max_tokens"].(float64); !probe.Stream && mt > 0 && mt <= 2 {
		if h.synthesizeProbeOpenAI(w, clientReq, model) {
			return
		}
	}

	// 请求级日志：定位客户端侧“无返回内容”类问题
	cMsgs, cTools := 0, 0
	if v, ok := clientReq["messages"].([]any); ok {
		cMsgs = len(v)
	}
	if v, ok := clientReq["tools"].([]any); ok {
		cTools = len(v)
	}
	log.Printf("chat: model=%q stream=%v msgs=%d tools=%d max_tokens=%v",
		model, probe.Stream, cMsgs, cTools, clientReq["max_tokens"])
	if os.Getenv("CB2A_TRACE_BODY") == "1" {
		logChatBody(clientReq)
	}

	payload, err := upstream.BuildChatPayload(clientReq, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "build payload: "+err.Error())
		return
	}

	rc, acct, status, ferr := h.forwardStream(r.Context(), payload, r.URL.Query().Get("uid"))
	if ferr != nil {
		code := "upstream_error"
		if status == http.StatusServiceUnavailable || status == http.StatusBadGateway {
			code = "no_healthy_account"
		}
		writeOpenAIError(w, status, code, ferr.Error())
		return
	}
	defer rc.Close()

	if probe.Stream {
		h.writeSSEHeader(w)
		flush := h.flusher(w)
		if err := upstream.StreamAsOpenAI(w, rc, model, flush, h.usageCallback(acct.UID)); err != nil {
			log.Printf("chat: stream aborted uid=%s model=%s: %v", acct.UID, model, err)
		}
		return
	}

	// 非流式：上游恒为流式，这里聚合成单个响应；偶发空工具响应自动换号重试
	resp, acctAgg, statusAgg, ferrAgg := h.aggregateWithRetry(r.Context(), payload, r.URL.Query().Get("uid"), model)
	if ferrAgg != nil {
		code := "upstream_error"
		if statusAgg == http.StatusServiceUnavailable || statusAgg == http.StatusBadGateway {
			code = "no_healthy_account"
		}
		writeOpenAIError(w, statusAgg, code, ferrAgg.Error())
		return
	}
	h.recordUsage(acctAgg.UID, resp)
	writeJSON(w, http.StatusOK, resp)
}

// forwardStream 把已构造好的上游请求体按账号池轮换发出，返回上游 SSE 流与命中的账号。
// 三套协议（/v1/chat/completions、/v1/messages、/v1/responses）共用这一份
// token 续期 / 错误分类 / 冷却禁用的逻辑。
// 失败时 err 非 nil，status 为建议回给客户端的 HTTP 状态码。
func (h *Handler) forwardStream(ctx context.Context, payload []byte, prefUID string) (io.ReadCloser, *cred.Cred, int, error) {
	if h.cfg.Pool.Pick() == nil {
		return nil, nil, http.StatusServiceUnavailable, errors.New("all accounts unavailable (cooling/disabled)")
	}

	// 指定账号（对话测试页 / 调试用）
	if prefUID != "" {
		acct := h.cfg.Pool.AuthByUID(prefUID)
		if acct == nil {
			return nil, nil, http.StatusNotFound, fmt.Errorf("account not found: %s", prefUID)
		}
		rlm := h.realmFor(acct)
		token, err := acct.EnsureToken(h.cfg.TokenURL, h.cfg.ClientID)
		if err != nil {
			if cred.IsAuthInvalid(err) {
				h.cfg.Pool.Disable(acct.UID, "auth invalid (re-login required)")
				return nil, nil, http.StatusUnauthorized, err
			}
			return nil, nil, http.StatusServiceUnavailable, err
		}
		rc, ferr := h.chatForwardWithRetry(ctx, rlm, token, payload)
		if ferr != nil {
			var ue *upstream.Error
			if errors.As(ferr, &ue) {
				if ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "credential rejected")
				} else if ue.Kind == upstream.ErrTokenExpired {
					acct.ExpiresAt = 0
				}
				return nil, nil, statusForKind(ue.Kind, ue.Status), ferr
			}
			return nil, nil, http.StatusBadGateway, ferr
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		return rc, acct, 0, nil
	}

	tried := map[string]bool{}
	var lastErr error
	lastStatus := 0
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		rlm := h.realmFor(acct)
		token, err := acct.EnsureToken(h.cfg.TokenURL, h.cfg.ClientID)
		if err != nil {
			lastErr = err
			lastStatus = http.StatusServiceUnavailable
			if cred.IsAuthInvalid(err) {
				h.cfg.Pool.Disable(acct.UID, "auth invalid (re-login required)")
			} else {
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "token ensure: "+err.Error())
			}
			continue
		}

		// 上游偶发 11128 "unapproved channel" 属瞬时风控抖动：带退避重试同一账号，
		// 让抖动自行恢复，而不是立刻把单账号判死（否则所有账号都中招时直接 503）。
		rc, ferr := h.chatForwardWithRetry(ctx, rlm, token, payload)
		if ferr != nil {
			err = ferr
		} else {
			err = nil
		}
		if err != nil {
			var ue *upstream.Error
			if errors.As(err, &ue) {
				lastStatus = statusForKind(ue.Kind, ue.Status)
				// 免费模型当日额度/频率用尽：不整号冷却（该账号其它模型照常可用），
				// 仅记录错误并换下一个账号重试本次请求。
				if upstream.IsDailyFreeLimit(ue) {
					lastErr = ue
					continue
				}
				switch ue.Kind {
				case upstream.ErrHardCredit:
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "配额/余额不足")
				case upstream.ErrSoftRate:
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				case upstream.ErrTokenExpired:
					acct.ExpiresAt = 0
					h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				case upstream.ErrSessionDead:
					h.cfg.Pool.Disable(acct.UID, "credential rejected")
				case upstream.ErrNotFound:
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
				case upstream.ErrClient:
					// 模型/通道不被该账号批准（如 11128 unapproved channel）：
					// 往往是该 realm 不支持当前模型，换成另一个账号/realm 重试通常能成功，
					// 因此继续轮询而非立刻失败。其余 4xx（参数错误等）仍直接返回。
					if upstream.IsModelChannelRejected(ue) {
						lastStatus = ue.Status
						if lastStatus == 0 {
							lastStatus = http.StatusBadRequest
						}
						lastErr = ue
						continue
					}
					// 其它无效请求：换账号重试没有意义，也不累计错误把账号池拖入冷却，
					// 立刻把错误交回调用方。
					return nil, nil, http.StatusBadRequest, err
				default:
					h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				}
				lastErr = ue
				continue
			}
			// 传输层错误（超时/连接失败）：通常不是凭证问题，只累计错误数
			h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		return rc, acct, 0, nil
	}

	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	if lastStatus == 0 {
		lastStatus = http.StatusServiceUnavailable
	}
	return nil, nil, lastStatus, errors.New(msg)
}

// aggregateWithRetry 聚合上游流为单个 OpenAI 响应；若命中偶发「空工具响应」
// （finish_reason=tool_calls 但 content/reasoning/tool_calls 全空），自动换号重试一次。
//
// 这类空响应原样下发时：OpenAI 客户端看到空消息；Anthropic 客户端（Claude Code）
// 会因 stop_reason=tool_use 而无 tool_use 块报 "The model's tool call could not be parsed"。
// 换号重试通常能拿到正常响应；重试失败则返回第一次结果兜底（Anthropic 侧另有降级保护）。
func (h *Handler) aggregateWithRetry(ctx context.Context, payload []byte, prefUID, model string) (map[string]any, *cred.Cred, int, error) {
	rc, acct, status, ferr := h.forwardStream(ctx, payload, prefUID)
	if ferr != nil {
		return nil, acct, status, ferr
	}
	defer rc.Close()
	resp, err := upstream.Aggregate(rc, model)
	if err != nil {
		return nil, acct, http.StatusBadGateway, err
	}
	logAggregatedChat(model, resp)
	if !upstream.IsEmptyToolResponse(resp) {
		return resp, acct, 0, nil
	}
	log.Printf("upstream: empty tool response (finish_reason=tool_calls, no content), retrying once with another account")
	rc2, acct2, _, ferr2 := h.forwardStream(ctx, payload, "")
	if ferr2 != nil {
		return resp, acct, 0, nil
	}
	defer rc2.Close()
	resp2, err2 := upstream.Aggregate(rc2, model)
	if err2 != nil {
		return resp, acct, 0, nil
	}
	return resp2, acct2, 0, nil
}

// synthesizeProbeOpenAI 对 max_tokens<=2 的非流式工具探测请求合成响应：
// 有 tools 时返回一个调用首个工具的 tool_calls，无 tools 时返回空消息。
// 返回 true 表示已写出响应，调用方直接 return。
func (h *Handler) synthesizeProbeOpenAI(w http.ResponseWriter, clientReq map[string]any, model string) bool {
	tools, _ := clientReq["tools"].([]any)
	name := ""
	if len(tools) > 0 {
		if t0, ok := tools[0].(map[string]any); ok {
			if fn, ok := t0["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": ""}
	finish := "stop"
	if name != "" {
		msg["tool_calls"] = []any{map[string]any{
			"id":       fmt.Sprintf("call_probe_%d", time.Now().UnixNano()),
			"type":     "function",
			"function": map[string]any{"name": name, "arguments": "{}"},
		}}
		finish = "tool_calls"
	}
	log.Printf("probe: max_tokens<=2, synthesize %s (model=%s tools=%d)", finish, model, len(tools))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-probe-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
		},
	})
	return true
}

// synthesizeProbeAnthropic 对 max_tokens<=2 的非流式工具探测请求合成 Anthropic
// 形态的响应（有 tools 返回 tool_use，否则空文本）。返回 true 表示已写出响应。
func (h *Handler) synthesizeProbeAnthropic(w http.ResponseWriter, req map[string]any, model string) bool {
	tools, _ := req["tools"].([]any)
	name := ""
	if len(tools) > 0 {
		if t0, ok := tools[0].(map[string]any); ok {
			name, _ = t0["name"].(string)
		}
	}
	content := []any{map[string]any{"type": "text", "text": ""}}
	stop := "end_turn"
	if name != "" {
		content = []any{map[string]any{
			"type":  "tool_use",
			"id":    fmt.Sprintf("toolu_probe_%d", time.Now().UnixNano()),
			"name":  name,
			"input": map[string]any{},
		}}
		stop = "tool_use"
	}
	log.Printf("probe: max_tokens<=2, synthesize anthropic %s (model=%s tools=%d)", stop, model, len(tools))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            fmt.Sprintf("msg_probe_%d", time.Now().UnixNano()),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	return true
}

// logChatBody 打印客户端请求体摘要（CB2A_TRACE_BODY=1 开启）。
//
// 排查「模型说没收到用户消息」这类问题时需要：看得到 roles、每条 content 的
// 真实形态（字符串 / 分片数组 / 分片里的 type），以及是否存在客户端把
// Responses 形态（input）误发到 chat 端点的情况。只打摘要，不打全文。
func logChatBody(req map[string]any) {
	msgs, ok := req["messages"].([]any)
	if !ok {
		keys := make([]string, 0, len(req))
		for k := range req {
			keys = append(keys, k)
		}
		log.Printf("TRACE BODY: no messages array (Responses 形态误发？) top-keys=%v", keys)
		if in, ok2 := req["input"]; ok2 {
			log.Printf("TRACE BODY: input field present, type=%T", in)
		}
		return
	}
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			log.Printf("TRACE MSG[%d] non-object %T", i, m)
			continue
		}
		role, _ := mm["role"].(string)
		desc := fmt.Sprintf("type=%T", mm["content"])
		switch c := mm["content"].(type) {
		case string:
			desc = fmt.Sprintf("string(%d)=%q", len(c), truncateForLog(c, 160))
		case []any:
			parts := make([]string, 0, len(c))
			for _, it := range c {
				im, _ := it.(map[string]any)
				if im == nil {
					parts = append(parts, fmt.Sprintf("%T", it))
					continue
				}
				t, _ := im["type"].(string)
				txt, _ := im["text"].(string)
				parts = append(parts, fmt.Sprintf("{%s len=%d %q}", t, len(txt), truncateForLog(txt, 80)))
			}
			desc = "parts=" + strings.Join(parts, ",")
		case nil:
			desc = "content=nil"
		}
		extra := ""
		if _, hasTC := mm["tool_calls"]; hasTC {
			extra += " has_tool_calls"
		}
		if _, hasTR := mm["tool_call_id"]; hasTR {
			extra += " has_tool_call_id"
		}
		log.Printf("TRACE MSG[%d] role=%s %s%s", i, role, desc, extra)
	}
}

// truncateForLog 按 rune 截断，避免长文本刷爆日志。
func truncateForLog(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// logAggregatedChat 打印聚合结果摘要（请求级日志，排查"无返回内容"类问题）。
func logAggregatedChat(model string, resp map[string]any) {
	ch, _ := resp["choices"].([]any)
	if len(ch) == 0 {
		log.Printf("chat aggregate: model=%s choices=0", model)
		return
	}
	c, _ := ch[0].(map[string]any)
	if c == nil {
		return
	}
	m, _ := c["message"].(map[string]any)
	fr, _ := c["finish_reason"].(string)
	cl, ntc := 0, 0
	if m != nil {
		if s, ok := m["content"].(string); ok {
			cl = len(s)
		}
		if t, ok := m["tool_calls"].([]map[string]any); ok {
			ntc = len(t)
		} else if t, ok := m["tool_calls"].([]any); ok {
			ntc = len(t)
		}
	}
	log.Printf("chat aggregate: model=%s finish=%q content_len=%d tool_calls=%d", model, fr, cl, ntc)
}

// chatForwardWithRetry 对单个账号尝试转发对话请求；当上游返回「未批准渠道」类瞬时安全拦截
// （11128 unapproved channel / illegal api invocation，网关侧偶发的风控/限流抖动）时，带递增退避
// 重试若干次，让抖动自行恢复，而不是立刻把单账号判死。其它错误（模型不支持、凭证失效、
// 传输层错误等）不重试、原样返回，交由 forwardStream 的既有分支处理。
func (h *Handler) chatForwardWithRetry(ctx context.Context, rlm RealmConfig, token string, payload []byte) (io.ReadCloser, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		rc, ferr := h.cfg.Upstream.ChatForwardRealm(ctx, rlm.Base, rlm.ChatPath, token, payload, rlm.Headers)
		if ferr == nil {
			return rc, nil
		}
		var ue *upstream.Error
		if !errors.As(ferr, &ue) {
			// 传输层错误（超时/连接失败）：不重试，交由外层按网络错误处理。
			return nil, ferr
		}
		lastErr = ue
		// 仅「未批准渠道」类瞬时安全拦截才退避重试；其余 4xx 是真错误，应立即上抛。
		if !upstream.IsTransientChannelBlock(ue) {
			return nil, ue
		}
		if attempt < maxAttempts-1 {
			backoff := time.Duration(attempt+1) * 700 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ue
			case <-time.After(backoff):
			}
		}
	}
	return nil, lastErr
}

// writeSSEHeader 设置流式响应公共头并发出 200。
func (h *Handler) writeSSEHeader(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// flusher 返回该响应 writer 的 flush 闭包（不支持时为空实现）。
func (h *Handler) flusher(w http.ResponseWriter) func() {
	fl, ok := w.(http.Flusher)
	if !ok {
		return func() {}
	}
	return fl.Flush
}

// ─────────────────────────────────────────────────────────────────────────────
// Anthropic Messages（Claude Code / CC Switch）
// ─────────────────────────────────────────────────────────────────────────────

// anthropicMessages 处理 POST /v1/messages：Anthropic 协议 ↔ 上游 chat 协议。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	_, req, ok := h.readJSON(w, r, anthropicBodyErr)
	if !ok {
		return
	}
	modelRaw, _ := req["model"].(string)
	if modelRaw == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	stream, _ := req["stream"].(bool)

	model, rewritten := upstream.MapModelName(modelRaw, h.cfg.FallbackModel)
	if rewritten {
		log.Printf("messages: model %q not supported by upstream, fallback to %q", modelRaw, model)
	}

	// 工具能力探测短路（同 chat 路径，Anthropic 形态）
	if mt, _ := req["max_tokens"].(float64); !stream && mt > 0 && mt <= 2 {
		if h.synthesizeProbeAnthropic(w, req, model) {
			return
		}
	}

	// 请求级日志：定位客户端侧“无返回内容”类问题
	nMsgs, nTools := 0, 0
	if v, ok := req["messages"].([]any); ok {
		nMsgs = len(v)
	}
	if v, ok := req["tools"].([]any); ok {
		nTools = len(v)
	}
	log.Printf("messages: model=%q stream=%v msgs=%d tools=%d max_tokens=%v thinking=%v system_len=%d",
		model, stream, nMsgs, nTools, req["max_tokens"], req["thinking"] != nil, len(upstream.AnthropicTextOf(req["system"])))

	chatReq, err := upstream.AnthropicToChat(req, model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	payload, err := upstream.BuildChatPayload(chatReq, model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "build payload: "+err.Error())
		return
	}

	rc, acct, status, ferr := h.forwardStream(r.Context(), payload, r.URL.Query().Get("uid"))
	if ferr != nil {
		writeAnthropicError(w, status, "api_error", ferr.Error())
		return
	}
	defer rc.Close()

	if stream {
		h.writeSSEHeader(w)
		flush := h.flusher(w)
		if err := upstream.StreamAsAnthropic(w, rc, model, chatReq["tools"], req["thinking"] != nil, flush, h.usageCallback(acct.UID)); err != nil {
			log.Printf("messages: stream aborted uid=%s model=%s: %v", acct.UID, model, err)
		}
		return
	}

	resp, acctAgg, statusAgg, ferrAgg := h.aggregateWithRetry(r.Context(), payload, r.URL.Query().Get("uid"), model)
	if ferrAgg != nil {
		writeAnthropicError(w, statusAgg, "api_error", ferrAgg.Error())
		return
	}
	h.recordUsage(acctAgg.UID, resp)
	writeJSON(w, http.StatusOK, upstream.AggregateAsAnthropic(resp, model, chatReq["tools"], req["thinking"] != nil))
}

// anthropicCountTokens 处理 POST /v1/messages/count_tokens。
// Claude Code 会用它估算上下文占用，没有真的计数接口，这里给出近似值。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	_, req, ok := h.readJSON(w, r, anthropicBodyErr)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": upstream.CountTokens(req)})
}

// ─────────────────────────────────────────────────────────────────────────────
// OpenAI Responses（Codex CLI）
// ─────────────────────────────────────────────────────────────────────────────

// responsesAPI 处理 POST /v1/responses：Responses 协议 ↔ 上游 chat 协议。
func (h *Handler) responsesAPI(w http.ResponseWriter, r *http.Request) {
	_, req, ok := h.readJSON(w, r, openAIBodyErr)
	if !ok {
		return
	}
	modelRaw, _ := req["model"].(string)
	if modelRaw == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	stream, _ := req["stream"].(bool)

	model, rewritten := upstream.MapModelName(modelRaw, h.cfg.FallbackModel)
	if rewritten {
		log.Printf("responses: model %q not supported by upstream, fallback to %q", modelRaw, model)
	}

	chatReq, err := upstream.ResponsesToChat(req, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	payload, err := upstream.BuildChatPayload(chatReq, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "build payload: "+err.Error())
		return
	}

	rc, acct, status, ferr := h.forwardStream(r.Context(), payload, r.URL.Query().Get("uid"))
	if ferr != nil {
		writeOpenAIError(w, status, "api_error", ferr.Error())
		return
	}
	defer rc.Close()

	if stream {
		h.writeSSEHeader(w)
		flush := h.flusher(w)
		if err := upstream.StreamAsResponses(w, rc, model, flush, h.usageCallback(acct.UID)); err != nil {
			log.Printf("responses: stream aborted uid=%s model=%s: %v", acct.UID, model, err)
		}
		return
	}

	resp, err := upstream.Aggregate(rc, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	h.recordUsage(acct.UID, resp)
	writeJSON(w, http.StatusOK, upstream.AggregateAsResponses(resp, model))
}

// bodyErrFunc 写「请求体读取/解析失败」响应，由调用方按协议传入。
type bodyErrFunc func(w http.ResponseWriter, status int, msg string)

// openAIBodyErr OpenAI 风格的错误响应（/v1/chat/completions、/v1/responses）。
func openAIBodyErr(w http.ResponseWriter, status int, msg string) {
	writeOpenAIError(w, status, "invalid_request", msg)
}

// anthropicBodyErr Anthropic 风格的错误响应（/v1/messages）。
func anthropicBodyErr(w http.ResponseWriter, status int, msg string) {
	writeAnthropicError(w, status, "invalid_request_error", msg)
}

// readJSON 读取并解析请求体；失败时用 errFn 写好错误响应并返回 ok=false。
func (h *Handler) readJSON(w http.ResponseWriter, r *http.Request, errFn bodyErrFunc) ([]byte, map[string]any, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		errFn(w, http.StatusBadRequest, "read body: "+err.Error())
		return nil, nil, false
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		errFn(w, http.StatusBadRequest, "parse json: "+err.Error())
		return nil, nil, false
	}
	return body, req, true
}

// recordUsage 从响应 usage 里提取 credit 并计入额度累计器。
func (h *Handler) recordUsage(uid string, resp map[string]any) {
	if h.cfg.Tracker == nil {
		return
	}
	u, _ := resp["usage"].(map[string]any)
	if credit := usage.ExtractCredit(u); credit > 0 {
		h.cfg.Tracker.Add(uid, credit)
	}
}

func (h *Handler) usageCallback(uid string) func(map[string]any) {
	return func(u map[string]any) {
		if h.cfg.Tracker == nil {
			return
		}
		if credit := usage.ExtractCredit(u); credit > 0 {
			h.cfg.Tracker.Add(uid, credit)
		}
	}
}

// apiQuotaAll 拉取池内所有账号的上游真实余额，并按账号聚合并给出总积分。
// 每个账号调用一次 FetchQuota（含 get-user-resource），累加各资源包的
// 本期/总量 已用与剩余；最终 total 为所有账号的实时总积分。
func (h *Handler) apiQuotaAll(w http.ResponseWriter, r *http.Request) {
	statuses := h.cfg.Pool.List()
	// 并发拉取：上游配额接口单次 ~2.2s，串行会随账号数线性变慢
	// （2 号 ~5s、4 号 ~10s）。改为每账号一个 goroutine 同时发，
	// 总耗时 ≈ 最慢的那一个；结果按原顺序写回，保持响应结构不变。
	rows := make([]map[string]any, len(statuses))
	total := map[string]any{
		"cycle_used":   0.0,
		"cycle_remain": 0.0,
		"total_used":   0.0,
		"total_remain": 0.0,
		"real":         false,
	}
	var wg sync.WaitGroup
	for i, st := range statuses {
		wg.Add(1)
		go func(i int, st pool.Status) {
			defer wg.Done()
			row := map[string]any{"uid": st.UID, "nickname": st.Nickname}
			acct := h.cfg.Pool.AuthByUID(st.UID)
			if acct == nil {
				row["ok"] = false
				row["error"] = "account not found"
				rows[i] = row
				return
			}
			row["realm"] = acct.Realm
			rlm := h.realmFor(acct)
			token, err := acct.EnsureToken(h.cfg.TokenURL, h.cfg.ClientID)
			if err != nil {
				if cred.IsAuthInvalid(err) {
					h.cfg.Pool.Disable(acct.UID, "auth invalid (re-login required)")
				}
				row["ok"] = false
				row["error"] = err.Error()
				rows[i] = row
				return
			}
			info, err := h.cfg.Upstream.FetchQuota(r.Context(), rlm.Base, token, rlm.Headers)
			if err != nil {
				row["ok"] = false
				row["error"] = err.Error()
				rows[i] = row
				return
			}
			row["payment_type"] = info.PaymentType
			res := info.Resource
			if res != nil && len(res.Accounts) > 0 {
				var cu, cr, tu, tr float64
				pkgs := make([]map[string]any, 0, len(res.Accounts))
				for _, a := range res.Accounts {
					cu += a.CycleCapacityUsed
					cr += a.CycleCapacityRemain
					tu += a.CapacityUsed
					tr += a.CapacityRemain
					pkgs = append(pkgs, map[string]any{
						"package":      a.PackageName,
						"cycle_used":   a.CycleCapacityUsed,
						"cycle_remain": a.CycleCapacityRemain,
						"total_used":   a.CapacityUsed,
						"total_remain": a.CapacityRemain,
					})
				}
				row["ok"] = true
				row["cycle_used"] = cu
				row["cycle_remain"] = cr
				row["total_used"] = tu
				row["total_remain"] = tr
				row["packages"] = pkgs
			} else {
				// 上游无真实余额接口：仅给出套餐信息，余额标记为估算缺失。
				row["ok"] = true
				row["estimated"] = true
			}
			rows[i] = row
		}(i, st)
	}
	wg.Wait()
	accounts := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		accounts = append(accounts, row)
		if ok, _ := row["ok"].(bool); ok {
			if cu, ok2 := row["cycle_used"].(float64); ok2 {
				total["cycle_used"] = total["cycle_used"].(float64) + cu
			}
			if cr, ok2 := row["cycle_remain"].(float64); ok2 {
				total["cycle_remain"] = total["cycle_remain"].(float64) + cr
			}
			if tu, ok2 := row["total_used"].(float64); ok2 {
				total["total_used"] = total["total_used"].(float64) + tu
			}
			if tr, ok2 := row["total_remain"].(float64); ok2 {
				total["total_remain"] = total["total_remain"].(float64) + tr
			}
			total["real"] = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accounts": accounts,
		"total":    total,
	})
}

// apiQuotaUsage 仅返回本地累计消耗快照（不访问上游）。
func (h *Handler) apiQuotaUsage(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tracker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tracker not initialized"})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Tracker.Snapshot())
}

// apiQuotaLimit 设置额度上限。
func (h *Handler) apiQuotaLimit(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tracker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tracker not initialized"})
		return
	}
	limitStr := r.URL.Query().Get("limit")
	var limit float64
	if _, err := fmt.Sscanf(limitStr, "%f", &limit); err != nil || limit <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid limit"})
		return
	}
	h.cfg.Tracker.SetLimit(limit)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "limit": limit})
}

// statusForKind 把上游错误映射给客户端的 HTTP 状态码。
func statusForKind(kind upstream.ErrKind, upstreamStatus int) int {
	switch kind {
	case upstream.ErrHardCredit:
		return http.StatusTooManyRequests
	case upstream.ErrSoftRate:
		return http.StatusTooManyRequests
	case upstream.ErrTokenExpired, upstream.ErrSessionDead:
		return http.StatusUnauthorized
	case upstream.ErrNotFound:
		return http.StatusNotFound
	default:
		if upstreamStatus >= 400 && upstreamStatus < 600 {
			return upstreamStatus
		}
		return http.StatusBadGateway
	}
}

// ---------------------------------------------------------------------------
// 管理与前端
// ---------------------------------------------------------------------------

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy := h.cfg.Pool.Count()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"accounts":      total,
		"healthy":       healthy,
		"has_api_key":   h.cfg.APIKey != "",
		"upstream_base": h.cfg.Upstream.Base,
	})
}

func (h *Handler) apiStatus(w http.ResponseWriter, r *http.Request) {
	total, healthy := h.cfg.Pool.Count()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":      h.cfg.Pool.List(),
		"total":         total,
		"healthy":       healthy,
		"has_api_key":   h.cfg.APIKey != "",
		"upstream_base": h.cfg.Upstream.Base,
		"auth_dir":      h.cfg.AuthDir,
	})
}

func (h *Handler) apiAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	if h.cfg.OnAdd == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "add not configured"})
		return
	}
	var req struct {
		APIKey    string `json:"api_key"`
		Token     string `json:"token"`
		UID       string `json:"uid"`
		Nickname  string `json:"nickname"`
		Refresh   string `json:"refresh_token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	tok := strings.TrimSpace(firstNonEmpty(req.APIKey, req.Token))
	if tok == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "api_key or token is required"})
		return
	}
	c := &cred.Cred{
		UID:          strings.TrimSpace(req.UID),
		Nickname:     strings.TrimSpace(req.Nickname),
		Token:        tok,
		Kind:         cred.ClassifyKind(tok),
		RefreshToken: req.Refresh,
		ExpiresAt:    req.ExpiresAt,
	}
	if c.UID == "" {
		c.UID = "k" + sanitizeUID(tok)
	}
	if c.Nickname == "" {
		c.Nickname = c.UID
	}
	path, err := h.cfg.OnAdd(c)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	h.cfg.Pool.Add(c)
	// 添加后立刻登录测试：确保 token 可用并拉取模型列表，结果写回账号状态。
	test := h.probeCred(c)
	if !test.ok {
		if cred.IsAuthInvalid(test.err) {
			h.cfg.Pool.Disable(c.UID, "登录失败: "+test.err.Error())
		} else {
			h.cfg.Pool.MarkProbed(c.UID, false, 0, "连接失败: "+test.err.Error())
		}
		h.cfg.Pool.SaveState()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":   true,
			"uid":  c.UID,
			"path": path,
			"test": map[string]any{"ok": false, "error": test.err.Error()},
		})
		return
	}
	h.cfg.Pool.MarkProbed(c.UID, true, test.models, "")
	// 刷新全局模型缓存（让 /v1/models 立刻可用）
	h.refreshModels(c)
	h.cfg.Pool.SaveState()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"uid":  c.UID,
		"path": path,
		"test": map[string]any{"ok": true, "models": test.models},
	})
}

// probeResult 连接探测结果。
type probeResult struct {
	ok     bool
	models int
	err    error
}

// probeCred 真实登录测试：续期 token + 拉取模型列表（按账号 realm 选上游）。
// 部分域（如中国版 CN）没有公开模型列表接口（/v2/models 404、/v3/config 的 models 为 null），
// 此时回退静态表，并用一次最小对话请求确认 token 确实可用，避免把有效账号判失败。
func (h *Handler) probeCred(c *cred.Cred) probeResult {
	token, err := c.EnsureToken(h.cfg.TokenURL, h.cfg.ClientID)
	if err != nil {
		return probeResult{err: err}
	}
	rlm := h.realmFor(c)
	ms, _ := h.cfg.Upstream.FetchModelsRealm(context.Background(), rlm.Base, token, rlm.Headers, rlm.ModelsPaths...)
	if len(ms) > 0 {
		return probeResult{ok: true, models: len(ms)}
	}
	// 上游无公开模型列表：回退静态表，并用最小对话请求验证 token 可用。
	ms = upstream.FallbackModels()
	if err := h.cfg.Upstream.ProbeRealm(context.Background(), rlm.Base, rlm.ChatPath, token, rlm.Headers); err != nil {
		return probeResult{err: err, models: len(ms)}
	}
	return probeResult{ok: true, models: len(ms)}
}

// refreshModels 用某账号的 token 刷新全局模型缓存（按 realm 选上游）。
func (h *Handler) refreshModels(c *cred.Cred) {
	acct := h.cfg.Pool.AuthByUID(c.UID)
	if acct == nil {
		return
	}
	rlm := h.realmFor(acct)
	token, err := acct.EnsureToken(h.cfg.TokenURL, h.cfg.ClientID)
	if err != nil {
		return
	}
	ms, err := h.cfg.Upstream.FetchModelsRealm(context.Background(), rlm.Base, token, rlm.Headers, rlm.ModelsPaths...)
	if err != nil || len(ms) == 0 {
		return
	}
	h.storeModels(ms)
}

func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey   string `json:"api_key"`
		Token    string `json:"token"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	tok := strings.TrimSpace(firstNonEmpty(req.APIKey, req.Token))
	if tok == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "api_key or token is required"})
		return
	}
	c := &cred.Cred{
		UID:      strings.TrimSpace(req.UID),
		Nickname: strings.TrimSpace(req.Nickname),
		Token:    tok,
		Kind:     cred.ClassifyKind(tok),
	}
	if c.UID == "" {
		c.UID = "k" + sanitizeUID(tok)
	}
	if c.Nickname == "" {
		c.Nickname = c.UID
	}
	res := h.probeCred(c)
	if !res.ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "kind": string(c.Kind),
			"uid": c.UID, "error": res.err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "kind": string(c.Kind),
		"uid": c.UID, "models": res.models,
	})
}

func (h *Handler) apiRetest(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uid is required"})
		return
	}
	c := h.cfg.Pool.AuthByUID(uid)
	if c == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}
	res := h.probeCred(c)
	if !res.ok {
		if cred.IsAuthInvalid(res.err) {
			h.cfg.Pool.Disable(uid, "登录失败: "+res.err.Error())
		} else {
			h.cfg.Pool.MarkProbed(uid, false, 0, "连接失败: "+res.err.Error())
		}
		h.cfg.Pool.SaveState()
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": res.err.Error()})
		return
	}
	h.cfg.Pool.MarkProbed(uid, true, res.models, "")
	h.refreshModels(c)
	h.cfg.Pool.SaveState()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": res.models})
}

// apiAuthStart 启动一次 CodeBuddy OAuth 设备授权会话，返回登录页 URL 与轮询 state。
// realm 可选：saas=国际版(www.codebuddy.ai) / cn=中国版(copilot.tencent.com)。
func (h *Handler) apiAuthStart(w http.ResponseWriter, r *http.Request) {
	realm := r.URL.Query().Get("realm")
	if realm == "" {
		realm = "saas"
	}
	prov := authcb.ProviderByRealm(realm)
	res, err := authcb.StartLogin(prov)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "启动登录失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth_url": res.AuthURL, "state": res.State, "realm": realm})
}

// apiAuthPoll 用 state 轮询授权结果；成功后自动保存凭证、登录测试并刷新模型。
func (h *Handler) apiAuthPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
		Realm string `json:"realm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.State == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state is required"})
		return
	}
	if req.Realm == "" {
		req.Realm = "saas"
	}
	prov := authcb.ProviderByRealm(req.Realm)
	pr, err := authcb.Poll(prov, req.State)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "轮询失败: " + err.Error()})
		return
	}
	if pr.Pending {
		writeJSON(w, http.StatusOK, map[string]any{"pending": true})
		return
	}
	tr := pr.Token
	uid := authcb.UserIDFromJWT(tr.AccessToken)
	realmTag := req.Realm
	if uid == "" {
		uid = realmTag + "-" + sanitizeUID(tr.AccessToken)
	}
	c := &cred.Cred{
		UID:          uid,
		Nickname:     uid,
		Realm:        realmTag,
		Token:        tr.AccessToken,
		Kind:         cred.KindToken,
		RefreshToken: tr.RefreshToken,
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	path, err := h.cfg.OnAdd(c)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存凭证失败: " + err.Error()})
		return
	}
	h.cfg.Pool.Add(c)

	res := h.probeCred(c)
	if !res.ok {
		if cred.IsAuthInvalid(res.err) {
			h.cfg.Pool.Disable(c.UID, "登录失败: "+res.err.Error())
		} else {
			h.cfg.Pool.MarkProbed(c.UID, false, 0, "连接失败: "+res.err.Error())
		}
		h.cfg.Pool.SaveState()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "uid": c.UID, "path": path,
			"realm": realmTag, "error": res.err.Error(),
		})
		return
	}
	h.cfg.Pool.MarkProbed(c.UID, true, res.models, "")
	h.refreshModels(c)
	h.cfg.Pool.SaveState()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": c.UID, "path": path,
		"realm": realmTag, "models": res.models,
	})
}

func (h *Handler) apiReload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.OnReload == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "reload not configured"})
		return
	}
	n, err := h.cfg.OnReload()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	h.modelsMu.Lock()
	h.fetchedAt = time.Time{}
	h.modelsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": n})
}

func (h *Handler) apiEnable(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uid is required"})
		return
	}
	if !h.cfg.Pool.Enable(uid) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

// apiDisable 手动禁用某账号（管理 API）。reason 缺省「手动禁用」。
func (h *Handler) apiDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uid is required"})
		return
	}
	if h.cfg.Pool.AuthByUID(uid) == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "手动禁用"
	}
	h.cfg.Pool.Disable(uid, reason)
	h.cfg.Pool.SaveState()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "reason": reason})
}

func (h *Handler) apiRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uid is required"})
		return
	}
	if h.cfg.OnRemove != nil {
		if err := h.cfg.OnRemove(uid); err != nil {
			log.Printf("remove %s: file cleanup failed: %v", uid, err)
		}
	}
	if !h.cfg.Pool.Remove(uid) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

func (h *Handler) serveAdmin(w http.ResponseWriter, r *http.Request) {
	data, err := adminHTML.ReadFile("admin.html")
	if err != nil {
		http.Error(w, "admin page not found", http.StatusNotFound)
		return
	}
	html := strings.Replace(string(data), "/*__CONFIG__*/",
		fmt.Sprintf("window.__API_KEY__ = %q; window.__HAS_AUTH__ = %v;",
			h.cfg.APIKey, h.cfg.APIKey != ""), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeAnthropicError 按 Anthropic 的错误结构写响应：
// {"type":"error","error":{"type":...,"message":...}}
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	})
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errorType(status),
			"code":    code,
		},
	})
}

func errorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "api_error"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// sanitizeUID 从 token 生成稳定短 ID（用户未指定 uid 时）。
func sanitizeUID(tok string) string {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(tok); i++ {
		h ^= uint64(tok[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%013x", h)
}
