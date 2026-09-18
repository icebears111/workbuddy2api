// main.go codebuddy2api 入口：加载配置、构建账号池、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"codebuddy2api/internal/cred"
	"codebuddy2api/internal/pool"
	"codebuddy2api/internal/scheduler"
	"codebuddy2api/internal/server"
	"codebuddy2api/internal/upstream"
	"codebuddy2api/internal/usage"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults + env", *cfgPath)
			if cfg, err = Load(""); err != nil {
				log.Fatalf("load config: %v", err)
			}
		} else {
			log.Fatalf("load config: %v", err)
		}
	}

	absAuthDir, _ := filepath.Abs(cfg.AuthDir)
	absStateFile, _ := filepath.Abs(cfg.StateFile)
	absUsageFile, _ := filepath.Abs(cfg.UsageFile)

	// 加载凭证
	creds, err := cred.LoadDir(absAuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d credential(s) from %s", len(creds), absAuthDir)
	if len(creds) == 0 {
		log.Printf("warning: no credentials yet — add one via the admin UI (/admin) or by dropping a json file into %s", absAuthDir)
	}

	// 账号池
	p := pool.New(absStateFile)
	for _, c := range creds {
		p.Add(c)
	}
	p.SaveState()

	// 上游客户端
	up := upstream.NewWithTimeout(time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second)
	up.Base = cfg.Upstream.Base

	// 内容审核脱敏：缓解 Claude Code / Codex CLI 合规模板被后端误判敏感词
	// 导致整条请求被拦（http 400 / code 11128）。
	upstream.SetDesensitize(cfg.Features.Desensitize, cfg.Features.DesensitizeTools, cfg.Features.StripToolMetadata)
	// 流式响应剥离 reasoning_content（非标准 OpenAI 字段，协议转换层不兼容）。
	upstream.SetHideReasoningStream(cfg.Features.HideReasoning)
	// 思考深度下限：防止协议转换层把客户端思考档位压成 low/off。
	upstream.SetMinReasoningEffort(cfg.Features.MinReasoningEffort)
	if cfg.Features.MinReasoningEffort != "" {
		log.Printf("min reasoning effort: %s", cfg.Features.MinReasoningEffort)
	}

	// 调度器
	sch := scheduler.New(scheduler.Config{
		Pool:             p,
		Upstream:         up,
		AuthDir:          absAuthDir,
		CheckHours:       cfg.Schedule.CheckHours,
		AutoCheckin:      cfg.Schedule.AutoCheckin,
		CheckinStartHour: cfg.Schedule.CheckinStartHour,
		CheckinEndHour:   cfg.Schedule.CheckinEndHour,
		TokenURL:         cfg.OAuth.TokenURL,
		ClientID:         cfg.OAuth.ClientID,
	})

	// 管理回调：凭证文件与内存池双向同步
	onReload := func() (int, error) {
		newCreds, err := cred.LoadDir(absAuthDir)
		if err != nil {
			return 0, err
		}
		p.SyncToDir(newCreds)
		p.SaveState()
		log.Printf("reload: %d credential(s)", len(newCreds))
		return len(newCreds), nil
	}
	onAdd := func(c *cred.Cred) (string, error) {
		return cred.SaveFile(absAuthDir, c)
	}
	onRemove := func(uid string) error {
		files, err := filepath.Glob(filepath.Join(absAuthDir, "*.json"))
		if err != nil {
			return err
		}
		for _, f := range files {
			c, err := cred.LoadFile(f)
			if err != nil || c.UID != uid {
				continue
			}
			if err := os.Remove(f); err != nil {
				return err
			}
			return nil
		}
		return nil
	}

	h := server.NewHandler(server.Config{
		Pool:                    p,
		Upstream:                up,
		APIKey:                  cfg.APIKey,
		HardCooldown:            cfg.HardCreditDur,
		SoftCooldown:            cfg.SoftRateDur,
		ErrThreshold:            cfg.Cooldown.ErrThresh,
		ErrCooldown:             cfg.ErrCooldownDur,
		FallbackModel:           cfg.FallbackModel,
		EnableAnthropicProtocol: cfg.Features.EnableAnthropicProtocol,
		EnableResponsesProtocol: cfg.Features.EnableResponsesProtocol,
		AuthDir:                 absAuthDir,
		OnReload:                onReload,
		OnAdd:                   onAdd,
		OnRemove:                onRemove,
		TokenURL:                cfg.OAuth.TokenURL,
		ClientID:                cfg.OAuth.ClientID,
		Tracker:                 usage.New(absUsageFile, cfg.QuotaLimit),
		Realms: map[string]server.RealmConfig{
			// CN 控制台域（ck_ Key），沿用全局上游。
			"cn": {
				Base:        cfg.Upstream.Base,
				ChatPath:    upstream.ChatPath,
				ModelsPaths: []string{upstream.ModelsPathV2, upstream.ConfigPathV3},
				Headers:     nil,
			},
			// SaaS 域（OAuth 登录所得 token），走 www.codebuddy.ai。
			// 实测 chat 路由为 /v2/chat/completions（/v1 返回 404，/v2 仅支持 stream）。
			"saas": {
				Base:        "https://www.codebuddy.ai",
				ChatPath:    "/v2/chat/completions",
				ModelsPaths: []string{"/v2/models", upstream.ConfigPathV3},
				Headers: map[string]string{
					"X-Domain":       "www.codebuddy.ai",
					"X-Product":      "SaaS",
					"X-IDE-Type":     "CLI",
					"X-Agent-Intent": "craft",
				},
			},
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("codebuddy2api listening on %s (api_key=%v, upstream=%s)", cfg.Listen, cfg.APIKey != "", up.Base)
	log.Printf("desensitize: enabled=%v tools=%v strip_tool_metadata=%v",
		cfg.Features.Desensitize, cfg.Features.DesensitizeTools, cfg.Features.StripToolMetadata)
	log.Printf("protocols: anthropic=%v responses=%v (false = 410 Gone, 需转换层)",
		cfg.Features.EnableAnthropicProtocol, cfg.Features.EnableResponsesProtocol)
	log.Printf("admin UI: http://localhost%s/admin", normalizeListenForLog(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Print("bye")
}

// normalizeListenForLog 把 ":7865" 变成用于打印的 host:port。
func normalizeListenForLog(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	return listen
}
