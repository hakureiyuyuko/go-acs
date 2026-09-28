// Command acs 是轻量 TR-069 ACS 的入口。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"acs/internal/config"
	"acs/internal/cwmp"
	"acs/internal/store"
	"acs/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// 上次进程被杀时留下的 running 任务，启动时退回待办（NFR-6）
	if n, err := st.ResetRunningTasks(); err == nil && n > 0 {
		log.Info("上次中断的任务已退回待办", "count", n)
	}

	srv := cwmp.NewServer(st, cwmp.Config{
		Path:                cfg.Path,
		User:                cfg.User,
		Password:            cfg.Password,
		SessionTimeout:      cfg.SessionTimeout,
		AutoFetchDeviceInfo: cfg.AutoFetchInfo,
		MaxBodyBytes:        cfg.MaxBodyBytes,
		LogRawSOAP:          cfg.LogRawSOAP,
		OfflineAfter:        cfg.OfflineAfter,
	}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.StartJanitor(ctx)

	mux := http.NewServeMux()
	// 用 "/acs" 精确匹配（末尾不带 / 时只匹配该路径本身），这样别的路径不会被吞掉
	mux.Handle(cfg.Path, srv)
	if err := web.Register(mux, st, srv); err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("ACS 已启动",
		"listen", cfg.Listen,
		"cwmp_endpoint", cfg.Path,
		"ui", "http://"+uiAddr(cfg.Listen),
		"db", cfg.DBPath,
		"auth", cfg.User != "")

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("ACS 已退出")
	return nil
}

func uiAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	return listen
}

func newLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if cfg.LogJSON {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
