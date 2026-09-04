package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
)

func main() {
	setMemorySoftLimit()

	UpdatePolicy = "web"

	runtime, err := newAppRuntime()
	if err != nil {
		log.Fatalf("启动服务失败: %v", err)
	}
	defer runtime.shutdown()

	server := newAdminServer(runtime)
	log.Printf("web admin listening on http://%s", runtime.adminAddr)
	if runtime.providerRelay != nil {
		log.Printf("provider relay listening on http://%s", runtime.providerRelay.Addr())
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil {
			log.Fatalf("web admin server failed: %v", err)
		}
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("web admin shutdown failed: %v", err)
	}
}

// setMemorySoftLimit 设置 Go 运行时内存软上限（GOMEMLIMIT）。
// 软上限不会导致 OOM，只让 GC 在逼近上限时更积极地回收并归还内存，
// 避免转发大请求体时的瞬时分配把 RSS 顶到历史峰值后长期不降。
// 可用环境变量 CODESWITCH_MEMLIMIT（字节数）覆盖，设为 0 表示不限制。
func setMemorySoftLimit() {
	const defaultLimit = 512 << 20 // 512MiB
	if v := os.Getenv("CODESWITCH_MEMLIMIT"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			log.Printf("忽略无效的 CODESWITCH_MEMLIMIT=%q: %v", v, err)
			return
		}
		debug.SetMemoryLimit(n)
		return
	}
	debug.SetMemoryLimit(defaultLimit)
}
