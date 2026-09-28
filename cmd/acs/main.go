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
		AutoFetchWiFi:       cfg.AutoFetchWiFi,
		ProbeCapabilities:   cfg.ProbeCapabilities,
		MaxBodyBytes:        cfg.MaxBodyBytes,
		LogRawSOAP:          cfg.LogRawSOAP,
		OfflineAfter:        cfg.OfflineAfter,
		MaxParamsPerRequest: cfg.MaxParamsPerRequest,
	}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.StartJanitor(ctx)

	mux := http.NewServeMux()
	// 用 "/acs" 精确匹配（末尾不带 / 时只匹配该路径本身），这样别的路径不会被吞掉
	mux.Handle(cfg.Path, srv)
	// 真机/运维常见做法是把 ACS URL 配成 http://host:port/（根路径，不带 /acs）。
	// 这里额外接受根路径上的 POST，免得因为少写一段路径就收不到上报。
	if cfg.Path != "/" {
		mux.Handle("POST /{$}", srv)
	}
	if err := web.Register(mux, st, srv); err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           requestLogger(log, mux),
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
		"cwmp_url_alt", "http://"+uiAddr(cfg.Listen)+"/",
		"ui", "http://"+uiAddr(cfg.Listen)+"/",
		"db", cfg.DBPath,
		"auth", cfg.User != "")

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("ACS 已退出")
	return nil
}

// statusWriter 记住实际写出的状态码。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// requestLogger 记录每个进来的 HTTP 请求。
//
// 为什么需要：真机接不上时，必须能区分「设备压根没打过来」、「打过来了但路径/方法不对」
// 和「打过来了但我们处理出错」这三种情况。所以：
//   - 所有请求在 debug 级别记录；
//   - 「打过来了但我们没接住」（404/405）在 warn 级别记录，这是最需要人看的信号。
func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		dur := time.Since(start)

		switch {
		case sw.status == http.StatusNotFound || sw.status == http.StatusMethodNotAllowed:
			log.Warn("=> 有请求打进来但没被处理（检查 ACS URL 的路径/方法是否写对）",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"from", r.RemoteAddr, "ua", r.UserAgent())
		case r.URL.Path != "/static/style.css" && r.URL.Path != "/static/app.js":
			log.Debug("HTTP",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"from", r.RemoteAddr, "dur", dur.Round(time.Millisecond), "ua", r.UserAgent())
		}
	})
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
