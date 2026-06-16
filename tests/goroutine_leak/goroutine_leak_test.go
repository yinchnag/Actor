package goroutineleak_test

import (
	"sync"
	"testing"
	"time"

	"actor"

	"go.uber.org/goleak"
)

// TestMain 在所有测试结束后用 goleak 全局扫描泄漏的 goroutine。
// 任何测试留下未关闭的 goroutine 都会在这里被捕获。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// --- 辅助 ---

type slowMod struct {
	actor.ModObj[*slowMod]
}

func newSlowMod() *slowMod {
	m := &slowMod{}
	m.Init()
	return m
}

// Fast 立即返回，用于验证 loader 超时后仍健康。
func (m *slowMod) Fast(a, b int) int { return a + b }

// Slow 故意阻塞 3.5s，触发默认 3s 超时（保留 0.5s 裕量避免机器负载影响）。
func (m *slowMod) Slow(a, b int) int {
	time.Sleep(3500 * time.Millisecond)
	return a + b
}

func startLoader(t *testing.T, name string) (*actor.ActorLoader, func()) {
	t.Helper()
	loader := actor.NewActorLoader(name)
	loader.Init()
	loader.AddModule(newSlowMod())

	var wg sync.WaitGroup
	wg.Add(1)
	go loader.RunUpdateLoop(&wg)

	// 等 goroutineID 就绪
	for i := 0; i < 50 && loader.GetGoroutineID() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}

	stop := func() {
		loader.Close()
		wg.Wait()
	}
	return loader, stop
}

// --- 测试 ---

// TestNoLeakNormalUsage 验证正常调用路径不泄漏 goroutine。
func TestNoLeakNormalUsage(t *testing.T) {
	loader, stop := startLoader(t, "normal")
	defer stop()

	for i := 0; i < 20; i++ {
		out, err := loader.ModInvoke("slowMod", "Fast", i, i)
		if err != nil {
			t.Fatalf("invoke failed: %v", err)
		}
		if len(out) != 1 || int(out[0].Int()) != i+i {
			t.Fatalf("unexpected result at i=%d", i)
		}
	}
}

// TestNoLeakOnTimeout 验证超时路径的清理 goroutine 在任务最终完成后能正常退出。
//
// 超时流程：ModInvoke 在 3s 后返回 ErrTaskAwaitTimeout，同时启动一个后台
// goroutine 等待任务完成后释放 ChanTask（actor_loader.go 中的清理路径）。
// Slow 执行 3.5s，<-done 等调用方 goroutine 退出，再等 2s 让清理 goroutine 退出。
// goleak 在 TestMain 中验证无泄漏。
func TestNoLeakOnTimeout(t *testing.T) {
	loader, stop := startLoader(t, "timeout")
	defer stop()

	// 单个慢任务，避免多个任务串行堆积拉长总等待时间。
	done := make(chan struct{})
	go func() {
		defer close(done)
		loader.ModInvoke("slowMod", "Slow", 1, 2) //nolint:errcheck
	}()
	<-done // 调用方 goroutine 在 3s 超时后返回

	// Slow 在 actor 内执行 3.5s，等 2s 确保 Slow 完成（共 5s > 3.5s）
	// 并让清理 goroutine 在 <-doneCh 返回后退出
	time.Sleep(2 * time.Second)
}

// TestLoaderHealthyAfterTimeout 验证单次超时后 loader 仍可正常处理新请求。
func TestLoaderHealthyAfterTimeout(t *testing.T) {
	loader, stop := startLoader(t, "health-after-timeout")
	defer stop()

	go func() {
		loader.ModInvoke("slowMod", "Slow", 1, 2) //nolint:errcheck
	}()

	// Slow 执行 3.5s，等待 4s 确保其在 actor 内执行完毕
	time.Sleep(4 * time.Second)

	out, err := loader.ModInvoke("slowMod", "Fast", 3, 4)
	if err != nil {
		t.Fatalf("loader unhealthy after timeout: %v", err)
	}
	if len(out) != 1 || int(out[0].Int()) != 7 {
		t.Fatalf("unexpected result: %v", out)
	}
}

// TestNoLeakOnClose 验证在有任务 in-flight 时关闭 loader，不泄漏 goroutine。
//
// 修复前行为：队列中未处理的任务的 done channel 永远不会关闭，
// 导致清理 goroutine 永久挂起。
// 修复后：RunUpdateLoop 关闭时 drain 队列，对所有未处理任务调用
// complete(nil, errActorClosed)，done channel 被关闭，清理 goroutine 可正常退出。
func TestNoLeakOnClose(t *testing.T) {
	loader, stop := startLoader(t, "close-inflight")

	go func() {
		loader.ModInvoke("slowMod", "Slow", 1, 2) //nolint:errcheck
	}()

	// 给任务入队的时间
	time.Sleep(100 * time.Millisecond)

	// 在任务执行中途关闭；stop() 会等待 RunUpdateLoop 退出
	// （若 Slow 正在执行则阻塞至 Slow 完成，约 3.5s）
	stop()

	// stop() 返回时 actor 已退出。若 Slow 是在 close 前开始执行的，
	// 清理 goroutine 在 done 关闭后立即退出；等待 1s 作为调度缓冲。
	time.Sleep(1 * time.Second)
}

// TestNoLeakHighConcurrencyTimeout 并发制造多个超时，验证无泄漏。
//
// Slow 执行 3.5s，actor 串行处理：2 个任务共需 7s。
// wg.Wait() 在 3s（超时）后返回，再等 5s（共 8s > 7s），确保所有清理 goroutine 退出。
func TestNoLeakHighConcurrencyTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in short mode")
	}

	loader, stop := startLoader(t, "hc-timeout")
	defer stop()

	const workers = 2
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			loader.ModInvoke("slowMod", "Slow", 1, 2) //nolint:errcheck
		}()
	}
	wg.Wait() // 3s 后两个调用方均超时返回

	// 2 个 Slow 串行执行共 7s，等待 5s（总计 8s > 7s）确保清理 goroutine 全部退出
	time.Sleep(5 * time.Second)
}