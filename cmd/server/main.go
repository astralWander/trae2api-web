// main.go trae2api-web 入口：加载配置 → 构建 pool → 起 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/dotenv"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/scheduler"
	"trae2api-web/internal/server"
	"trae2api-web/internal/upstream"
	"trae2api-web/internal/version"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	log.Printf("trae2api-web build=%s (核对: git rev-parse --short HEAD)", version.Badge())

	// 必须在 Load 之前：配置项全部来自环境变量，先让 .env 补上。
	loadDotEnv()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	p := pool.New(cfg.StateFile)
	p.SyncToDir(auths) // 对齐：剔除 state.json 中已删除 auth 文件的幽灵账号

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 流式客户端无总超时，仅用首字节兜底（时长由 SSE 流本身决定）。
	if tr, ok := up.StreamHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	}

	sch := scheduler.New(scheduler.Config{
		Pool:         p,
		Upstream:     up,
		CheckinHour:  cfg.Schedule.CheckinHour,
		RefreshHours: cfg.Schedule.RefreshHours,
		RefreshSkew:  24 * time.Hour,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		AuthDir:      cfg.AuthDir,
		PlanCooldown: cfg.PlanCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		DefaultModel: cfg.DefaultModel,
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

	// 第二个 http.Server：监听 CallbackPort（默认 18080），只处理 /authorize 回调。
	// 复用同一 Handler（/authorize 已在主 mux 注册）。
	// cfg.CallbackPort == "0" 时不启动（纯手动粘贴模式）。
	var cbSrv *http.Server
	if cfg.CallbackPort != "" && cfg.CallbackPort != "0" {
		cbSrv = &http.Server{
			Addr:              "127.0.0.1:" + cfg.CallbackPort,
			Handler:           h,
			ReadHeaderTimeout: 30 * time.Second,
		}
		go func() {
			<-ctx.Done()
			sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = cbSrv.Shutdown(sc)
		}()
		go func() {
			log.Printf("trae2api-web callback server on 127.0.0.1:%s (TRAE login /authorize)", cfg.CallbackPort)
			if err := cbSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// 端口被占用（login.sh / 旧实例）不致命，降级为手动粘贴模式。
				log.Printf("callback server (:%s) failed: %v — web 登录降级为手动粘贴回调链接", cfg.CallbackPort, err)
			}
		}()
	}

	log.Printf("trae2api-web %s listening on %s (api_key=%v)", version.ID(), cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// loadDotEnv 把 .env 注入进程环境变量，必须在 Load 之前调用。
//
// 为什么需要：本服务端的配置项只从环境变量读取（见 config.go 的 applyEnv），
// 而 .env 文件只有 docker compose 会自动读 —— 二进制 / nohup / systemd 部署时
// 它形同废纸，导致 TW2A_API_KEY 恒为空、/admin/api/accounts/export 一直 403。
//
// 开关：
//   - TW2A_NO_DOTENV 设为任意非空值 → 关闭本次加载；
//   - TW2A_ENV_FILE 指定文件名或绝对路径（默认 .env，相对当前工作目录）。
//
// 优先级：进程里【已有】的环境变量更高，绝不会被 .env 覆盖 ——
// 这样 `export TW2A_API_KEY=xxx ./trae2api-web`、systemd 的 Environment=、
// docker 的 -e 永远说了算，.env 只是「没别的来源时」的兜底。
func loadDotEnv() {
	if os.Getenv("TW2A_NO_DOTENV") != "" {
		log.Printf("dotenv: 已按 TW2A_NO_DOTENV 关闭 .env 加载")
		return
	}
	path := os.Getenv("TW2A_ENV_FILE")
	if path == "" {
		path = dotenv.DefaultFile
	}
	if _, err := os.Stat(path); err != nil {
		log.Printf("dotenv: 未找到 %s（跳过；可用 TW2A_ENV_FILE 指定路径）", path)
		return
	}
	res, err := dotenv.Load(path)
	if err != nil {
		log.Printf("dotenv: 读取 %s 失败: %v", path, err)
		return
	}
	if len(res.Loaded) > 0 {
		log.Printf("dotenv: %s 注入 %d 项: %s", res.Path, len(res.Loaded), strings.Join(res.Loaded, ", "))
	}
	if len(res.Kept) > 0 {
		log.Printf("dotenv: %d 项因环境变量已存在而跳过（env 优先）: %s",
			len(res.Kept), strings.Join(res.Kept, ", "))
	}
	if len(res.Invalid) > 0 {
		log.Printf("dotenv: %s 有 %d 行无法解析，已跳过: %s",
			res.Path, len(res.Invalid), strings.Join(res.Invalid, "; "))
	}
}
