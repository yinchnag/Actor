package actor

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type closeRaceMod struct{ ModObj[*closeRaceMod] }

func newCloseRaceMod() *closeRaceMod {
	m := &closeRaceMod{}
	m.Init()
	return m
}

func (m *closeRaceMod) Echo(x int) int { return x }

// TestActorLoaderCloseRaceNoStrandedTask 覆盖投递与 Close 的竞态。
//
// enqueueTask 的 select 里「发送」和「stopChan 已关」可能同时就绪，Go 会随机选。
// 若此时 loop 已经排空并退出，被随机选中发送的任务就落进了没人消费的 channel：
// 它的 done 永远不会关闭，调用方等满 defaultTaskTimeout 后，
// ModInvokeFrom 的清理 goroutine 会永久阻塞在 <-doneCh，ChanTask 也回不了池。
//
// 窗口很窄，靠单轮跑不出来，用多轮「建 actor → 并发调用 → 立刻 Close」放大。
func TestActorLoaderCloseRaceNoStrandedTask(t *testing.T) {
	const rounds = 200
	const callersPerRound = 40

	gBefore := runtime.NumGoroutine()
	var timedOut int64

	for round := 0; round < rounds; round++ {
		loader := NewActorLoader("close-race")
		loader.Init()
		loader.AddModule(newCloseRaceMod())

		var loopWG sync.WaitGroup
		loopWG.Add(1)
		go loader.RunUpdateLoop(&loopWG)

		var callers sync.WaitGroup
		for i := 0; i < callersPerRound; i++ {
			callers.Add(1)
			go func(i int) {
				defer callers.Done()
				gid := CurrentGID()
				start := time.Now()
				if _, err := loader.ModInvokeFrom(gid, "closeRaceMod", "Echo", i); err != nil {
					// 关闭期间报错是正常的；等满超时才说明任务被丢在了队列里
					if time.Since(start) >= defaultTaskTimeout {
						atomic.AddInt64(&timedOut, 1)
					}
				}
			}(i)
		}

		loader.Close()
		callers.Wait()
		loopWG.Wait()

		if n := len(loader.taskChan); n != 0 {
			t.Fatalf("round %d: %d 个任务被遗留在 taskChan 中未结算", round, n)
		}
	}

	if n := atomic.LoadInt64(&timedOut); n != 0 {
		t.Fatalf("%d 次调用等满了 %v 才返回，说明任务投递后无人结算", n, defaultTaskTimeout)
	}

	// 每个被遗弃的任务都会永久挂住一个清理 goroutine
	time.Sleep(200 * time.Millisecond)
	if leaked := runtime.NumGoroutine() - gBefore; leaked > callersPerRound/2 {
		t.Errorf("疑似 goroutine 泄漏: 增量 %d", leaked)
	}
}
