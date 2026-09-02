// Package bases 是 HTTP 层的公共基座：全局 gin 引擎、应答格式、
// 请求体解码，以及把 actor 框架的调用错误翻译成 HTTP 语义。
//
// 路由本身不在这里。本项目的路由是**显式注册**的（r.POST("/login", h.Login)），
// 没有走参考项目 roleSvr 那套反射自动注册——那套东西和 actor 的 ModObj[T]
// 是同一种 CRTP 思路，但一个示例里演示两遍同样的反射魔法，
// 反而会盖住真正想展示的东西：actor 编排。
package bases

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

// R 是全局 gin 引擎，V1 是它下面的 /api 分组。
//
// 与 roleSvr 一致地做成包级变量，让 main 不必到处传引擎。
// 但路由构造函数一律接收 *gin.RouterGroup 而不是直接用 V1——
// 测试要在自己的引擎上装同一套路由，那时候全局变量帮不上忙。
var (
	R  = gin.New()
	V1 = R.Group("/api")
)

// Setup 按配置初始化全局引擎：gin 模式、日志与 panic 恢复中间件。
func Setup(ginMode string) {
	if ginMode != "" {
		gin.SetMode(ginMode)
	}
	R.Use(gin.Logger(), gin.Recovery())
}

// Run 启动 HTTP 服务并阻塞到收到退出信号，随后优雅关闭。
//
// onShutdown 在 HTTP 停下**之后**才被调用，顺序不能反：
// 先停 HTTP 是为了让在途请求把手上的 actor 调用跑完并释放，
// 反过来做的话在途请求会拿到一堆 actor is closed。
func Run(addr string, handler http.Handler, onShutdown func()) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
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
		return err
	case s := <-sig:
		log.Printf("收到信号 %v，开始退出", s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP 优雅退出超时: %v", err)
	}
	if onShutdown != nil {
		onShutdown()
	}
	return nil
}
