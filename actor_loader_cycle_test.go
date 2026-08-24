package actor

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const cycleModName = "cycleMod"

type cycleMod struct {
	ModObj[*cycleMod]

	peer     *ActorLoader // 只在 Start 之前赋值，之后只在本 actor 协程上读
	gate     chan struct{}
	kickErr  chan error
	notified atomic.Int64
}

func newCycleMod() *cycleMod {
	m := &cycleMod{
		gate:    make(chan struct{}),
		kickErr: make(chan error, 1),
	}
	m.Init()
	return m
}

func (m *cycleMod) Echo(x int) (int, error) { return x, nil }

// Notify 没有返回值，调用方投递完就走，不构成等待边。
func (m *cycleMod) Notify(x int) { m.notified.Add(int64(x)) }

// Ping 把调用同步接力给对端。peer 串成环时，这条链上的某一跳必然被环检测拦下。
func (m *cycleMod) Ping(n int) (int, error) {
	if n <= 0 || m.peer == nil {
		return n, nil
	}
	out, err := m.peer.ModInvoke(cycleModName, "Ping", n-1)
	if err != nil {
		return -1, err
	}
	if e, _ := out[1].Interface().(error); e != nil {
		return -1, e
	}
	return int(out[0].Int()) + 1, nil
}

// NotifyThenAck 对应真实场景：被调方处理完顺手给调用方回推一条消息，再正常返回。
// 那条回推是无返回值的，不该被环检测拦下——哪怕此刻调用方正阻塞等着自己。
func (m *cycleMod) NotifyThenAck(n int) (int, error) {
	if _, err := m.peer.ModInvoke(cycleModName, "Notify", n); err != nil {
		return -1, err
	}
	return n, nil
}

// AskPeer 同步问对端要结果，用来把"调用方正被钉住"这个前提做出来。
func (m *cycleMod) AskPeer(n int) (int, error) {
	out, err := m.peer.ModInvoke(cycleModName, "NotifyThenAck", n)
	if err != nil {
		return -1, err
	}
	if e, _ := out[1].Interface().(error); e != nil {
		return -1, e
	}
	return int(out[0].Int()), nil
}

// Kick 无返回值，用来让 actor 自己的事件循环去发起一次阻塞调用。
// 两个 actor 在 gate 上对齐，才能构造出"同时互相调用"的场面。
func (m *cycleMod) Kick(x int) {
	<-m.gate
	_, err := m.peer.ModInvoke(cycleModName, "Echo", x)
	m.kickErr <- err
}

func startCycleActor(name string) (*ActorLoader, *cycleMod, *sync.WaitGroup) {
	l := NewActorLoader(name)
	l.Init()
	m := newCycleMod()
	l.AddModule(m)
	var wg sync.WaitGroup
	return l, m, &wg
}

// TestCrossActorCycleRejectedImmediately 两个 actor 互相同步调用。
//
// 没有环检测时这里会稳定卡满 defaultTaskTimeout：a 的事件循环卡在等 b，
// 消费不了自己的队列，b 回调过来的任务只能干躺着。
// 有了检测，b 那一跳当场失败，整件事在毫秒级结束。
func TestCrossActorCycleRejectedImmediately(t *testing.T) {
	a, ma, wg := startCycleActor("cycle-a")
	b, mb, _ := startCycleActor("cycle-b")
	ma.peer, mb.peer = b, a // 成环
	a.Start(wg)
	b.Start(wg)
	defer func() {
		a.Close()
		b.Close()
		wg.Wait()
	}()

	start := time.Now()
	out, err := a.ModInvoke(cycleModName, "Ping", 2)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("外层调用不该失败: %v", err)
	}
	inner, _ := out[1].Interface().(error)
	if !errors.Is(inner, ErrCallCycle) {
		t.Fatalf("环形同步调用应当被当场拦下，got %v", inner)
	}
	if elapsed >= defaultTaskTimeout {
		t.Fatalf("耗时 %v：说明没检出环，退化成了超时兜底", elapsed)
	}
	t.Logf("环形调用 %v 内被拦下（没有检测时要卡满 %v）", elapsed, defaultTaskTimeout)

	// 拦下之后两个 actor 都要立刻可用，等待图也要清干净
	for name, l := range map[string]*ActorLoader{"a": a, "b": b} {
		if _, err := l.ModInvoke(cycleModName, "Echo", 1); err != nil {
			t.Fatalf("actor %s 在环被拦下后不可用: %v", name, err)
		}
	}
	if w := a.waitingFor.Load(); w != 0 {
		t.Fatalf("a 仍留在等待图里: waitingFor=%d", w)
	}
	if w := b.waitingFor.Load(); w != 0 {
		t.Fatalf("b 仍留在等待图里: waitingFor=%d", w)
	}
}

// TestCrossActorThreeWayCycleRejected 环不止两环，回溯得能一路走下去。
func TestCrossActorThreeWayCycleRejected(t *testing.T) {
	a, ma, wg := startCycleActor("tri-a")
	b, mb, _ := startCycleActor("tri-b")
	c, mc, _ := startCycleActor("tri-c")
	ma.peer, mb.peer, mc.peer = b, c, a // a→b→c→a
	a.Start(wg)
	b.Start(wg)
	c.Start(wg)
	defer func() {
		a.Close()
		b.Close()
		c.Close()
		wg.Wait()
	}()

	start := time.Now()
	out, err := a.ModInvoke(cycleModName, "Ping", 3)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("外层调用不该失败: %v", err)
	}
	inner, _ := out[1].Interface().(error)
	if !errors.Is(inner, ErrCallCycle) {
		t.Fatalf("三环也应当被拦下，got %v", inner)
	}
	if elapsed >= defaultTaskTimeout {
		t.Fatalf("耗时 %v：三环没检出", elapsed)
	}
	t.Logf("a→b→c→a 三环 %v 内被拦下", elapsed)
}

// TestCrossActorChainNotRejected 对照组：不成环的链式同步调用不能被误伤。
func TestCrossActorChainNotRejected(t *testing.T) {
	a, ma, wg := startCycleActor("chain-a")
	b, mb, _ := startCycleActor("chain-b")
	c, mc, _ := startCycleActor("chain-c")
	ma.peer, mb.peer, mc.peer = b, c, nil // a→b→c，到此为止
	a.Start(wg)
	b.Start(wg)
	c.Start(wg)
	defer func() {
		a.Close()
		b.Close()
		c.Close()
		wg.Wait()
	}()

	out, err := a.ModInvoke(cycleModName, "Ping", 2)
	if err != nil {
		t.Fatalf("链式调用失败: %v", err)
	}
	if e, _ := out[1].Interface().(error); e != nil {
		t.Fatalf("不成环的链式调用被误伤: %v", e)
	}
	if got := int(out[0].Int()); got != 2 {
		t.Fatalf("a→b→c 应当接力两跳, got %d", got)
	}
}

// TestVoidCallbackIntoWaitingActorNotRejected 覆盖最容易误伤的那种写法：
// a 正阻塞等着 b 的返回值，b 处理过程中给 a 回推一条无返回值的消息。
//
// 这不构成死锁——b 投递完就走，a 拿到结果后自然会把那条消息消费掉。
// 环检测必须放行它，否则"处理完顺手通知调用方"这种再普通不过的写法就废了。
func TestVoidCallbackIntoWaitingActorNotRejected(t *testing.T) {
	a, ma, wg := startCycleActor("void-a")
	b, mb, _ := startCycleActor("void-b")
	ma.peer, mb.peer = b, a
	a.Start(wg)
	b.Start(wg)
	defer func() {
		a.Close()
		b.Close()
		wg.Wait()
	}()

	out, err := a.ModInvoke(cycleModName, "AskPeer", 5)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if e, _ := out[1].Interface().(error); e != nil {
		t.Fatalf("无返回值的回推被环检测误伤了: %v", e)
	}
	if got := int(out[0].Int()); got != 5 {
		t.Fatalf("结果异常: %d", got)
	}

	// taskChan 是单消费者 FIFO：这次调用返回时，b 回推给 a 的 Notify
	// 必然已经被 a 的事件循环消费掉了，不用 sleep 去猜。
	if _, err := a.ModInvoke(cycleModName, "Echo", 1); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}
	if got := ma.notified.Load(); got != 5 {
		t.Fatalf("a 应当收到回推的通知, notified=%d", got)
	}
}

// TestSimultaneousMutualCallsAtLeastOneRejected 两个 actor 同时向对方发起阻塞调用。
//
// 这是最难的时序：双方几乎同时进入检测，谁都可能还没看到对方的意图。
// 实现上先公布"我要等谁"再回溯，保证至少一方能看见对方——
// 最坏情况是双方都拒绝，那也是安全的方向，绝不能双双漏检然后一起卡满超时。
func TestSimultaneousMutualCallsAtLeastOneRejected(t *testing.T) {
	a, ma, wg := startCycleActor("mutual-a")
	b, mb, _ := startCycleActor("mutual-b")
	ma.peer, mb.peer = b, a
	a.Start(wg)
	b.Start(wg)
	defer func() {
		a.Close()
		b.Close()
		wg.Wait()
	}()

	// Kick 无返回值，投递完即返回；两个事件循环随后一起停在各自的 gate 上
	if _, err := a.ModInvoke(cycleModName, "Kick", 1); err != nil {
		t.Fatalf("投递 Kick 到 a 失败: %v", err)
	}
	if _, err := b.ModInvoke(cycleModName, "Kick", 1); err != nil {
		t.Fatalf("投递 Kick 到 b 失败: %v", err)
	}

	start := time.Now()
	close(ma.gate) // 同时放行，两边一起冲进 ModInvoke
	close(mb.gate)

	var errs [2]error
	for i, ch := range []chan error{ma.kickErr, mb.kickErr} {
		select {
		case errs[i] = <-ch:
		case <-time.After(2 * defaultTaskTimeout):
			t.Fatalf("第 %d 个 Kick 没有返回", i)
		}
	}
	elapsed := time.Since(start)

	rejected := 0
	for _, err := range errs {
		if errors.Is(err, ErrCallCycle) {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatalf("双方都没检出环: a=%v b=%v，耗时 %v", errs[0], errs[1], elapsed)
	}
	if elapsed >= defaultTaskTimeout {
		t.Fatalf("耗时 %v：至少有一方退化成了超时兜底", elapsed)
	}
	t.Logf("同时互调：%d/2 方被拦下，整体 %v 内收敛", rejected, elapsed)
}

// TestOrdinaryGoroutineCallerNotInWaitGraph 普通业务协程（网络处理、worker）
// 阻塞等待不会钉住任何事件循环，不该进等待图，也不该因为并发多而被误判成环。
func TestOrdinaryGoroutineCallerNotInWaitGraph(t *testing.T) {
	a, ma, wg := startCycleActor("plain-a")
	b, mb, _ := startCycleActor("plain-b")
	ma.peer, mb.peer = b, a
	a.Start(wg)
	b.Start(wg)
	defer func() {
		a.Close()
		b.Close()
		wg.Wait()
	}()

	const callers, perCaller = 16, 200
	var failed int64
	var callerWG sync.WaitGroup
	callerWG.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer callerWG.Done()
			gid := CurrentGID()
			target := a
			if i%2 == 1 {
				target = b
			}
			for j := 0; j < perCaller; j++ {
				if _, err := target.ModInvokeFrom(gid, cycleModName, "Echo", j); err != nil {
					atomic.AddInt64(&failed, 1)
					return
				}
			}
		}(i)
	}
	callerWG.Wait()

	if failed != 0 {
		t.Fatalf("普通协程的调用被拦下了 %d 次：环检测误伤了非 actor 调用方", failed)
	}
}

// registrySize 数一遍等待图的注册表。只有本包能看到它，所以这条断言只能写在这里。
func registrySize() int {
	n := 0
	loaderRegistry.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestLoaderRegistryClearedOnClose actor 关闭后必须从等待图的注册表里退出来。
//
// 这是环检测引入的一处新泄漏面，而且 goroutine 数看不见它：actor 上下线频繁的
// 游戏服里，漏删一条就意味着注册表随在线人次无限膨胀，环检测回溯时还会一路
// 走进早就死掉的 actor。
func TestLoaderRegistryClearedOnClose(t *testing.T) {
	before := registrySize()

	const rounds = 500
	for i := 0; i < rounds; i++ {
		l := NewActorLoader(fmt.Sprintf("reg-%d", i))
		l.Init()
		l.AddModule(newCycleMod())
		var wg sync.WaitGroup
		l.Start(&wg)
		if _, err := l.ModInvoke(cycleModName, "Echo", i); err != nil {
			t.Fatalf("第 %d 轮调用失败: %v", i, err)
		}
		l.Close()
		wg.Wait()
	}

	if after := registrySize(); after != before {
		t.Fatalf("%d 轮建/停之后注册表残留 %d 条：actor 关闭时没退出等待图",
			rounds, after-before)
	}
}
