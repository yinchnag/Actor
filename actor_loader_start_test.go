package actor

import (
	"errors"
	"sync"
	"testing"
)

type startRaceMod struct {
	ModObj[*startRaceMod]
	n int
}

func newStartRaceMod() *startRaceMod {
	m := &startRaceMod{}
	m.Init()
	return m
}

func (m *startRaceMod) Bump(x int) int { m.n += x; return m.n }

// TestActorLoaderStartNoStartupRace 覆盖启动竞态。
//
// 曾经 ModInvokeFrom 把 goroutineID == 0 当作"同协程"处理，于是在
// `go RunUpdateLoop` 与循环内部写入 goroutineID 之间的窗口里，任意多个调用方
// 都会各自在自己的栈上直接执行模块方法——actor "模块状态不被并发触碰"
// 的核心保证就此失效（-race 可直接抓到，计数也会丢失）。
//
// Start 返回时 goroutineID 已经发布，窗口不存在，因此这里不 sleep、不轮询。
func TestActorLoaderStartNoStartupRace(t *testing.T) {
	const callers, perCaller = 8, 200

	loader := NewActorLoader("start-race")
	loader.Init()
	loader.AddModule(newStartRaceMod())

	// 先把调用方全部拉起并停在栅栏上（CurrentGID 这类慢操作提前做完），
	// 再启动事件循环、同时放行。这样它们恰好落在"启动瞬间"，
	// 才能真正压到 goroutineID 尚未发布的那个窗口。
	gate := make(chan struct{})
	var callerWG sync.WaitGroup
	ready := make(chan struct{}, callers)
	for i := 0; i < callers; i++ {
		callerWG.Add(1)
		go func() {
			defer callerWG.Done()
			gid := CurrentGID()
			ready <- struct{}{}
			<-gate
			for j := 0; j < perCaller; j++ {
				if _, err := loader.ModInvokeFrom(gid, "startRaceMod", "Bump", 1); err != nil {
					t.Errorf("Bump failed: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < callers; i++ {
		<-ready
	}

	var loopWG sync.WaitGroup
	loader.Start(&loopWG)
	close(gate)
	callerWG.Wait()

	out, err := loader.ModInvokeFrom(CurrentGID(), "startRaceMod", "Bump", 0)
	if err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if got, want := int(out[0].Int()), callers*perCaller; got != want {
		t.Fatalf("计数丢失: got %d, want %d（说明有调用绕过了事件循环）", got, want)
	}

	loader.Close()
	loopWG.Wait()
}

// TestActorLoaderNotStartedRejectsOtherGoroutines 验证未启动时的所有权语义：
// 第一个使用 loader 的 goroutine 可以直接执行（初始化场景），
// 其余 goroutine 拿到 ErrActorNotStarted，而不是并发直改模块状态。
func TestActorLoaderNotStartedRejectsOtherGoroutines(t *testing.T) {
	loader := NewActorLoader("not-started")
	loader.Init()
	loader.AddModule(newStartRaceMod())

	// 当前 goroutine 抢到所有权
	if _, err := loader.ModInvoke("startRaceMod", "Bump", 1); err != nil {
		t.Fatalf("初始化持有者应当可以直接调用: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := loader.ModInvoke("startRaceMod", "Bump", 1)
		errCh <- err
	}()
	if err := <-errCh; !errors.Is(err, ErrActorNotStarted) {
		t.Fatalf("其它 goroutine 应当被拒绝，got %v", err)
	}

	// 持有者仍然可用
	out, err := loader.ModInvoke("startRaceMod", "Bump", 0)
	if err != nil {
		t.Fatalf("持有者调用失败: %v", err)
	}
	if got := int(out[0].Int()); got != 1 {
		t.Fatalf("被拒绝的调用不应改到状态: got %d, want 1", got)
	}
}
