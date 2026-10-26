// lovart2api 入口：加载配置、初始化签名 wasm 与账号池、起 HTTP 服务。
//
// Lovart 的图生图/文生图走 lgw 网关（需要 wasm 签名），本项目把它包成 OpenAI 的
// /v1/images/generations 与 /v1/images/edits。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yuanjie-ai/lovart2api/internal/config"
	"github.com/yuanjie-ai/lovart2api/internal/pool"
	"github.com/yuanjie-ai/lovart2api/internal/server"
	"github.com/yuanjie-ai/lovart2api/internal/sign"
)

// appVersion 由发版流水线通过 -X main.appVersion=<tag> 注入；
// 必须是 var（ldflags -X 只对字符串变量生效，const 注入不进去）。
var appVersion = "dev"

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	dumpExample := flag.Bool("example", false, "打印示例配置后退出")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("lovart2api", appVersion)
		return
	}
	if *dumpExample {
		b, _ := json.MarshalIndent(config.Default(), "", "  ")
		fmt.Println(string(b))
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if _, statErr := os.Stat(*cfgPath); errors.Is(statErr, os.ErrNotExist) {
		// 首次启动落一份完整配置，方便用户改
		if b, mErr := json.MarshalIndent(cfg, "", "  "); mErr == nil {
			_ = os.WriteFile(*cfgPath, b, 0o600)
			log.Printf("已生成示例配置: %s", *cfgPath)
		}
	}
	if err := config.EnsureDir(cfg.StateFile); err != nil {
		log.Fatalf("创建状态目录失败: %v", err)
	}

	ctx := context.Background()
	sg, err := sign.New(ctx)
	if err != nil {
		log.Fatalf("初始化签名 wasm 失败: %v", err)
	}
	defer sg.Close(ctx)

	p, err := pool.New(pool.Options{
		StateFile:     cfg.StateFile,
		MaxPerAccount: cfg.Pool.MaxConcurrencyPerAccount,
		QuotaCooldown: config.Duration(cfg.Pool.QuotaCooldown, 6*time.Hour),
		ErrorCooldown: config.Duration(cfg.Pool.ErrorCooldown, 90*time.Second),
		DailyLimit:    cfg.Pool.DailyLimit, // 0 = 不本地拦，靠上游额度错误兜底
	})
	if err != nil {
		log.Fatalf("初始化账号池失败: %v", err)
	}

	srv := server.New(cfg, p, sg)
	srv.StartBackground(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 图像生成是长任务，写超时要放宽
		WriteTimeout: 20 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("lovart2api %s 监听 %s（面板 /panel，接口 /v1/images/*）", appVersion, cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务退出: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("收到退出信号，正在关闭…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	_ = p.Save()
}
