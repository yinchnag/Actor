package actor

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type cancelMod struct {
	ModObj[*cancelMod]

	gate        chan struct{}
	releaseOnce sync.Once
	entered     chan struct{}
	worked      atomic.Int64 // Work 的方法体真正被执行到的次数
}

func newCancelMod() *cancelMod {
	m := &cancelMod{
		gate:    make(chan struct{}),
		entered: make(chan struct{}, 8),
	}
	m.Init()
	return m
}

// Block 把事件循环钉死，好制造出"任务排在队列里没人取"的局面。
func (m *cancelMod) Block(x int) int {
	select {
	case m.entered <- struct{}{}:
	default:
	}
	<-m.gate
	return x
}

// Work 是被取消的目标：方法体只要跑到，计数就加一。
func (m *cancelMod) Work(x int) int {
	m.worked.Add(1)
	return x
}

func (m *cancelMod) release() {
	m.releaseOnce.Do(func() { close(m.gate) })
}

// waitCallers 等一组调用方返回。不直接 wg.Wait() 是因为"卡住不返回"正是
// 这里要抓的失败模式，直接 Wait 会把测试挂死，什么信息都报不出来。
func waitCallers(t *testing.T, wg *sync.WaitGroup, timeout time.Duration, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("等待超时(%v): %s", timeout, what)
	}
}

// TestModInvokeTimeoutCancelsQueuedTask 覆盖"超时即取消"的两条分支。
//
// 事件循环被 Block 钉死期间：
//   - 正在执行的那个任务（Block 自己）取消不掉——Go 打断不了运行中的函数，
//     所以调用方拿到的超时错误里不该带 ErrTaskCanceled，否则就是向业务谎报
//     "副作用没发生"；
//   - 还排在队列里的任务（Work）能被就地取消，调用方拿到 ErrTaskCanceled，
//     并且放行之后事件循环必须跳过它，方法体一次都不能执行。
//
// 后者是这次改动的要害。在此之前，调用方超时走人后任务照样躺在队列里，
// 等 actor 空下来再补跑一遍：结果没人接收，副作用却照做——
// 放到业务上就是重复扣费、重复发奖。
func TestModInvokeTimeoutCancelsQueuedTask(t *testing.T) {
	loader := NewActorLoader("cancel")
	loader.Init()
	mod := newCancelMod()
	loader.AddModule(mod)

	var loopWG sync.WaitGroup
	loader.Start(&loopWG)
	defer func() {
		mod.release() // 先放行 Block，事件循环才退得出来
		loader.Close()
		loopWG.Wait()
	}()

	// 1) 把事件循环钉死在 Block 上
	var callers sync.WaitGroup
	var blockErr error
	callers.Add(1)
	go func() {
		defer callers.Done()
		_, blockErr = loader.ModInvoke("cancelMod", "Block", 1)
	}()
	select {
	case <-mod.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("事件循环没能进入 Block")
	}

	// 2) 此时投进去的 Work 只能排队，没人取
	var workErr error
	callers.Add(1)
	go func() {
		defer callers.Done()
		_, workErr = loader.ModInvoke("cancelMod", "Work", 7)
	}()

	// 两个调用方都会在 defaultTaskTimeout 上下返回，放宽到 3 倍留调度裕量
	waitCallers(t, &callers, 3*defaultTaskTimeout, "两个调用方超时返回")

	if !errors.Is(blockErr, ErrTaskAwaitTimeout) {
		t.Fatalf("Block 的调用方应当等到超时，got %v", blockErr)
	}
	if errors.Is(blockErr, ErrTaskCanceled) {
		t.Fatal("Block 已被事件循环认领并正在执行，不该报告成已取消")
	}
	if !errors.Is(workErr, ErrTaskAwaitTimeout) {
		t.Fatalf("Work 的调用方应当等到超时，got %v", workErr)
	}
	if !errors.Is(workErr, ErrTaskCanceled) {
		t.Fatalf("还在排队的 Work 应当被就地取消，got %v", workErr)
	}

	// 3) 放行，让事件循环把积压跑完
	mod.release()

	// taskChan 是单消费者 FIFO：这次调用返回时，排在它前面那个被取消的 Work
	// 必然已经被事件循环处理过了（应当是跳过），不需要 sleep 去猜。
	out, err := loader.ModInvoke("cancelMod", "Work", 42)
	if err != nil {
		t.Fatalf("取消之后 actor 应当照常可用: %v", err)
	}
	if len(out) != 1 || int(out[0].Int()) != 42 {
		t.Fatalf("屏障调用结果异常: %v", out)
	}

	if got := mod.worked.Load(); got != 1 {
		t.Fatalf("Work 方法体执行了 %d 次，期望 1 次——被取消的那次不该补跑", got)
	}
}

// TestChanTaskClaimAndAbandonAreExclusive 直接压状态机本身：
// abandon 与 claimForRun 是一对竞争的 CAS，无论多少协程同时抢，
// 只能有一个赢。赢家决定模块方法到底执不执行，两边都赢就会重复执行，
// 两边都输则任务永远不会被结算、调用方的清理协程永久挂死。
func TestChanTaskClaimAndAbandonAreExclusive(t *testing.T) {
	const rounds = 2000

	for i := 0; i < rounds; i++ {
		task := NewChanTask(1, 2, "cancelMod", "Work", 1)

		var claimed, abandoned bool
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			claimed = task.claimForRun()
		}()
		go func() {
			defer wg.Done()
			<-start
			abandoned = task.abandon()
		}()
		close(start)
		wg.Wait()

		if claimed == abandoned {
			t.Fatalf("第 %d 轮：claimForRun=%v abandon=%v，两者必须恰好一个成功",
				i, claimed, abandoned)
		}

		wantStatus := TaskStatusRunning
		if abandoned {
			wantStatus = TaskStatusAbandoned
		}
		if got := task.GetStatus(); got != wantStatus {
			t.Fatalf("第 %d 轮状态错乱: got %d, want %d", i, got, wantStatus)
		}

		// 两种状态都必须还能被结算，否则等在 done 上的一方永远醒不过来
		task.complete(nil, nil)
		if got := task.GetStatus(); got != TaskStatusDone {
			t.Fatalf("第 %d 轮 complete 之后状态应为 Done, got %d", i, got)
		}
		task.Release()
	}
}
