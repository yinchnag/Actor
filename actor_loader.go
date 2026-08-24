// Package actor provides a concurrent actor model framework for game servers.
// It enables modular game logic organization with cross-goroutine communication
// via channels and task queues.
package actor

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const defaultTaskTimeout = 3 * time.Second

var (
	errActorClosed      = errors.New("actor is closed")
	ErrTaskQueueTimeout = errors.New("task enqueue timeout")
	// ErrTaskCanceled 与 ErrTaskAwaitTimeout 一起返回，表示等待超时时任务还排在
	// 队列里没被取走，已经就地取消：模块方法确定一次都没执行。
	//
	// 这个区分对业务很关键。同样是超时，带 ErrTaskCanceled 的可以直接安全重试；
	// 不带的说明方法可能正在执行或已经执行完（Go 打断不了正在跑的函数），
	// 重试前必须自己去重，否则就是重复扣费、重复发奖。
	//
	//	if errors.Is(err, ErrTaskCanceled) { retry() }
	ErrTaskCanceled = errors.New("task canceled before execution")
	// ErrCallCycle 表示这次跨 actor 的同步调用会绕回一个已经在等待中的 actor，
	// 放进去必然死锁。事件循环是单消费者，一旦卡在 Await 上就消费不了自己的队列，
	// 对端回调过来的任务只能干躺着——环上每一环都等满 defaultTaskTimeout 才解开。
	//
	// 检测出来就立刻失败，把"整条环停摆 3 秒"降级成"当场报错"。
	// 任务根本没有被创建和投递，模块方法确定没有执行。
	// 但注意这跟 ErrTaskCanceled 不是一回事：重试解决不了环，得改调用结构
	// （拆成无返回值的异步回调，或者调整分层不让调用成环）。
	ErrCallCycle = errors.New("cross-actor call cycle detected")
	// ErrActorNotStarted 表示事件循环尚未启动，且调用方不是启动前的唯一持有者。
	// 用 Start 启动 actor 可以彻底避免这个错误。
	ErrActorNotStarted = errors.New("actor not started")
)

type ActorLoader struct {
	name string
	// initOwnerGID 记录事件循环启动之前第一个使用该 loader 的 goroutine。
	// 未启动时只有它能直接执行模块方法，其余调用方一律拒绝，
	// 否则它们会各自在自己的栈上并发修改模块状态。
	initOwnerGID uint64
	goroutineID  uint64
	closed       atomic.Bool
	taskChan     chan ITask
	stopChan     chan struct{}

	// waitingFor 是本 actor 的事件循环当前正在等待结果的那个 actor 的循环 GID，
	// 0 表示没在等。它是环检测的全部状态：谁在等谁，连起来就是一张等待图，
	// 图上出现回边就是死锁。
	//
	// 只有"要等返回值"的跨 actor 调用才会写它——那种调用会把事件循环整个钉住，
	// 是唯一能构成等待边的情形。无返回值调用投递完就走，不进这张图。
	waitingFor atomic.Uint64

	// enqueueMu 让投递与排空互斥：enqueueTask 持读锁投递，closeWith 持写锁排空。
	enqueueMu sync.RWMutex
	modulesMu sync.RWMutex
	modules   map[string]IModule
	closeOnce sync.Once

	// dispatchIndex 是「协议 ID → 处理它的模块方法」的只读快照。
	// AddModule 持 modulesMu 写锁整体重建，再原子换上；分发侧只做一次原子读。
	//
	// 之所以做成整体替换而不是就地改：分发路径上的 ModInvoke 会再次获取
	// modulesMu（directInvoke/shouldWaitResult 里都要 GetModule），
	// 所以分发绝不能持着 modulesMu。原来的写法是先把模块列表拷一份出来再放锁，
	// 每条消息一次分配、一次 O(模块数) 的遍历。换成只读快照之后，
	// 分发既不用加锁也不用拷贝，复杂度还从 O(模块数) 降到 O(该协议的处理器数)。
	dispatchIndex atomic.Pointer[map[int][]dispatchTarget]

	// 无返回值调用失败时的出口。没有调用方在等结果，错误无处可返，
	// 不记一笔就等于消息凭空消失。
	discardedCount   atomic.Uint64
	discardedHandler atomic.Pointer[DiscardedErrorFunc]
	discardedLogAt   atomic.Int64 // 上次兜底日志的时间戳，用于限流
}

// dispatchTarget 是一条解析好的分发路径，建索引时算一次，之后只读。
type dispatchTarget struct {
	modName    string
	methodName string
}

// ErrorPhase 指出失败发生在哪一步。两者的处置方式完全不同：
// 送不到通常是运行时压力（队列打满、actor 已关闭），执行失败基本都是编码问题
// （协议 ID 与消息类型对不上、方法签名不匹配）。
type ErrorPhase int8

const (
	// PhaseDeliver 消息没能送到模块方法手里：投递失败，或投递成功但 actor
	// 在取用之前就关闭了。方法一次都没执行。
	PhaseDeliver ErrorPhase = iota
	// PhaseInvoke 已经送到并开始执行，是方法调用本身失败了。
	PhaseInvoke
)

func (p ErrorPhase) String() string {
	if p == PhaseInvoke {
		return "invoke"
	}
	return "deliver"
}

// DiscardedError 是一次没人接收的调用失败。
//
// 无返回值的调用——协议派发、fire-and-forget 的 ModInvoke——按定义就没有调用方
// 在等结果，出了错也没有任何地方可以返回。不主动报出来，消息就是彻底静默地消失。
//
// 方法名足以定位是哪条协议：ModObj 给每个协议 ID 只绑一个处理方法。
type DiscardedError struct {
	Phase      ErrorPhase
	ModName    string
	MethodName string
	Err        error
}

func (e DiscardedError) Error() string {
	return fmt.Sprintf("%s %s.%s failed: %v", e.Phase, e.ModName, e.MethodName, e.Err)
}

// Unwrap 让 errors.Is 能穿透到底层原因，比如 ErrTaskQueueTimeout。
func (e DiscardedError) Unwrap() error { return e.Err }

// DiscardedErrorFunc 是丢弃回调。它在发现失败的那条协程上**同步**执行：
// 投递阶段是调用方协程，执行阶段是 actor 自己的事件循环。
// 所以务必轻量——在里面做阻塞 I/O 会拖慢投递，回调进同一个 actor 更会直接把
// 事件循环卡死。要落盘或上报，请自己丢进队列异步处理。
type DiscardedErrorFunc func(DiscardedError)

// maxWaitChainWalk 限制等待图的回溯深度。正常的调用链远到不了这个数，
// 走满了就保守放行，交给 defaultTaskTimeout 兜底——宁可漏检一次多等 3 秒，
// 也不能因为图走岔了就把合法调用拦下来。
const maxWaitChainWalk = 64

// loaderRegistry 把事件循环的 goroutine ID 映射回 loader，供环检测反查调用方。
// 只在 SetGoroutineID 与关闭时写，其余全是读，用 sync.Map 避开读锁竞争。
var loaderRegistry sync.Map // uint64 -> *ActorLoader

func loaderByGID(gid uint64) *ActorLoader {
	if gid == 0 {
		return nil
	}
	if v, ok := loaderRegistry.Load(gid); ok {
		return v.(*ActorLoader)
	}
	return nil
}

// wouldDeadlock 判断"callerGID 这条事件循环再去等 targetGID 的结果"会不会成环。
// 从目标出发顺着 waitingFor 一路往下走：绕回调用方，就说明这次调用一放进去，
// 环上每一环都在等下一环，谁也动不了。
//
// 只有 actor 的事件循环才会进这张图，普通业务协程等待不会钉住任何队列，
// 所以查不到 loader 就直接判定链断了。
func wouldDeadlock(targetGID, callerGID uint64) bool {
	g := targetGID
	for i := 0; i < maxWaitChainWalk; i++ {
		l := loaderByGID(g)
		if l == nil {
			return false // 不是已注册的 actor（或已关闭），链到此为止
		}
		w := l.waitingFor.Load()
		if w == 0 {
			return false // 这一环没在等人，链断了
		}
		if w == callerGID {
			return true // 绕回了调用方
		}
		g = w
	}
	return false
}

func NewActorLoader(name string) *ActorLoader {
	return &ActorLoader{
		name:     name,
		taskChan: make(chan ITask, 64),
		stopChan: make(chan struct{}),
		modules:  map[string]IModule{},
	}
}

func (that *ActorLoader) Init() {
	if that.modules == nil {
		that.modules = map[string]IModule{}
	}
}

func (that *ActorLoader) IsClose() bool {
	return that.closed.Load()
}

func (that *ActorLoader) GetGoroutineID() uint64 {
	return atomic.LoadUint64(&that.goroutineID)
}

func (that *ActorLoader) SetGoroutineID(role string) {
	that.name = role
	id := currentGID()
	if id == 0 {
		id = uint64(time.Now().UnixNano())
	}
	old := atomic.SwapUint64(&that.goroutineID, id)
	if old != 0 && old != id {
		loaderRegistry.Delete(old)
	}
	// 注册进 GID→loader 表，环检测才能从 callerGID 反查到调用方 actor。
	// 这次 Store 排在 that.name 与 goroutineID 的写之后，读方通过注册表或
	// 原子读到非零 goroutineID 都能看到完整的初始化结果。
	loaderRegistry.Store(id, that)
}

func (that *ActorLoader) GetTaskChan() chan<- ITask {
	return that.taskChan
}

// SetDiscardedErrorHandler 接管无返回值调用的失败，传 nil 可以取消接管。
//
// 不接管时框架会往 stderr 打兜底日志，但限流到每秒一条：队列打满时一条消息一行
// 会瞬间把日志淹掉，而"完全不吭声"比丢弃本身更糟——上层根本不知道消息没到。
// 计数器不受限流影响，始终精确。
func (that *ActorLoader) SetDiscardedErrorHandler(h DiscardedErrorFunc) {
	if h == nil {
		that.discardedHandler.Store(nil)
		return
	}
	that.discardedHandler.Store(&h)
}

// DiscardedErrors 返回累计的丢弃次数，供监控采样。
func (that *ActorLoader) DiscardedErrors() uint64 {
	return that.discardedCount.Load()
}

// reportDiscarded 记一次丢弃。计数永远精确，日志限流，回调优先。
func (that *ActorLoader) reportDiscarded(phase ErrorPhase, modName, methodName string, err error) {
	if err == nil {
		return
	}
	e := DiscardedError{Phase: phase, ModName: modName, MethodName: methodName, Err: err}
	total := that.discardedCount.Add(1)

	if h := that.discardedHandler.Load(); h != nil {
		(*h)(e)
		return
	}

	now := time.Now().UnixNano()
	last := that.discardedLogAt.Load()
	if now-last < int64(time.Second) {
		return
	}
	// CAS 失败说明刚有人打过这一秒的日志，让给它
	if !that.discardedLogAt.CompareAndSwap(last, now) {
		return
	}
	fmt.Fprintf(os.Stderr, "actor %s: 已丢弃 %d 次无返回值调用，最近一次: %v\n",
		that.name, total, e)
}

func (that *ActorLoader) Close() {
	that.closeWith(errActorClosed)
}

// closeWith 关闭 actor，并用 drainErr 结算所有还排在队列里的任务。
// drainErr 应当包装 errActorClosed，调用方才能用 errors.Is 统一判定。
func (that *ActorLoader) closeWith(drainErr error) {
	that.closeOnce.Do(func() {
		that.closed.Store(true) // 此后进入 enqueueTask 的投递方会被挡在 RLock 内
		close(that.stopChan)    // 唤醒已经卡在 select 里的投递方，避免下面干等

		// 退出等待图。关掉的 actor 不再接活，留在图里只会让别人多走一段冤枉路；
		// actor 频繁创建销毁时不清理还会让注册表无限膨胀。
		if gid := atomic.LoadUint64(&that.goroutineID); gid != 0 {
			loaderRegistry.Delete(gid)
		}

		// 写锁与 enqueueTask 的读锁互斥：拿到它就意味着没有任何投递方还在
		// 临界区内，也不会再有新的进来。只有在这个前提下排空，队列里剩下的
		// 才是真正的全部，不会出现“刚排完又飘进来一个”。
		that.enqueueMu.Lock()
		defer that.enqueueMu.Unlock()
		that.drainTask(drainErr)
	})
}

// Start 在独立 goroutine 上启动事件循环，直到 goroutineID 发布完成才返回。
// 这是启动 actor 的推荐方式：Start 返回后，任何来自其它 goroutine 的调用都会
// 走入队路径，不会再落到 directInvoke 上并发直改模块状态，调用方也不需要
// sleep 或轮询 GetGoroutineID 去等待启动完成。
//
// wg 由 Start 负责 Add，调用方只需 Wait，不要再自行 Add。
func (that *ActorLoader) Start(wg *sync.WaitGroup) {
	ready := make(chan struct{})
	wg.Add(1)
	go func() {
		that.SetGoroutineID(that.name)
		close(ready)
		that.RunUpdateLoop(wg)
	}()
	<-ready
}

// RunUpdateLoop 运行事件循环，调用方需自行 wg.Add(1)。
//
// 优先使用 Start：直接 go RunUpdateLoop 时，从发起 go 到循环内部写入
// goroutineID 之间存在一个窗口，窗口内的调用会被当成"未启动"处理。
func (that *ActorLoader) RunUpdateLoop(wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "actor %s panic in RunUpdateLoop: %v\n%s\n", that.name, r, debug.Stack())
			that.closeWith(fmt.Errorf("%w: panic in RunUpdateLoop: %v", errActorClosed, r))
		}
	}()
	if that.GetGoroutineID() == 0 {
		that.SetGoroutineID(that.name)
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-that.stopChan:
			// Drain queued tasks so their cleanup goroutines don't hang indefinitely.
			// Tasks currently executing complete normally; queued-but-unstarted tasks
			// are completed with errActorClosed so their done channels are closed.
			that.drainTask(errActorClosed)
			return
		case <-ticker.C:
			that.updateAll(1000)
		case task := <-that.taskChan:
			that.handleTask(task)
		}
	}
}

func (that *ActorLoader) drainTask(err error) {
	for {
		select {
		case task := <-that.taskChan:
			if ct, ok := task.(*ChanTask); ok {
				// 投递进来了却没轮到执行，对无返回值调用来说同样是消息丢失
				that.settleTask(ct, PhaseDeliver, task.GetModName(), task.GetMethodName(), nil, err)
			}
		default:
			return
		}
	}
}

func (that *ActorLoader) AddModule(mod IModule) {
	if mod == nil {
		return
	}
	that.modulesMu.Lock()
	that.modules[mod.GameName()] = mod
	that.rebuildDispatchIndex()
	that.modulesMu.Unlock()
}

// rebuildDispatchIndex 重建协议分发索引，调用方必须持有 modulesMu 写锁。
//
// 整表重建而不是增量更新：AddModule 只在启动装配时调用，重建成本无所谓；
// 换来的是分发侧永远读到一张完整、此后不再改动的表，不需要任何锁。
func (that *ActorLoader) rebuildDispatchIndex() {
	idx := make(map[int][]dispatchTarget, len(that.modules))
	for name, mod := range that.modules {
		// 可选接口，与 shouldWaitResult 里的 GetNumOut 同一套路：
		// 嵌了 ModObj 的模块自动具备；手写的 IModule 只是享受不到索引，不会坏。
		h, ok := mod.(interface{ MetaHandlers() map[int]string })
		if !ok {
			continue
		}
		for msgID, method := range h.MetaHandlers() {
			if method == "" {
				continue
			}
			idx[msgID] = append(idx[msgID], dispatchTarget{modName: name, methodName: method})
		}
	}
	// 同一个协议被多个模块认领时，按模块名定序。
	// 原来靠 map 迭代，每条消息的派发顺序都不一样；定序之后行为才可复现。
	for _, targets := range idx {
		sort.Slice(targets, func(i, j int) bool { return targets[i].modName < targets[j].modName })
	}
	that.dispatchIndex.Store(&idx)
}

func (that *ActorLoader) GetModule(name string) IModule {
	that.modulesMu.RLock()
	m := that.modules[name]
	that.modulesMu.RUnlock()
	return m
}

// ModInvoke dispatches a module method call from the current goroutine.
// For hot-path callers that invoke repeatedly from the same long-lived goroutine,
// capture the GID once with CurrentGID() and use ModInvokeFrom to skip
// the runtime.Stack overhead on every call.
func (that *ActorLoader) ModInvoke(modName, methodName string, args ...any) ([]reflect.Value, error) {
	return that.ModInvokeFrom(currentGID(), modName, methodName, args...)
}

// ModInvokeFrom dispatches a module method call using a pre-captured callerGID.
// Long-lived caller goroutines (user sessions, workers) should capture their ID once:
//
//	gid := actor.CurrentGID()
//	loader.ModInvokeFrom(gid, modName, methodName, args...)
func (that *ActorLoader) ModInvokeFrom(callerGID uint64, modName, methodName string, args ...any) ([]reflect.Value, error) {
	if that.IsClose() {
		return nil, errActorClosed
	}

	gid := atomic.LoadUint64(&that.goroutineID)
	if gid == 0 {
		// 事件循环还没启动，不存在"actor 所属 goroutine"。此时只放行第一个
		// 使用它的 goroutine（初始化、加载存档等单线程场景），其余一律拒绝：
		// 否则多个调用方会各自在自己的栈上直接改模块状态，actor 模型就破了。
		if !that.claimInitOwner(callerGID) {
			return nil, ErrActorNotStarted
		}
		return that.directInvoke(modName, methodName, args...)
	}
	if callerGID == gid {
		return that.directInvoke(modName, methodName, args...)
	}

	// waitResult 必须在投递之前存进局部变量。投递成功之后，无返回值的任务
	// 归事件循环所有：它 complete 完就 Release，任务当场回池、被别的调用方
	// 重新拿去用。此时再回头读 task 的任何字段都是读别人的任务——
	// 轻则拿到脏值，重则误判成"要等结果"，去 Await 并 Release 一个不属于自己的任务。
	// handleTask/drainTask 里同样是先取快照再 complete，这里遵循同一条纪律。
	waitResult := that.shouldWaitResult(modName, methodName)

	// 环检测只管"要等返回值"的调用：只有它会把调用方的事件循环钉住，
	// 也只有它能构成等待边。无返回值调用投递完就返回，进不了等待图，
	// 自然也不会被误伤——环上只要有一处是异步的，环就断了。
	if waitResult {
		if src := loaderByGID(callerGID); src != nil {
			// 调用方是另一个 actor 的事件循环。先公布"我要等谁"再检查：
			// 两个 actor 同时互调时，这个顺序保证至少有一方能看见对方的意图，
			// 不会双双漏检（代价是可能双双拒绝，那是安全的方向）。
			src.waitingFor.Store(gid)
			defer src.waitingFor.Store(0)

			if wouldDeadlock(gid, callerGID) {
				src.waitingFor.Store(0) // 这次不会真的等了，立刻退出等待图
				return nil, fmt.Errorf("%w: actor %s 等 actor %s 的返回值会绕回自己",
					ErrCallCycle, src.name, that.name)
			}
		}
	}

	task := NewChanTask(callerGID, gid, modName, methodName, args...)
	task.SetWaitResult(waitResult)

	if err := that.enqueueTask(task); err != nil {
		task.Release()
		return nil, err
	}

	if waitResult {
		err := task.WithTimeout(defaultTaskTimeout).Await()
		if err != nil {
			// 超时即取消：抢在事件循环认领之前把任务作废，模块方法就不会再执行。
			// 调用方已经拿着超时错误走人了，这时候再跑一遍就是一次没人接收结果的
			// 副作用。抢不到说明它已经在跑了，只能如实告诉调用方，别谎称已取消。
			if task.abandon() {
				err = fmt.Errorf("%w: %w", ErrTaskAwaitTimeout, ErrTaskCanceled)
			}
			done := task.done
			go func(t *ChanTask, doneCh <-chan struct{}) {
				<-doneCh
				t.Release()
			}(task, done)
			return nil, err
		}
		results := append([]reflect.Value(nil), task.GetResults()...)
		callErr := task.Err
		task.Release()
		return results, callErr
	}
	return nil, nil
}

// claimInitOwner 认定事件循环启动前的唯一持有者：第一个调用方抢到所有权，
// 此后只有它返回 true。取不到 gid（callerGID 为 0）时一律拒绝。
func (that *ActorLoader) claimInitOwner(callerGID uint64) bool {
	if callerGID == 0 {
		return false
	}
	if atomic.CompareAndSwapUint64(&that.initOwnerGID, 0, callerGID) {
		return true
	}
	return atomic.LoadUint64(&that.initOwnerGID) == callerGID
}

// OnMessageHandler 把协议消息派发给认领它的模块方法。
//
// 前端消息是最高频的入口，而它每次都要解析一遍调用栈取 GID（约 4µs，
// 比投递本身贵好几倍）。长期存活的接收协程（网络读协程、消息泵）应当用
// CurrentGID() 缓存一次，改走 OnMessageHandlerFrom，把这笔开销彻底省掉。
func (that *ActorLoader) OnMessageHandler(p IProtocol) {
	if p == nil {
		return
	}
	// 先确认真有处理器再去解析栈：没人认领的协议一次 runtime.Stack 都不该花。
	targets, msg, ok := that.dispatchTargets(p)
	if !ok {
		return
	}
	that.dispatch(currentGID(), targets, msg)
}

// OnMessageHandlerFrom 用预先取好的 callerGID 派发协议消息。
// 长期存活的调用协程应当只取一次 GID 然后一直复用：
//
//	gid := actor.CurrentGID()
//	for msg := range conn.Recv() {
//	    loader.OnMessageHandlerFrom(gid, msg)
//	}
func (that *ActorLoader) OnMessageHandlerFrom(callerGID uint64, p IProtocol) {
	if p == nil {
		return
	}
	targets, msg, ok := that.dispatchTargets(p)
	if !ok {
		return
	}
	that.dispatch(callerGID, targets, msg)
}

// dispatchTargets 查出该派发给谁。索引是只读快照，这里只做一次原子读加一次查表，
// 不碰 modulesMu——分发路径上的 ModInvoke 还要再拿它。
func (that *ActorLoader) dispatchTargets(p IProtocol) ([]dispatchTarget, IMessage, bool) {
	idx := that.dispatchIndex.Load()
	if idx == nil {
		return nil, nil, false // 还没 AddModule 过，没有任何处理器
	}
	targets := (*idx)[p.GetMessageID()]
	if len(targets) == 0 {
		return nil, nil, false
	}
	return targets, p.GetMessage(), true
}

// dispatch 逐个投递。GID 由调用方给定，一条消息只取一次——
// 原来是每个命中的模块各解析一遍栈，模块越多越亏。
//
// 返回值确实没有意义（协议处理器按定义就没有返回值），但错误有：
// 投递不进去就意味着这条前端消息被丢了，必须留下痕迹。
// 至于送达之后执行阶段的失败，发生在事件循环那侧，由 settleTask 负责上报。
func (that *ActorLoader) dispatch(callerGID uint64, targets []dispatchTarget, msg IMessage) {
	for _, t := range targets {
		if _, err := that.ModInvokeFrom(callerGID, t.modName, t.methodName, msg); err != nil {
			that.reportDiscarded(PhaseDeliver, t.modName, t.methodName, err)
		}
	}
}

func (that *ActorLoader) directInvoke(modName, methodName string, args ...any) ([]reflect.Value, error) {
	mod := that.GetModule(modName)
	if mod == nil {
		return nil, fmt.Errorf("module not found: %s", modName)
	}
	return mod.Invoke(methodName, args...)
}

// settleTask 结算一个任务，是所有结算路径的唯一出口。
//
// 顺序不能反：必须先把 waitResult 取成快照再 complete。complete 会关闭 done，
// 等在上面的调用方随时可能把任务 Release 回池、被别人重新拿去用，
// 之后再读 task 的任何字段都是读别人的任务。
// 无返回值的任务没人等结果，回收责任就在结算这一侧。
//
// 也正因为没人等结果，它的错误在这里是最后一次被看见的机会：
// complete 之后 err 只存在 task.Err 里，随任务一起回池，谁也读不到了。
// 有返回值的任务不用管——调用方会自己拿到 err。
// phase 由调用方给定：排空时任务根本没被取用过（PhaseDeliver），
// 执行完或执行中出错才是 PhaseInvoke。混为一谈会让排查方向完全走偏——
// 一个是过载/关闭，一个是代码有问题。
func (that *ActorLoader) settleTask(ct *ChanTask, phase ErrorPhase, modName, methodName string, results []reflect.Value, err error) {
	waitResult := ct.ShouldWaitResult()
	if !waitResult && err != nil {
		that.reportDiscarded(phase, modName, methodName, err)
	}
	ct.complete(results, err)
	if !waitResult {
		ct.Release()
	}
}

func (that *ActorLoader) handleTask(task ITask) {
	defer func() {
		if r := recover(); r != nil {
			if ct, ok := task.(*ChanTask); ok {
				that.settleTask(ct, PhaseInvoke, task.GetModName(), task.GetMethodName(),
					nil, fmt.Errorf("panic: %v", r))
			}
			that.Close()
		}
	}()
	if task == nil {
		return
	}

	ct, _ := task.(*ChanTask)
	if ct != nil && !ct.claimForRun() {
		// 认领失败：调用方在事件循环取到它之前就等超时并作废了它。
		// 跳过模块方法——这正是"超时即取消"要的效果。但仍然要结算，
		// 好让等在 <-done 上的清理协程退出、任务回池。
		that.settleTask(ct, PhaseDeliver, ct.GetModName(), ct.GetMethodName(), nil, ErrTaskCanceled)
		return
	}

	modName, methodName := task.GetModName(), task.GetMethodName()
	results, err := that.directInvoke(modName, methodName, task.GetArgs()...)
	if ct != nil {
		that.settleTask(ct, PhaseInvoke, modName, methodName, results, err)
	}
}

func (that *ActorLoader) updateAll(dt int64) {
	that.modulesMu.RLock()
	mods := make([]IModule, 0, len(that.modules))
	for _, m := range that.modules {
		mods = append(mods, m)
	}
	that.modulesMu.RUnlock()

	for _, mod := range mods {
		mod.Update(dt)
	}
}

func (that *ActorLoader) shouldWaitResult(modName, methodName string) bool {
	mod := that.GetModule(modName)
	if mod == nil {
		return false
	}
	t, ok := mod.(interface{ GetNumOut(string) int })
	if !ok {
		return false
	}
	return t.GetNumOut(methodName) > 0
}

func (that *ActorLoader) enqueueTask(task *ChanTask) error {
	// 持读锁投递：Close 必须等这里退出后才排空，
	// 否则 select 随机选中发送时，任务会落进已经没人消费的 channel。
	that.enqueueMu.RLock()
	defer that.enqueueMu.RUnlock()
	if that.IsClose() {
		return errActorClosed
	}

	enqueueTimer := time.NewTimer(defaultTaskTimeout)
	defer enqueueTimer.Stop()

	select {
	case that.taskChan <- task:
		return nil
	case <-that.stopChan:
		return errActorClosed
	case <-enqueueTimer.C:
		return ErrTaskQueueTimeout
	}
}

func moduleTypeName(mod IModule) string {
	t := reflect.TypeOf(mod)
	if t == nil {
		return ""
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}

// CurrentGID returns the current goroutine's ID.
// Cache the returned value and pass it to ModInvoke to avoid calling
// runtime.Stack on every hot-path ModInvoke.
func CurrentGID() uint64 {
	return currentGID()
}

// currentGID 解析当前协程的 ID。
//
// 这个函数很贵：runtime.Stack 会走一遍调用栈，实测在几微秒量级，
// 比一次跨协程投递本身还贵。凡是能缓存 GID 的地方都该缓存，
// 走 ModInvokeFrom / OnMessageHandlerFrom，别让它进热路径。
//
// 既然躲不掉的调用还是有，就把 runtime.Stack 之外的开销榨干：
// 直接在字节上取数字，不转 string、不用 strings.Fields——
// 那两步是每次调用两次堆分配，而 runtime.Stack 本身是零分配的。
func currentGID() uint64 {
	// 栈上的固定数组，不逃逸。64 字节足够放下 "goroutine <id> [" 这个前缀，
	// runtime.Stack 会把写不下的部分直接丢掉。
	var buf [64]byte
	n := runtime.Stack(buf[:], false)

	// 首行形如 "goroutine 123 [running]:"
	const prefix = "goroutine "
	if n < len(prefix) || string(buf[:len(prefix)]) != prefix {
		return 0
	}
	var id uint64
	i := len(prefix)
	for ; i < n && buf[i] >= '0' && buf[i] <= '9'; i++ {
		id = id*10 + uint64(buf[i]-'0')
	}
	if i == len(prefix) {
		return 0 // 前缀后面一个数字都没有
	}
	return id
}

var _ IModLoader = (*ActorLoader)(nil)
