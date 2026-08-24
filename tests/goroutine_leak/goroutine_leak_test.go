package goroutineleak_test

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"actor"

	"go.uber.org/goleak"
)

// 本文件压的是 actor 框架"边界之外"的行为：队列被打满、模块方法 panic、
// 关闭时队列里还有活、跨 actor 环形同步调用、actor 大批量创建销毁。
// 这些路径只要漏掉一次结算，泄漏的就不止一个 goroutine——
// ModInvokeFrom 超时后会派一个清理协程挂在 <-doneCh 上，
// 任务永远不 complete，它就永远不退出，ChanTask 也回不了池。
//
// 两层防护：每个用例自己 defer noLeak(t)()，泄漏能定位到具体用例；
// TestMain 再全局兜一次，防止用例的检查窗口没覆盖到。
//
// 时间处理原则：能用 channel 同步就不用 sleep。唯一躲不开的等待是框架写死的
// defaultTaskTimeout(3s)——它不可配置，凡是要触发超时的用例都至少花 3s，
// 已用 -short 标出。

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ModObj 用宿主结构体的类型名当模块名。
const (
	gateName  = "gateMod"
	relayName = "relayMod"
	selfName  = "selfMod"
)

// defaultTaskTimeout 在框架内部是私有常量，这里留一点裕量复刻它，
// 用于等待"未 Stop 的定时器"自然到期。
const timeoutSlack = 4 * time.Second

// --- 通用辅助 ---

// noLeak 返回一个收尾函数：只盯本用例新增的 goroutine。
// 用法 `defer noLeak(t)()` —— 它要第一个注册，才能最后一个执行（defer 是 LIFO），
// 否则会在 actor 还没停下来的时候就去数 goroutine。
func noLeak(t *testing.T) func() {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	return func() { goleak.VerifyNone(t, ignore) }
}

// waitAll 等一组调用方全部返回。用它而不是直接 wg.Wait()，
// 是因为"卡住不返回"正是这些用例要抓的 bug——直接 Wait 会把测试挂死，
// 报不出任何信息。
func waitAll(t *testing.T, wg *sync.WaitGroup, timeout time.Duration, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("等待超时(%v): %s", timeout, what)
	}
}

func requireStress(t *testing.T) {
	t.Helper()
	if os.Getenv("ACTOR_STRESS") != "1" {
		t.Skip("重压用例，设置 ACTOR_STRESS=1 运行")
	}
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*p)]
}

// --- 压测模块 ---

// gateMod 的耗时行为全部由 channel 控制，不用 sleep：
// "actor 何时被堵死""何时放行"由测试精确决定，断言就不会随机器负载抖动，
// goleak 那约 0.5s 的重试窗口也才够用。
type gateMod struct {
	actor.ModObj[*gateMod]

	gate        chan struct{} // 关闭后 Block 立即返回
	releaseOnce sync.Once
	entered     chan int // Block 已进入的信号，带缓冲、非阻塞投递

	echoed  atomic.Int64
	fired   atomic.Int64
	updates atomic.Int64
}

func newGateMod() *gateMod {
	m := &gateMod{
		gate:    make(chan struct{}),
		entered: make(chan int, 1024),
	}
	m.Init() // Init 靠字段偏移反推宿主指针，此后这个结构体不能再被拷贝
	return m
}

// Echo 是最廉价的有返回值方法，用来量吞吐、验结果有没有串味。
func (m *gateMod) Echo(x int) int {
	m.echoed.Add(1)
	return x
}

// Fire 没有返回值 → shouldWaitResult 为 false → 走"投递完就返回"的路径，
// 不会创建 3s 定时器，适合用来长时间饱和 actor。
func (m *gateMod) Fire(x int) {
	m.fired.Add(int64(x))
}

// Block 把事件循环钉死在这里，直到测试关闭 gate。
// 事件循环是单消费者，钉住它就等于钉住整个 actor——正是要压的那条边界。
func (m *gateMod) Block(x int) int {
	select {
	case m.entered <- x:
	default:
	}
	<-m.gate
	return x
}

// Boom 模拟模块方法炸掉：handleTask 会 recover，把 panic 当错误回传，
// 顺手 Close 掉整个 actor。
func (m *gateMod) Boom(x int) int {
	panic(fmt.Sprintf("boom:%d", x))
}

// Update 覆盖 ModObj.Update（它在 baseMethodSet 里，不会被反射注册），
// 用来验证重压之下 1s 的 ticker 没被任务饿死。
func (m *gateMod) Update(dt int64) {
	_ = dt
	m.updates.Add(1)
}

func (m *gateMod) release() {
	m.releaseOnce.Do(func() { close(m.gate) })
}

// harness 把 loader、模块和事件循环的 WaitGroup 绑在一起。
// 需要精确控制关闭时序的用例可以单独调 mod.release()/loader.Close()/wg.Wait()，
// stop() 是幂等的兜底。
type harness struct {
	loader *actor.ActorLoader
	mod    *gateMod
	wg     sync.WaitGroup
	once   sync.Once
}

func newHarness(name string) *harness {
	h := &harness{loader: actor.NewActorLoader(name), mod: newGateMod()}
	h.loader.Init()
	h.loader.AddModule(h.mod)
	// Start 返回时 goroutineID 已发布：此后外部调用必然走入队路径，
	// 不会退化成 directInvoke 并发直改模块状态，也就不必 sleep 等启动。
	h.loader.Start(&h.wg)
	return h
}

func (h *harness) stop() {
	h.once.Do(func() {
		h.mod.release() // 先放行卡在 Block 里的方法，否则事件循环退不出来
		h.loader.Close()
		h.wg.Wait()
	})
}

// blockActor 让事件循环停在 Block 上；返回时可以确信 actor 已被堵死。
func (h *harness) blockActor(t *testing.T) {
	t.Helper()
	go h.loader.ModInvoke(gateName, "Block", 1) //nolint:errcheck
	select {
	case <-h.mod.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("事件循环没能进入 Block")
	}
}

// --- 基线 ---

// TestNoLeakNormalUsage 正常调用路径不泄漏，且结果逐一对得上号。
func TestNoLeakNormalUsage(t *testing.T) {
	defer noLeak(t)()
	h := newHarness("normal")
	defer h.stop()

	const calls = 2000
	gid := actor.CurrentGID()
	for i := 0; i < calls; i++ {
		out, err := h.loader.ModInvokeFrom(gid, gateName, "Echo", i)
		if err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i, err)
		}
		if len(out) != 1 || int(out[0].Int()) != i {
			t.Fatalf("第 %d 次调用结果串味: %v", i, out)
		}
	}
	if got := h.mod.echoed.Load(); got != calls {
		t.Fatalf("实际执行 %d 次，期望 %d 次", got, calls)
	}
}

// TestNoLeakFireAndForgetFlood 压无返回值路径：投递完即返回，
// 任务由事件循环 complete 后自行 Release。丢一个就对不上账。
func TestNoLeakFireAndForgetFlood(t *testing.T) {
	defer noLeak(t)()
	h := newHarness("fire-flood")
	defer h.stop()

	workers := runtime.NumCPU()
	if workers < 4 {
		workers = 4
	}
	const perWorker = 2000

	var enqueued, rejected int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			gid := actor.CurrentGID()
			for i := 0; i < perWorker; i++ {
				if _, err := h.loader.ModInvokeFrom(gid, gateName, "Fire", 1); err != nil {
					atomic.AddInt64(&rejected, 1)
					continue
				}
				atomic.AddInt64(&enqueued, 1)
			}
		}()
	}
	waitAll(t, &wg, 60*time.Second, "fire-and-forget 洪水")

	// taskChan 是单消费者 FIFO：这次同步调用返回时，
	// 排在它前面的 Fire 必然都已执行完，不需要 sleep 去猜。
	if _, err := h.loader.ModInvoke(gateName, "Echo", 1); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}

	if rejected != 0 {
		t.Errorf("%d 次投递被拒：64 槽的队列在 3s 内都没排空，吞吐异常", rejected)
	}
	if got := h.mod.fired.Load(); got != enqueued {
		t.Fatalf("投递成功 %d 次，实际执行 %d 次——有任务被丢了", enqueued, got)
	}
	t.Logf("fire-and-forget: %d 次投递全部落地", enqueued)
}

// TestNoLeakOnCloseDrainsQueuedTasks 覆盖"事件循环还堵着、队列里全是活"时关闭。
//
// 关键在 closeWith 的顺序：先置 closed、关 stopChan 放走卡在满队列上的投递方，
// 再拿 enqueueMu 写锁排空。少任何一步，队列里的任务就没人 complete：
// 调用方要干等满 3s，超时后派出的清理协程更会永久挂在 <-doneCh 上。
func TestNoLeakOnCloseDrainsQueuedTasks(t *testing.T) {
	defer noLeak(t)()
	h := newHarness("close-drain")
	defer h.stop()

	h.blockActor(t)

	const callers = 100 // 远超 taskChan 的 64 槽，剩下的会卡在投递上
	var slow, returned int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			gid := actor.CurrentGID()
			start := time.Now()
			_, _ = h.loader.ModInvokeFrom(gid, gateName, "Echo", i)
			if time.Since(start) >= 2*time.Second {
				atomic.AddInt64(&slow, 1)
			}
			atomic.AddInt64(&returned, 1)
		}(i)
	}
	// 没有可观测的"已入队"信号，只能给一小段时间让队列填满、
	// 其余投递方卡到 select 上。断言本身不依赖这个时长的精度。
	time.Sleep(100 * time.Millisecond)

	h.loader.Close() // 事件循环此刻仍卡在 Block 里，排空只能由 Close 这一侧完成
	waitAll(t, &wg, 5*time.Second, "关闭后所有调用方返回")

	if slow != 0 {
		t.Fatalf("%d 个调用方等满超时才返回：关闭时有任务没被结算", slow)
	}
	if returned != callers {
		t.Fatalf("只有 %d/%d 个调用方返回", returned, callers)
	}

	h.mod.release() // 放行 Block，事件循环才能退出
	h.wg.Wait()
}

// TestPanicWithSaturatedQueueUnblocksEveryone 压最难的那个交织：
// 模块方法 panic 时，队列是满的、还有一批投递方卡在 select 上。
//
// handleTask 的 recover 会在 actor 自己的协程里调 Close，
// 而 closeWith 要拿 enqueueMu 写锁——此时投递方正持着读锁卡在满队列上，
// 唯一的解法是 close(stopChan) 先于取锁执行。顺序反了就是死锁，谁也退不出去。
func TestPanicWithSaturatedQueueUnblocksEveryone(t *testing.T) {
	defer noLeak(t)()
	h := newHarness("panic-saturated")
	defer h.stop()

	h.blockActor(t)

	// Boom 抢在其它任务前面入队，放行后第一个被取出
	boomErr := make(chan error, 1)
	go func() {
		_, err := h.loader.ModInvoke(gateName, "Boom", 7)
		boomErr <- err
	}()
	time.Sleep(50 * time.Millisecond)

	const callers = 120 // 64 个占满队列，其余卡在投递上
	var slow int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			gid := actor.CurrentGID()
			start := time.Now()
			_, _ = h.loader.ModInvokeFrom(gid, gateName, "Echo", i)
			if time.Since(start) >= 2*time.Second {
				atomic.AddInt64(&slow, 1)
			}
		}(i)
	}
	time.Sleep(100 * time.Millisecond)

	// 放行：循环从 Block 里出来 → 取到 Boom → panic → recover → 关掉整个 actor
	h.mod.release()

	waitAll(t, &wg, 5*time.Second, "panic 后所有调用方返回")

	select {
	case err := <-boomErr:
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Fatalf("panic 应当作为错误回传给调用方，got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Boom 的调用方没有返回")
	}
	if slow != 0 {
		t.Fatalf("%d 个调用方等满超时：panic 关闭 actor 时漏了结算", slow)
	}
	if !h.loader.IsClose() {
		t.Fatal("模块方法 panic 之后 actor 应当已关闭")
	}
	h.wg.Wait() // 事件循环应当已自行退出
}

// TestNoLeakActorChurn 压 actor 的生命周期：反复建了停、停了建。
// 对应"每个玩家一个 actor、频繁上下线"的场景——只要 Close 漏掉一次，
// 峰值就会随轮次线性涨上去。
func TestNoLeakActorChurn(t *testing.T) {
	defer noLeak(t)()

	rounds := 300
	if testing.Short() {
		rounds = 50
	}
	base := runtime.NumGoroutine()
	peak := base

	for r := 0; r < rounds; r++ {
		h := newHarness(fmt.Sprintf("churn-%d", r))
		for i := 0; i < 5; i++ {
			out, err := h.loader.ModInvoke(gateName, "Echo", i)
			if err != nil || len(out) != 1 || int(out[0].Int()) != i {
				h.stop()
				t.Fatalf("第 %d 轮调用异常: %v", r, err)
			}
		}
		h.stop()
		if n := runtime.NumGoroutine(); n > peak {
			peak = n
		}
	}

	t.Logf("%d 轮建/停 actor：goroutine 基线 %d，峰值 %d，收尾 %d",
		rounds, base, peak, runtime.NumGoroutine())
	if peak > base+16 {
		t.Fatalf("goroutine 峰值 %d 远高于基线 %d：有 actor 没停干净", peak, base)
	}
}

// TestUpdateTickerNotStarvedUnderLoad 验证饱和时 1s 的 ticker 没被任务饿死。
//
// 事件循环用一个 select 同时等 stopChan/ticker.C/taskChan：taskChan 若持续就绪，
// Go 在两者都就绪时是随机选的——Update 会被推迟，但不该被饿死。
// 负载用 Fire（无返回值），既能打满队列又不会留下一堆 3s 定时器。
func TestUpdateTickerNotStarvedUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("需要持续加压 2.5s，-short 下跳过")
	}
	defer noLeak(t)()
	h := newHarness("ticker-starve")
	defer h.stop()

	stopLoad := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gid := actor.CurrentGID()
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				_, _ = h.loader.ModInvokeFrom(gid, gateName, "Fire", 1)
			}
		}()
	}
	time.Sleep(2500 * time.Millisecond)
	close(stopLoad)
	waitAll(t, &wg, 10*time.Second, "停止加压")

	if _, err := h.loader.ModInvoke(gateName, "Echo", 1); err != nil {
		t.Fatalf("加压后 actor 不健康: %v", err)
	}
	updates := h.mod.updates.Load()
	t.Logf("2.5s 满负荷期间执行了 %d 次 Fire，Update 触发 %d 次", h.mod.fired.Load(), updates)
	if updates < 1 {
		t.Fatal("满负荷时 Update 一次都没触发：ticker 被任务饿死了")
	}
}

// --- 协议分发 ---
//
// OnMessageHandler + 协议注册是这套框架分发前端消息的主路径：协议 ID 与响应函数
// 由 ModObj 在 Init 时自动绑定，省掉手写 switch。这条路径上每条消息都会走一次
// 遍历模块 + ModInvoke，量大且高频，是最不该漏测的地方。

type dispatchMsgA struct{ N int }
type dispatchMsgB struct{ N int }

const (
	dispatchIDA = 9001
	dispatchIDB = 9002
)

func init() {
	actor.RegisterProtocol(dispatchIDA, dispatchMsgA{})
	actor.RegisterProtocol(dispatchIDB, dispatchMsgB{})
}

// dispatchModA 只认 dispatchMsgA。ModObj 按"单入参 + 无返回值 + 入参类型已注册"
// 三条把 OnDispatchA 识别成协议处理器，写进 metaMsgHandler。
type dispatchModA struct {
	actor.ModObj[*dispatchModA]
	got atomic.Int64
}

func newDispatchModA() *dispatchModA {
	m := &dispatchModA{}
	m.Init()
	return m
}

func (m *dispatchModA) OnDispatchA(msg dispatchMsgA) { m.got.Add(int64(msg.N)) }

// Total 有返回值，用来做 FIFO 屏障。
func (m *dispatchModA) Total() int { return int(m.got.Load()) }

type dispatchModB struct {
	actor.ModObj[*dispatchModB]
	got atomic.Int64
}

func newDispatchModB() *dispatchModB {
	m := &dispatchModB{}
	m.Init()
	return m
}

func (m *dispatchModB) OnDispatchB(msg dispatchMsgB) { m.got.Add(int64(msg.N)) }

// TestNoLeakProtocolDispatch 并发灌协议消息，逐条对账。
//
// 协议处理器没有返回值，走的是"投递完就返回"的路径，任务由事件循环 complete 后
// 自行 Release——丢一条就对不上账。而 OnMessageHandler 把 ModInvoke 的错误直接
// 丢掉了（`_, _ =`），投递失败不会有任何声响，所以这里必须严格对账才看得见。
func TestNoLeakProtocolDispatch(t *testing.T) {
	defer noLeak(t)()

	loader := actor.NewActorLoader("dispatch")
	loader.Init()
	modA, modB := newDispatchModA(), newDispatchModB()
	loader.AddModule(modA)
	loader.AddModule(modB)

	var wg sync.WaitGroup
	loader.Start(&wg)
	defer func() {
		loader.Close()
		wg.Wait()
	}()

	// 先确认协议真的绑上了。少了这步，即使一条都没分发出去，下面的对账也可能"通过"。
	if got := modA.GetMetaHandler(dispatchIDA); got != "OnDispatchA" {
		t.Fatalf("协议 %d 没绑到 OnDispatchA, got %q", dispatchIDA, got)
	}
	if got := modB.GetMetaHandler(dispatchIDB); got != "OnDispatchB" {
		t.Fatalf("协议 %d 没绑到 OnDispatchB, got %q", dispatchIDB, got)
	}
	// 模块之间不该串台：A 不认 B 的协议
	if got := modA.GetMetaHandler(dispatchIDB); got != "" {
		t.Fatalf("dispatchModA 不该认领协议 %d, got %q", dispatchIDB, got)
	}

	workers := runtime.NumCPU()
	if workers < 4 {
		workers = 4
	}
	const perWorker = 500

	var sendWG sync.WaitGroup
	sendWG.Add(workers)
	start := time.Now()
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer sendWG.Done()
			for i := 0; i < perWorker; i++ {
				if w%2 == 0 {
					loader.OnMessageHandler(actor.NewProtocolMessage(dispatchIDA, dispatchMsgA{N: 1}, nil))
				} else {
					loader.OnMessageHandler(actor.NewProtocolMessage(dispatchIDB, dispatchMsgB{N: 1}, nil))
				}
			}
		}(w)
	}
	waitAll(t, &sendWG, 60*time.Second, "协议分发")
	elapsed := time.Since(start)

	// FIFO 屏障：这次同步调用返回时，排在它前面的协议任务必然都已执行完
	if _, err := loader.ModInvoke("dispatchModA", "Total"); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}

	total := workers * perWorker
	wantA := int64((workers + 1) / 2 * perWorker)
	wantB := int64(workers / 2 * perWorker)
	if got := modA.got.Load(); got != wantA {
		t.Fatalf("协议 A 收到 %d 条，期望 %d 条——有消息被静默丢弃", got, wantA)
	}
	if got := modB.got.Load(); got != wantB {
		t.Fatalf("协议 B 收到 %d 条，期望 %d 条——有消息被静默丢弃", got, wantB)
	}
	t.Logf("%d 条协议消息全部落地，耗时 %v，约 %.0f 条/秒（每条都要遍历模块并解析一次栈取 GID）",
		total, elapsed, float64(total)/elapsed.Seconds())
}

// BenchmarkProtocolDispatch 与 BenchmarkProtocolDispatchDirect 拆开量协议分发的开销。
//
// 两者最终都只是往队列里投一个无返回值任务，差额全在 OnMessageHandler 自己身上：
// 它每条消息都要在锁里把所有模块拷进一个新切片，再对每个命中的模块走一次 ModInvoke
// （而 ModInvoke 又要 currentGID，即解析一次调用栈）。模块越多，这笔固定开销越大，
// 而它落在每条前端消息上。
//
//	go test ./tests/goroutine_leak/ -bench=BenchmarkProtocolDispatch -benchmem -run XXX
func BenchmarkProtocolDispatch(b *testing.B) {
	loader, modA, stop := newDispatchActor("bench-dispatch")
	defer stop()
	_ = modA

	msg := actor.NewProtocolMessage(dispatchIDA, dispatchMsgA{N: 1}, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loader.OnMessageHandler(msg)
	}
}

// BenchmarkProtocolDispatchFrom 走缓存 GID 的分发入口。
// 与上面的差额就是 currentGID 那一次 runtime.Stack——长期存活的网络读协程
// 只要把 GID 缓存一次，这笔钱就完全不用付。
func BenchmarkProtocolDispatchFrom(b *testing.B) {
	loader, _, stop := newDispatchActor("bench-dispatch-from")
	defer stop()

	msg := actor.NewProtocolMessage(dispatchIDA, dispatchMsgA{N: 1}, nil)
	gid := actor.CurrentGID()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loader.OnMessageHandlerFrom(gid, msg)
	}
}

// BenchmarkProtocolDispatchDirect 绕开 OnMessageHandler，直接投给已知的处理器，
// 并复用缓存好的 GID——这是同一条投递路径的下限。
func BenchmarkProtocolDispatchDirect(b *testing.B) {
	loader, modA, stop := newDispatchActor("bench-dispatch-direct")
	defer stop()

	method := modA.GetMetaHandler(dispatchIDA)
	gid := actor.CurrentGID()
	msg := dispatchMsgA{N: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := loader.ModInvokeFrom(gid, "dispatchModA", method, msg); err != nil {
			b.Fatalf("invoke failed: %v", err)
		}
	}
}

func newDispatchActor(name string) (*actor.ActorLoader, *dispatchModA, func()) {
	loader := actor.NewActorLoader(name)
	loader.Init()
	modA := newDispatchModA()
	loader.AddModule(modA)
	loader.AddModule(newDispatchModB())
	// 收尾时关闭 actor，队列里还没跑完的消息会被判为丢弃。这里接管掉，
	// 免得框架的兜底 stderr 日志插进基准输出把结果行冲散。
	var discarded atomic.Int64
	loader.SetDiscardedErrorHandler(func(actor.DiscardedError) { discarded.Add(1) })
	var wg sync.WaitGroup
	loader.Start(&wg)
	return loader, modA, func() {
		loader.Close()
		wg.Wait()
	}
}

// --- 需要等满框架 3s 超时的用例 ---

// TestTimeoutStormNoLeakAndPoolIntegrity 同时压两条超时路径，再验池子有没有被串用。
//
// actor 全程被 Block 钉死：抢到 64 个槽的调用方等 Await 超时，
// 没抢到的等投递超时。前者每人留下一个清理协程挂在 <-doneCh 上——
// 放行后事件循环必须把它们逐个 complete，否则泄漏数正好等于 awaitTimeout。
//
// 放行之后立刻并发打新调用：那一刻正是超时任务批量回池的时刻，
// 若有晚到的 complete 写进了被复用的 ChanTask，结果就会串味。
func TestTimeoutStormNoLeakAndPoolIntegrity(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等满框架默认的 3s 超时，-short 下跳过")
	}
	defer noLeak(t)()
	h := newHarness("timeout-storm")
	defer h.stop()

	h.blockActor(t)

	const callers = 200
	var awaitTimeout, queueTimeout, other, unexpectedOK, canceled int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			gid := actor.CurrentGID()
			_, err := h.loader.ModInvokeFrom(gid, gateName, "Echo", i)
			if errors.Is(err, actor.ErrTaskCanceled) {
				atomic.AddInt64(&canceled, 1)
			}
			switch {
			case err == nil:
				atomic.AddInt64(&unexpectedOK, 1)
			case errors.Is(err, actor.ErrTaskAwaitTimeout):
				atomic.AddInt64(&awaitTimeout, 1)
			case errors.Is(err, actor.ErrTaskQueueTimeout):
				atomic.AddInt64(&queueTimeout, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}(i)
	}
	waitAll(t, &wg, 10*time.Second, "超时风暴中所有调用方返回")

	t.Logf("超时分布: await=%d queue=%d 其它=%d 意外成功=%d",
		awaitTimeout, queueTimeout, other, unexpectedOK)
	if unexpectedOK != 0 || other != 0 {
		t.Fatalf("actor 全程被堵死，不该有成功或其它类型的错误")
	}
	if awaitTimeout == 0 || queueTimeout == 0 {
		t.Fatalf("没能同时压到两条超时路径（64 槽 vs %d 个调用方）", callers)
	}
	// 入队成功那批（awaitTimeout 个）应当已经被调用方标成取消，全都带 ErrTaskCanceled；
	// 卡在投递上那批压根没进队列，不存在取消一说。
	if canceled != awaitTimeout {
		t.Fatalf("%d 个入队后超时的调用里只有 %d 个被就地取消", awaitTimeout, canceled)
	}

	// 放行前先记账：那批被取消的任务还躺在队列里，事件循环恢复后必须逐个跳过。
	echoedBefore := h.mod.echoed.Load()
	h.mod.release()

	// FIFO 屏障：这次调用排在所有被取消的任务后面，它一返回就说明那批全被处理过了。
	if _, err := h.loader.ModInvoke(gateName, "Echo", -1); err != nil {
		t.Fatalf("放行后的屏障调用失败: %v", err)
	}
	if got, want := h.mod.echoed.Load(), echoedBefore+1; got != want {
		t.Fatalf("方法体被执行了 %d 次，期望 %d 次——被取消的任务补跑了 %d 次",
			got, want, got-want)
	}

	const checkers, perChecker = 8, 400
	var mismatch int64
	var wg2 sync.WaitGroup
	wg2.Add(checkers)
	for c := 0; c < checkers; c++ {
		go func(c int) {
			defer wg2.Done()
			gid := actor.CurrentGID()
			for i := 0; i < perChecker; i++ {
				want := c*perChecker + i
				out, err := h.loader.ModInvokeFrom(gid, gateName, "Echo", want)
				if err != nil || len(out) != 1 || int(out[0].Int()) != want {
					atomic.AddInt64(&mismatch, 1)
					return
				}
			}
		}(c)
	}
	waitAll(t, &wg2, 60*time.Second, "超时风暴后的健康检查")
	if mismatch != 0 {
		t.Fatalf("风暴过后有 %d 个调用拿错了结果：ChanTask 池被串用了", mismatch)
	}
}

// TestNoLeakCloseDrainsAbandonedTasks 覆盖"被取消的任务还躺在队列里时关闭 actor"。
//
// 调用方超时后把任务标成 Abandoned 就走了，任务本身还排在队列里等着被跳过。
// 这时候关闭 actor，排空必须能结算 Abandoned 状态的任务——complete 若只认 Pending，
// done 就永远不会关闭，每个被取消的任务都会永久挂住一个清理协程，
// ChanTask 也全都回不了池。
func TestNoLeakCloseDrainsAbandonedTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等满框架默认的 3s 超时，-short 下跳过")
	}
	defer noLeak(t)()
	h := newHarness("close-abandoned")
	defer h.stop()

	h.blockActor(t)

	const callers = 40 // 小于 64 槽，保证全部入队成功、随后全部被取消
	var canceled int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			gid := actor.CurrentGID()
			_, err := h.loader.ModInvokeFrom(gid, gateName, "Echo", i)
			if errors.Is(err, actor.ErrTaskCanceled) {
				atomic.AddInt64(&canceled, 1)
			}
		}(i)
	}
	waitAll(t, &wg, 10*time.Second, "调用方超时并取消各自的任务")
	if canceled != callers {
		t.Fatalf("只有 %d/%d 个任务被就地取消，用例前提不成立", canceled, callers)
	}

	// 不放行 Block，直接关闭：队列里全是 Abandoned 状态的任务，
	// 事件循环还卡着，只能由 closeWith 这一侧的排空来结算它们。
	h.loader.Close()

	h.mod.release() // 放行 Block，事件循环才退得出来
	h.wg.Wait()

	if got := h.mod.echoed.Load(); got != 0 {
		t.Fatalf("被取消的任务执行了 %d 次，一次都不该执行", got)
	}
	t.Logf("%d 个被取消的任务在关闭排空中完成结算，方法体一次都没跑", canceled)
}

// --- 跨 actor / 重入 ---

// relayMod 在自己的 actor 协程里同步调用对端 actor。
type relayMod struct {
	actor.ModObj[*relayMod]
	peer  *actor.ActorLoader // 只在 Start 之前赋值，之后只在本 actor 协程上读
	calls atomic.Int64
}

func newRelayMod() *relayMod {
	m := &relayMod{}
	m.Init()
	return m
}

// Ping 把调用接力给对端。对端若再回调回来，本 actor 正卡在 Await 上、
// 消费不了任务，就是死锁——只能等 defaultTaskTimeout 兜底。
func (m *relayMod) Ping(n int) (int, error) {
	m.calls.Add(1)
	if n <= 0 || m.peer == nil {
		return n, nil
	}
	out, err := m.peer.ModInvoke(relayName, "Ping", n-1)
	if err != nil {
		return -1, err
	}
	if len(out) != 2 {
		return -1, fmt.Errorf("对端返回值个数异常: %d", len(out))
	}
	if e, _ := out[1].Interface().(error); e != nil {
		return -1, e
	}
	return int(out[0].Int()) + 1, nil
}

func startRelay(name string, wg *sync.WaitGroup, peer *actor.ActorLoader) *actor.ActorLoader {
	l := actor.NewActorLoader(name)
	l.Init()
	m := newRelayMod()
	m.peer = peer // Start 之前写，之后只读，没有竞态
	l.AddModule(m)
	l.Start(wg)
	return l
}

// TestCrossActorChainNoDeadlock 对照组：非环形的链式同步调用一切正常。
func TestCrossActorChainNoDeadlock(t *testing.T) {
	defer noLeak(t)()

	var wg sync.WaitGroup
	c := startRelay("relay-c", &wg, nil)
	b := startRelay("relay-b", &wg, c)
	a := startRelay("relay-a", &wg, b)
	defer func() {
		a.Close()
		b.Close()
		c.Close()
		wg.Wait()
	}()

	start := time.Now()
	out, err := a.ModInvoke(relayName, "Ping", 2)
	if err != nil {
		t.Fatalf("链式调用失败: %v", err)
	}
	if e, _ := out[1].Interface().(error); e != nil {
		t.Fatalf("链式调用内层报错: %v", e)
	}
	if got := int(out[0].Int()); got != 2 {
		t.Fatalf("a→b→c 应当接力两跳，got %d", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("链式调用不该慢到 %v", d)
	}
}

// TestCrossActorCycleRejectedWithoutLeak 覆盖环检测这条路径的资源收尾。
//
// 两个 actor 互相同步调用是这套框架的一条硬边界：a 的事件循环卡在等 b 的结果时
// 消费不了自己的队列，b 回调过来的任务只能干躺着。以前只能等满 defaultTaskTimeout
// 才解开，整条环停摆 3 秒；现在由等待图当场拦下。
//
// 这里关心的是拦下之后有没有留下垃圾：被拒的调用根本没建任务、没投递，
// 不该有 ChanTask 漏出池，也不该有清理协程挂着；两个 actor 还得立刻能干活。
// 检测语义本身（两环、三环、异步回调不误伤、同时互调）在根包的
// actor_loader_cycle_test.go 里确定性覆盖。
func TestCrossActorCycleRejectedWithoutLeak(t *testing.T) {
	defer noLeak(t)()

	var wg sync.WaitGroup
	a := actor.NewActorLoader("cycle-a")
	b := actor.NewActorLoader("cycle-b")
	a.Init()
	b.Init()
	ma, mb := newRelayMod(), newRelayMod()
	ma.peer, mb.peer = b, a // 成环
	a.AddModule(ma)
	b.AddModule(mb)
	a.Start(&wg)
	b.Start(&wg)
	defer func() {
		a.Close()
		b.Close()
		wg.Wait()
	}()

	// 反复撞环，把"每撞一次漏一点"的问题放大出来
	const rounds = 200
	for i := 0; i < rounds; i++ {
		start := time.Now()
		out, err := a.ModInvoke(relayName, "Ping", 2)
		if err != nil {
			t.Fatalf("第 %d 轮外层调用不该失败: %v", i, err)
		}
		inner, _ := out[1].Interface().(error)
		if !errors.Is(inner, actor.ErrCallCycle) {
			t.Fatalf("第 %d 轮环形调用应当被拦下，got %v", i, inner)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("第 %d 轮耗时 %v：退化成超时兜底了", i, d)
		}
	}

	// 撞完环两个 actor 都要立刻可用
	for name, l := range map[string]*actor.ActorLoader{"a": a, "b": b} {
		out, err := l.ModInvoke(relayName, "Ping", 0)
		if err != nil {
			t.Fatalf("actor %s 在环被拦下后不可用: %v", name, err)
		}
		if int(out[0].Int()) != 0 {
			t.Fatalf("actor %s 状态异常", name)
		}
	}

	// 被拒的那一跳压根没执行到 b 的模块方法体，a 只执行了每轮的 Ping(2) 加最后一次健康检查
	if got, want := ma.calls.Load(), int64(rounds+1); got != want {
		t.Fatalf("a 执行了 %d 次 Ping，期望 %d 次", got, want)
	}
	if got := mb.calls.Load(); got != rounds+1 {
		t.Fatalf("b 执行了 %d 次 Ping，期望 %d 次", got, rounds+1)
	}
}

// TestNoLeakCloseActorAwaitedByAnotherActor 关闭一个正被另一个 actor 同步等待的 actor。
//
// 这是最容易漏的一处交织：a 的事件循环卡在等 b 的返回值上——此刻 a 自己的队列
// 也没人消费，a 事实上整个停摆了。关闭 b 时，排空必须把 a 那个还排在 b 队列里的
// 任务结算掉，a 才能醒过来；漏了的话 a 要干等满 3s，期间它自己的调用方全部被拖住，
// a 的等待边也一直挂在环检测的等待图上，别人查图会一路走进一个已经死掉的 actor。
func TestNoLeakCloseActorAwaitedByAnotherActor(t *testing.T) {
	defer noLeak(t)()

	var wg sync.WaitGroup

	// b 同时挂两个模块：relayMod 接 a 的同步调用，gateMod 用来把 b 的事件循环钉死，
	// 好让 a 那个任务只能排队。
	b := actor.NewActorLoader("awaited-b")
	b.Init()
	bGate := newGateMod()
	b.AddModule(newRelayMod()) // peer 为 nil，Ping 到此为止
	b.AddModule(bGate)
	b.Start(&wg)

	a := actor.NewActorLoader("awaited-a")
	a.Init()
	ma := newRelayMod()
	ma.peer = b
	a.AddModule(ma)
	a.Start(&wg)

	defer func() {
		bGate.release()
		a.Close()
		b.Close()
		wg.Wait()
	}()

	// 1) 钉死 b 的事件循环
	go b.ModInvoke(gateName, "Block", 1) //nolint:errcheck
	select {
	case <-bGate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("b 的事件循环没能进入 Block")
	}

	// 2) a 的事件循环去同步问 b 要结果，任务排在 Block 后面，a 就此停摆
	outerErr := make(chan error, 1)
	go func() {
		out, err := a.ModInvoke(relayName, "Ping", 1)
		if err != nil {
			outerErr <- err
			return
		}
		inner, _ := out[1].Interface().(error)
		outerErr <- inner
	}()
	// 没有可观测的"已入队"信号，给一小段时间让 a 的调用落进 b 的队列。
	// 断言本身不依赖这个时长的精度。
	time.Sleep(100 * time.Millisecond)

	// 3) 关掉 b。a 那个任务还在 b 的队列里，只能靠排空结算
	start := time.Now()
	b.Close()

	select {
	case err := <-outerErr:
		if err == nil {
			t.Fatal("b 已经关闭，a 的调用不该成功")
		}
		if errors.Is(err, actor.ErrTaskAwaitTimeout) {
			t.Fatalf("a 等满了超时才返回（%v）：关闭 b 时漏了结算它队列里的任务",
				time.Since(start))
		}
		t.Logf("b 关闭后 %v 内 a 就被唤醒，拿到 %v", time.Since(start), err)
	case <-time.After(2 * time.Second):
		t.Fatal("b 关闭后 a 仍未被唤醒")
	}

	// 4) a 必须立刻恢复服务——它自己没被关，只是刚才被钉住了
	out, err := a.ModInvoke(relayName, "Ping", 0)
	if err != nil {
		t.Fatalf("a 在对端关闭后不可用: %v", err)
	}
	if int(out[0].Int()) != 0 {
		t.Fatalf("a 状态异常: %v", out)
	}
}

// selfMod 在模块方法里回调自己所属的 loader。
type selfMod struct {
	actor.ModObj[*selfMod]
	self *actor.ActorLoader
}

func newSelfMod() *selfMod {
	m := &selfMod{}
	m.Init()
	return m
}

// Recurse 递归调用自己。调用方 GID 与 actor 自身的 GID 相同，
// ModInvokeFrom 会走 directInvoke，直接在当前栈上执行，不入队。
func (m *selfMod) Recurse(n int) int {
	if n <= 0 {
		return 0
	}
	out, err := m.self.ModInvoke(selfName, "Recurse", n-1)
	if err != nil || len(out) != 1 {
		return -1 << 30 // 让上层的累加结果明显跑飞
	}
	return int(out[0].Int()) + 1
}

// RecurseFast 与 Recurse 等价，只是不再让 ModInvoke 去 currentGID 解析栈：
// 模块方法本来就跑在 actor 自己的协程上，GetGoroutineID() 就是当前 GID，
// 一次原子读即可。用它跟 Recurse 对比，能量出 currentGID 在重入路径上的占比。
func (m *selfMod) RecurseFast(n int) int {
	if n <= 0 {
		return 0
	}
	out, err := m.self.ModInvokeFrom(m.self.GetGoroutineID(), selfName, "RecurseFast", n-1)
	if err != nil || len(out) != 1 {
		return -1 << 30
	}
	return int(out[0].Int()) + 1
}

// TestReentrantSelfInvokeDoesNotDeadlock 验证同 actor 内的重入调用不会自死锁：
// 同协程走 directInvoke，不经过队列，所以深递归也安全。
//
// 但它有个很贵的隐性成本：ModInvoke 每次都要 currentGID()，而 currentGID 靠
// runtime.Stack 解析——栈越深它越慢，重入路径恰恰是把栈越堆越高。
// 用例量两个深度看成本怎么涨，再跟 ModInvokeFrom(GetGoroutineID(), ...) 对照，
// 把 currentGID 在其中的占比算出来。
//
// 另有一条隐含上限：整段递归跑在一次 ModInvoke 的 3s 预算里，
// 递归太深会直接把外层调用方拖到超时。
func TestReentrantSelfInvokeDoesNotDeadlock(t *testing.T) {
	defer noLeak(t)()

	loader := actor.NewActorLoader("reentrant")
	loader.Init()
	m := newSelfMod()
	m.self = loader
	loader.AddModule(m)

	var wg sync.WaitGroup
	loader.Start(&wg)
	defer func() {
		loader.Close()
		wg.Wait()
	}()

	// repeat 是为了让总耗时远高于时钟粒度：Windows 上 time.Now 偶尔只有
	// 十几毫秒的分辨率，单次几百微秒的测量会直接量成 0。
	measure := func(method string, depth, repeat int) time.Duration {
		t.Helper()
		start := time.Now()
		for r := 0; r < repeat; r++ {
			out, err := loader.ModInvoke(selfName, method, depth)
			if err != nil {
				t.Fatalf("%s 递归 %d 层失败: %v", method, depth, err)
			}
			if got := int(out[0].Int()); got != depth {
				t.Fatalf("%s 重入深度对不上: got %d, want %d", method, got, depth)
			}
		}
		elapsed := time.Since(start)
		per := elapsed / time.Duration(depth*repeat)
		t.Logf("%-12s 递归 %3d 层 ×%d 共 %-14v 每层约 %v", method, depth, repeat, elapsed, per)
		return per
	}

	shallow := measure("Recurse", 100, 5)
	deep := measure("Recurse", 500, 1)
	fast := measure("RecurseFast", 500, 100)

	t.Logf("深度 100→500，每层成本 %v→%v（×%.1f）：currentGID 的 runtime.Stack 要走栈，重入越深越贵",
		shallow, deep, float64(deep)/float64(shallow))
	t.Logf("同样 500 层，改用 ModInvokeFrom(GetGoroutineID()) 后每层 %v，快约 %.0f 倍——"+
		"模块方法本就跑在 actor 协程上，没必要每层再解析一次栈",
		fast, float64(deep)/float64(fast))

	if fast <= 0 || fast >= deep {
		t.Fatalf("缓存 GID 的重入路径不该比 currentGID 版本更慢: %v vs %v", fast, deep)
	}
}

// --- 重压用例（ACTOR_STRESS=1） ---

// TestExtremeManyConcurrentActors 同时拉起大批 actor 并交叉调用，
// 对应"大量玩家同时在线"。每个 actor 一条 goroutine 加一个 1s ticker，
// 这里要看的是它们能不能全部正常服务、并在收尾时干净退出。
func TestExtremeManyConcurrentActors(t *testing.T) {
	requireStress(t)
	defer noLeak(t)()

	const actors = 2000
	base := runtime.NumGoroutine()

	hs := make([]*harness, actors)
	for i := range hs {
		hs[i] = newHarness(fmt.Sprintf("mesh-%d", i))
	}
	afterStart := runtime.NumGoroutine()

	callers := runtime.NumCPU() * 2
	const perCaller = 2000
	var okCount, errCount int64
	var wg sync.WaitGroup
	wg.Add(callers)
	start := time.Now()
	for c := 0; c < callers; c++ {
		go func(c int) {
			defer wg.Done()
			gid := actor.CurrentGID()
			for i := 0; i < perCaller; i++ {
				// 用互质步长打散，让每个调用方均匀地敲遍所有 actor
				h := hs[(c*7919+i*31)%actors]
				out, err := h.loader.ModInvokeFrom(gid, gateName, "Echo", i)
				if err != nil || len(out) != 1 || int(out[0].Int()) != i {
					atomic.AddInt64(&errCount, 1)
					continue
				}
				atomic.AddInt64(&okCount, 1)
			}
		}(c)
	}
	waitAll(t, &wg, 180*time.Second, "多 actor 交叉调用")
	elapsed := time.Since(start)
	peak := runtime.NumGoroutine()

	for _, h := range hs {
		h.stop()
	}

	t.Logf("%d 个 actor 同时在线：goroutine %d → %d（峰值 %d）；"+
		"%d 个调用方共 %d 次调用耗时 %v，约 %.0f ops/s，失败 %d",
		actors, base, afterStart, peak, callers, okCount+errCount, elapsed,
		float64(okCount+errCount)/elapsed.Seconds(), errCount)

	if errCount != 0 {
		t.Fatalf("多 actor 交叉调用出现 %d 次失败", errCount)
	}
	if afterStart-base < actors {
		t.Fatalf("只起来了 %d 个 actor 协程，期望 %d", afterStart-base, actors)
	}
}

// TestExtremeSingleActorThroughput 量单个 actor 的吞吐上限与延迟分布，
// 同时守住一条容易退化的内存性质：ChanTask 拿到结果后必须把超时定时器 Stop 掉。
//
// 不 Stop 的话，每次跨协程调用都会留下一个要等满 3s 才到期的定时器，
// 闭包连带把 ChanTask 和它的两个 channel 一起吊住——压测期间堆会被顶得很高
// （这台机器上 28 万次调用曾把堆从 45MB 顶到 172MB，还带出 79ms 的 GC 长尾），
// 而 Stop 之后同样的压测只多出 2MB 出头。用例对压测中的堆增量设了上限，
// 万一哪天 Stop 被去掉，这里会先炸。
func TestExtremeSingleActorThroughput(t *testing.T) {
	requireStress(t)
	defer noLeak(t)()
	h := newHarness("throughput")
	defer h.stop()

	workers := runtime.NumCPU()
	const perWorker = 10000
	total := workers * perWorker

	var before, afterBurst, afterDrain runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	lat := make([][]time.Duration, workers)
	var errCount int64
	var wg sync.WaitGroup
	wg.Add(workers)
	start := time.Now()
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			gid := actor.CurrentGID()
			buf := make([]time.Duration, 0, perWorker)
			for i := 0; i < perWorker; i++ {
				t0 := time.Now()
				out, err := h.loader.ModInvokeFrom(gid, gateName, "Echo", i)
				buf = append(buf, time.Since(t0))
				if err != nil || len(out) != 1 || int(out[0].Int()) != i {
					atomic.AddInt64(&errCount, 1)
				}
			}
			lat[w] = buf
		}(w)
	}
	waitAll(t, &wg, 180*time.Second, "单 actor 吞吐压测")
	elapsed := time.Since(start)
	runtime.ReadMemStats(&afterBurst)

	all := make([]time.Duration, 0, total)
	zeroSamples := 0
	for _, b := range lat {
		for _, d := range b {
			if d == 0 {
				zeroSamples++
			}
		}
		all = append(all, b...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	t.Logf("%d 个调用方 × %d 次 = %d 次调用，耗时 %v，约 %.0f ops/s，单次均值 %v",
		workers, perWorker, total, elapsed, float64(total)/elapsed.Seconds(),
		elapsed/time.Duration(perWorker))
	// 低分位常常量成 0：Windows 的单调时钟粒度（约 0.5~1ms）远大于单次调用耗时，
	// 所以低分位只能说明"快到量不出来"，真正有信息量的是均值和高分位。
	t.Logf("延迟 p50=%v p90=%v p99=%v p999=%v max=%v（%d/%d 个样本因时钟粒度量成 0）",
		pct(all, 0.50), pct(all, 0.90), pct(all, 0.99), pct(all, 0.999), all[len(all)-1],
		zeroSamples, total)
	burstGrow := int64(afterBurst.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("堆占用：压测前 %.1fMB → 压测刚结束 %.1fMB（%d 次调用共多出 %.1fMB，"+
		"合每次在途调用 %.0fB）",
		float64(before.HeapAlloc)/(1<<20), float64(afterBurst.HeapAlloc)/(1<<20),
		total, float64(burstGrow)/(1<<20), float64(burstGrow)/float64(total))

	if errCount != 0 {
		t.Fatalf("%d 次调用失败或结果串味", errCount)
	}
	// 定时器若没被 Stop，这批调用会把堆顶高一两个数量级（实测 +127MB）。
	// 阈值放到 64MB，留足 GC 时机和机器差异的裕量，只拦真正的退化。
	if burstGrow > 64<<20 {
		t.Errorf("压测期间堆多出 %.1fMB：ChanTask 的超时定时器多半没在成功路径上被 Stop",
			float64(burstGrow)/(1<<20))
	}

	// 再等一个超时周期确认没有滞留：成功返回的调用不该在这段时间里
	// 还挂着任何定时器，堆应当回到基线附近。
	time.Sleep(timeoutSlack)
	runtime.GC()
	runtime.ReadMemStats(&afterDrain)
	t.Logf("再等一个超时周期并 GC 后：%.1fMB", float64(afterDrain.HeapAlloc)/(1<<20))

	if grow := int64(afterDrain.HeapAlloc) - int64(before.HeapAlloc); grow > 64<<20 {
		t.Errorf("静置后堆仍比压测前多 %.1fMB，疑似有对象没被释放", float64(grow)/(1<<20))
	}
}
