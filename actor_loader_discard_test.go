package actor

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mismatchMsg 与 lateMsg 结构不同，无法转换。
// 注意不能拿 dupMsg 当反例：它和 lateMsg 底层都是 struct{ N int }，
// ModObj.Invoke 的 ConvertibleTo 兜底会直接把它转过去，一点错都不报。
type mismatchMsg struct{ S string }

// blockingMod 用来把事件循环钉死，好把队列灌满。
type blockingMod struct {
	ModObj[*blockingMod]
	gate        chan struct{}
	releaseOnce sync.Once
	entered     chan struct{}
	handled     atomic.Int64
}

func newBlockingMod() *blockingMod {
	m := &blockingMod{gate: make(chan struct{}), entered: make(chan struct{}, 4)}
	m.Init()
	return m
}

func (m *blockingMod) Block(x int) int {
	select {
	case m.entered <- struct{}{}:
	default:
	}
	<-m.gate
	return x
}

// OnLate 是协议处理器，无返回值——正是"错误无处可返"的那类调用。
func (m *blockingMod) OnLate(msg lateMsg) { m.handled.Add(int64(msg.N)) }

func (m *blockingMod) Total() int { return int(m.handled.Load()) }

func (m *blockingMod) release() { m.releaseOnce.Do(func() { close(m.gate) }) }

func startBlockingActor(name string) (*ActorLoader, *blockingMod, func()) {
	l := NewActorLoader(name)
	l.Init()
	m := newBlockingMod()
	l.AddModule(m)
	var wg sync.WaitGroup
	l.Start(&wg)
	var once sync.Once
	return l, m, func() {
		once.Do(func() {
			m.release()
			l.Close()
			wg.Wait()
		})
	}
}

// collectDiscarded 装一个回调，把失败都收进切片。
func collectDiscarded(l *ActorLoader) (*[]DiscardedError, *sync.Mutex) {
	var mu sync.Mutex
	got := make([]DiscardedError, 0, 8)
	l.SetDiscardedErrorHandler(func(e DiscardedError) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	})
	return &got, &mu
}

// TestDiscardedOnInvokeFailure 覆盖最隐蔽的那一类：协议 ID 与消息类型对不上。
//
// 处理器是无返回值的，走跨协程路径时反射报的"参数类型不匹配"产生在事件循环那侧，
// 没有任何调用方在等结果。改之前这个错误会随任务一起回池，消息凭空消失——
// 既不报错也不崩，只能靠人肉发现某条协议"没反应"。
func TestDiscardedOnInvokeFailure(t *testing.T) {
	loader, mod, stop := startBlockingActor("discard-invoke")
	defer stop()
	got, mu := collectDiscarded(loader)

	// lateMsgID 注册的是 lateMsg，这里故意塞一个结构完全不同的载荷进去
	loader.OnMessageHandler(NewProtocolMessage(lateMsgID, mismatchMsg{S: "x"}, nil))

	// FIFO 屏障：这次同步调用返回时，上面那条消息必然已经被事件循环处理过了
	if _, err := loader.ModInvoke("blockingMod", "Total"); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("期望上报 1 次，实际 %d 次: %v", len(*got), *got)
	}
	e := (*got)[0]
	if e.Phase != PhaseInvoke {
		t.Fatalf("类型不匹配是执行阶段的失败，got phase=%v", e.Phase)
	}
	if e.ModName != "blockingMod" || e.MethodName != "OnLate" {
		t.Fatalf("上报的目标不对: %s.%s", e.ModName, e.MethodName)
	}
	if !strings.Contains(e.Err.Error(), "type mismatch") {
		t.Fatalf("期望参数类型不匹配，got %v", e.Err)
	}
	if n := loader.DiscardedErrors(); n != 1 {
		t.Fatalf("计数器 = %d, 期望 1", n)
	}
	if mod.handled.Load() != 0 {
		t.Fatal("类型不匹配的消息不该被处理器收到")
	}
}

// TestDiscardedOnQueueFull 队列打满导致投递失败：这条前端消息被丢了，必须留下痕迹。
func TestDiscardedOnQueueFull(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等满框架默认的 3s 投递超时，-short 下跳过")
	}
	loader, mod, stop := startBlockingActor("discard-queuefull")
	defer stop()
	got, mu := collectDiscarded(loader)

	// 先把事件循环钉死，后续任务只能排队
	go loader.ModInvoke("blockingMod", "Block", 1) //nolint:errcheck
	select {
	case <-mod.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("事件循环没能进入 Block")
	}

	// 先灌满 64 槽的队列（瞬间完成），再并发投溢出的那批——
	// 它们会各自卡满投递超时后被丢弃。并发投是为了让这几条 3s 并行掉，
	// 串行的话用例要跑十几秒。
	const queueCap, overflow = 64, 8
	for i := 0; i < queueCap; i++ {
		loader.OnMessageHandler(NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))
	}
	var senders sync.WaitGroup
	senders.Add(overflow)
	for i := 0; i < overflow; i++ {
		go func() {
			defer senders.Done()
			loader.OnMessageHandler(NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))
		}()
	}
	senders.Wait()
	const flood = queueCap + overflow

	mu.Lock()
	n := len(*got)
	var sample DiscardedError
	if n > 0 {
		sample = (*got)[0]
	}
	mu.Unlock()

	if n == 0 {
		t.Fatal("队列打满时投递失败没有被上报——消息就这么静默消失了")
	}
	if sample.Phase != PhaseDeliver {
		t.Fatalf("投递失败应当是 deliver 阶段, got %v", sample.Phase)
	}
	// Unwrap 要能穿透到底层原因，业务才能按类型分流处理
	if !errors.Is(sample, ErrTaskQueueTimeout) {
		t.Fatalf("期望能 errors.Is 到 ErrTaskQueueTimeout, got %v", sample.Err)
	}
	if uint64(n) != loader.DiscardedErrors() {
		t.Fatalf("回调 %d 次与计数器 %d 对不上", n, loader.DiscardedErrors())
	}
	t.Logf("灌入 %d 条，%d 条因队列打满被丢弃并上报", flood, n)
}

// TestDiscardedOnClosedActor actor 已关闭后到达的消息同样是被丢弃。
func TestDiscardedOnClosedActor(t *testing.T) {
	loader, _, stop := startBlockingActor("discard-closed")
	got, mu := collectDiscarded(loader)
	stop() // 关闭并等事件循环退出

	loader.OnMessageHandler(NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("关闭后到达的消息应当上报一次, got %d", len(*got))
	}
	if (*got)[0].Phase != PhaseDeliver {
		t.Fatalf("应当是 deliver 阶段, got %v", (*got)[0].Phase)
	}
}

// TestDiscardedOnCloseDrainsQueue 关闭时队列里还压着无返回值任务：
// 它们投递是成功的，但一次都没被执行，属于 deliver 阶段的丢弃。
//
// 这条容易标错：排空复用的是同一个结算出口，若沿用"执行失败"的阶段，
// 一次正常关闭就会报成一堆 invoke 错误，把排查方向带偏——
// 一个是过载/关闭，一个是代码有问题，处置方式完全不同。
func TestDiscardedOnCloseDrainsQueue(t *testing.T) {
	loader, mod, stop := startBlockingActor("discard-drain")
	defer stop()
	got, mu := collectDiscarded(loader)

	// 钉死事件循环，后续协议消息只能在队列里排着
	go loader.ModInvoke("blockingMod", "Block", 1) //nolint:errcheck
	select {
	case <-mod.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("事件循环没能进入 Block")
	}

	const queued = 20
	for i := 0; i < queued; i++ {
		loader.OnMessageHandler(NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))
	}

	loader.Close() // 排空这一侧结算它们
	stop()         // 放行 Block，等事件循环退出

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != queued {
		t.Fatalf("关闭时队列里的 %d 条应当全部上报, got %d", queued, len(*got))
	}
	for i, e := range *got {
		if e.Phase != PhaseDeliver {
			t.Fatalf("第 %d 条被标成了 %v：排空的任务根本没执行过，只能是 deliver", i, e.Phase)
		}
	}
	if mod.handled.Load() != 0 {
		t.Fatalf("排空掉的任务不该被执行, handled=%d", mod.handled.Load())
	}
}

// TestDiscardedQuietOnSuccess 正常路径一次都不该上报——
// 误报会让这个信号失去意义，监控上全是噪声就等于没有监控。
func TestDiscardedQuietOnSuccess(t *testing.T) {
	loader, mod, stop := startBlockingActor("discard-quiet")
	defer stop()
	got, mu := collectDiscarded(loader)

	const rounds = 200
	for i := 0; i < rounds; i++ {
		loader.OnMessageHandler(NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))
	}
	// 没人认领的协议只是丢弃，不算失败，也不该上报
	loader.OnMessageHandler(NewProtocolMessage(999999, lateMsg{N: 1}, nil))

	if _, err := loader.ModInvoke("blockingMod", "Total"); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Fatalf("正常路径不该有任何上报, got %v", *got)
	}
	if n := loader.DiscardedErrors(); n != 0 {
		t.Fatalf("计数器应当为 0, got %d", n)
	}
	if mod.handled.Load() != rounds {
		t.Fatalf("处理器收到 %d 条，期望 %d 条", mod.handled.Load(), rounds)
	}
}

// TestDiscardedHandlerCanBeCleared 取消接管之后回落到框架自己的兜底日志，
// 计数器照常累加。
func TestDiscardedHandlerCanBeCleared(t *testing.T) {
	loader, _, stop := startBlockingActor("discard-clear")
	defer stop()
	got, mu := collectDiscarded(loader)
	loader.SetDiscardedErrorHandler(nil)

	loader.OnMessageHandler(NewProtocolMessage(lateMsgID, mismatchMsg{S: "x"}, nil))
	if _, err := loader.ModInvoke("blockingMod", "Total"); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Fatalf("已经取消接管，回调不该再被调用: %v", *got)
	}
	if n := loader.DiscardedErrors(); n != 1 {
		t.Fatalf("取消接管不影响计数，期望 1, got %d", n)
	}
}
