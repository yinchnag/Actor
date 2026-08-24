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
	"strconv"
	"strings"
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
}

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
				settleTask(ct, nil, err)
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
	that.modulesMu.Unlock()
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

func (that *ActorLoader) OnMessageHandler(p IProtocol) {
	if p == nil {
		return
	}

	that.modulesMu.RLock()
	mods := make([]IModule, 0, len(that.modules))
	for _, m := range that.modules {
		mods = append(mods, m)
	}
	that.modulesMu.RUnlock()

	msgID := p.GetMessageID()
	msg := p.GetMessage()

	for _, mod := range mods {
		methodName := mod.GetMetaHandler(msgID)
		if methodName == "" {
			continue
		}
		// already on the actor's own goroutine — pass its GID directly to skip currentGID()
		_, _ = that.ModInvoke(mod.GameName(), methodName, msg)
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
func settleTask(ct *ChanTask, results []reflect.Value, err error) {
	waitResult := ct.ShouldWaitResult()
	ct.complete(results, err)
	if !waitResult {
		ct.Release()
	}
}

func (that *ActorLoader) handleTask(task ITask) {
	defer func() {
		if r := recover(); r != nil {
			if ct, ok := task.(*ChanTask); ok {
				settleTask(ct, nil, fmt.Errorf("panic: %v", r))
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
		settleTask(ct, nil, ErrTaskCanceled)
		return
	}

	results, err := that.directInvoke(task.GetModName(), task.GetMethodName(), task.GetArgs()...)
	if ct != nil {
		settleTask(ct, results, err)
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

func currentGID() uint64 {
	buf := make([]byte, 64)
	n := runtime.Stack(buf, false)
	line := string(buf[:n])
	parts := strings.Fields(line)
	if len(parts) < 2 || parts[0] != "goroutine" {
		return 0
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

var _ IModLoader = (*ActorLoader)(nil)
