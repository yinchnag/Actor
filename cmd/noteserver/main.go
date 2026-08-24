package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	envPath := flag.String("env", ".env", "配置文件路径")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[noteserver] ")

	cfg, err := LoadConfig(*envPath)
	if err != nil {
		log.Fatalf("配置有问题: %v", err)
	}

	store, err := OpenMySQL(cfg.MySQLDSN)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer store.Close()

	sessions, err := OpenRedis(cfg.RedisURL)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer sessions.Close()

	hub := NewHub(store)
	defer hub.Close()

	srv := &http.Server{
		Addr:    cfg.Listen(),
		Handler: NewServer(hub, sessions).Routes(),
		// 读写超时都比框架的任务超时（3s）宽一些，否则请求会先被 HTTP 层掐掉，
		// 客户端看到的是连接断开而不是我们精心区分过的超时语义。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("监听 %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Fatalf("监听失败: %v", err)
	case s := <-sig:
		log.Printf("收到信号 %v，开始退出", s)
	}

	// 先停 HTTP：让在途请求把手上的 actor 调用跑完并释放，
	// 再关 actor。顺序反了的话在途请求会拿到一堆 actor is closed。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP 优雅退出超时: %v", err)
	}
	hub.Close()
	log.Print("已退出")
}
