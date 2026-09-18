// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen     string  `json:"listen"`
	APIKey     string  `json:"api_key"`
	AuthDir    string  `json:"auth_dir"`
	StateFile  string  `json:"state_file"`
	UsageFile  string  `json:"usage_file"`
	QuotaLimit float64 `json:"quota_limit"`
	// FallbackModel 客户端传入上游不存在的模型（如 claude-* / gpt-*）时回落到该模型。
	FallbackModel string `json:"fallback_model"`

	Upstream struct {
		// Base 上游根地址，留空时按 Env 自动推导。
		Base           string `json:"base"`
		Env            string `json:"env"` // internal | public | ioa | cloudhosted | selfhosted
		TimeoutSeconds int    `json:"timeout_seconds"`
	} `json:"upstream"`

	OAuth struct {
		// TokenURL OAuth2 换 token 的地址（仅 refresh_token 续期时需要）。
		TokenURL string `json:"token_url"`
		ClientID string `json:"client_id"`
	} `json:"oauth"`

	Cooldown struct {
		HardCredit  string `json:"hard_credit"`
		SoftRate    string `json:"soft_rate"`
		ErrThresh   int    `json:"err_threshold"`
		ErrCooldown string `json:"err_cooldown"`
	} `json:"cooldown"`

	Features struct {
		// Desensitize 对发往上游的请求做脱敏（零宽处理敏感词 + 压缩 harness
		// 运行时块），缓解 Claude Code / Codex CLI 合规模板被后端内容审核误拦
		// （http 400 / code 11128 "Illegal API invocation ..."）。默认开启。
		Desensitize bool `json:"desensitize"`
		// DesensitizeTools 是否对 tools 的 description / title 做脱敏。
		DesensitizeTools bool `json:"desensitize_tools"`
		// StripToolMetadata 为 true 时直接删除 description / title 字段，
		// 而不是做零宽替换。过审率更高，但模型看不到工具/参数说明。
		StripToolMetadata bool `json:"strip_tool_metadata"`
		// HideReasoning 流式响应剥离 reasoning_content（DeepSeek 系私有扩展，
		// 非标准 OpenAI 字段）。经协议转换层（CC Switch 等）接入的客户端对
		// 未知字段不兼容，默认剥离，保证响应为标准 OpenAI 流。
		HideReasoning bool `json:"hide_reasoning"`
		// MinReasoningEffort 思考深度下限（low/medium/high/max）。CC Switch chat
		// 模式会把 Claude 桌面端的思考设置映射成 reasoning_effort，未开扩展思考
		// 时发 low（上游直接关闭思考）。设置后低于下限的档位抬到下限、缺省补下限；
		// 留空保持客户端原值透传。
		MinReasoningEffort string `json:"min_reasoning_effort"`

		// EnableAnthropicProtocol 重新开放 POST /v1/messages 与
		// POST /v1/messages/count_tokens（Anthropic Messages 协议）。
		//
		// 默认 false：保持 README「本网关只对外提供 OpenAI Chat」的对外契约，
		// 三个非 OpenAI 端点一律 410 Gone + 迁移指引。
		// 置 true 后 Claude Code / Claude Desktop 可**直连本网关**，不再需要
		// CC Switch / ccr / LiteLLM 之类的中间转换层 —— 这样「两个终端各指一个
		// 网关实例」就能把账号池彻底隔离开（见 docs/deploy-nas.md 第五节）。
		EnableAnthropicProtocol bool `json:"enable_anthropic_protocol"`
		// EnableResponsesProtocol 重新开放 POST /v1/responses（OpenAI Responses
		// 协议，Codex CLI 用）。默认 false，理由同上。
		EnableResponsesProtocol bool `json:"enable_responses_protocol"`
	} `json:"features"`

	Schedule struct {
		CheckHours       []int `json:"check_hours"`        // 保活探活整点
		AutoCheckin      bool  `json:"auto_checkin"`       // 是否开启每日自动签到
		CheckinStartHour int   `json:"checkin_start_hour"` // 自动签到窗口起点（本地时区，含）
		CheckinEndHour   int   `json:"checkin_end_hour"`   // 自动签到窗口终点（本地时区，不含）
	} `json:"schedule"`

	HardCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:     ":7865",
		APIKey:     "",
		AuthDir:    "./auths",
		StateFile:  "./data/state.json",
		UsageFile:  "./data/usage.json",
		QuotaLimit: 500,
	}
	c.FallbackModel = "hy4-preview"
	c.Upstream.Env = "internal"
	c.Upstream.TimeoutSeconds = 180
	c.Cooldown.HardCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 5
	c.Cooldown.ErrCooldown = "10m"
	c.Features.Desensitize = true
	c.Features.DesensitizeTools = true
	c.Features.StripToolMetadata = false
	c.Features.HideReasoning = true
	c.Schedule.CheckHours = []int{0, 6, 12, 18}
	c.Schedule.AutoCheckin = true
	c.Schedule.CheckinStartHour = 8
	c.Schedule.CheckinEndHour = 10
	return c
}

// Load 从文件读（path 为空表示纯默认值），再用 CB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("CB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("CB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("CB2A_FALLBACK_MODEL"); v != "" {
		c.FallbackModel = v
	}
	if v := os.Getenv("CB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("CB2A_UPSTREAM_BASE"); v != "" {
		c.Upstream.Base = v
	}
	if v := os.Getenv("CB2A_UPSTREAM_ENV"); v != "" {
		c.Upstream.Env = v
	}
	if v := os.Getenv("CB2A_OAUTH_TOKEN_URL"); v != "" {
		c.OAuth.TokenURL = v
	}
	if v := os.Getenv("CB2A_OAUTH_CLIENT_ID"); v != "" {
		c.OAuth.ClientID = v
	}
	if v := os.Getenv("CB2A_HARD_CREDIT"); v != "" {
		c.Cooldown.HardCredit = v
	}
	if v := os.Getenv("CB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("CB2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("CB2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("CB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("CB2A_DESENSITIZE"); v != "" {
		c.Features.Desensitize = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("CB2A_DESENSITIZE_TOOLS"); v != "" {
		c.Features.DesensitizeTools = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("CB2A_STRIP_TOOL_METADATA"); v != "" {
		c.Features.StripToolMetadata = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("CB2A_HIDE_REASONING"); v != "" {
		c.Features.HideReasoning = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("CB2A_MIN_REASONING_EFFORT"); v != "" {
		c.Features.MinReasoningEffort = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("CB2A_ENABLE_ANTHROPIC_PROTOCOL"); v != "" {
		c.Features.EnableAnthropicProtocol = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("CB2A_ENABLE_RESPONSES_PROTOCOL"); v != "" {
		c.Features.EnableResponsesProtocol = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("CB2A_AUTO_CHECKIN"); v != "" {
		c.Schedule.AutoCheckin = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("CB2A_CHECKIN_START_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.CheckinStartHour = n
		}
	}
	if v := os.Getenv("CB2A_CHECKIN_END_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.CheckinEndHour = n
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.HardCreditDur, err = time.ParseDuration(c.Cooldown.HardCredit); err != nil {
		return fmt.Errorf("cooldown.hard_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 5
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 180
	}
	if c.Upstream.Base == "" {
		c.Upstream.Base = baseForEnv(c.Upstream.Env)
	}
	c.Upstream.Base = strings.TrimRight(c.Upstream.Base, "/")
	if c.Listen == "" {
		c.Listen = ":7865"
	}
	if !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "./data/state.json"
	}
	if c.UsageFile == "" {
		c.UsageFile = "./data/usage.json"
	}
	if c.QuotaLimit <= 0 {
		c.QuotaLimit = 500
	}
	// 签到窗口兜底：默认 8-10；非法区间纠正
	if c.Schedule.CheckinStartHour < 0 {
		c.Schedule.CheckinStartHour = 8
	}
	if c.Schedule.CheckinEndHour <= c.Schedule.CheckinStartHour {
		c.Schedule.CheckinEndHour = c.Schedule.CheckinStartHour + 1
	}
	if c.Schedule.CheckinEndHour > 24 {
		c.Schedule.CheckinEndHour = 24
	}
	if c.Schedule.CheckinStartHour >= 24 {
		c.Schedule.CheckinStartHour = 23
		c.Schedule.CheckinEndHour = 24
	}
	return nil
}

// baseForEnv 按 CODEBUDDY_INTERNET_ENVIRONMENT 语义推导上游地址。
func baseForEnv(env string) string {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "ioa":
		return "https://tencent.sso.copilot.tencent.com"
	case "public", "", "overseas":
		return "https://www.codebuddy.ai"
	case "cloudhosted", "selfhosted":
		// 企业自定义域名，必须显式配置 upstream.base
		return "https://copilot.tencent.com"
	default: // internal
		return "https://copilot.tencent.com"
	}
}
