package actor

import (
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

var ErrTaskAwaitTimeout = errors.New("task await timeout")

var chanTaskPool = sync.Pool{
	New: func() any {
		return &ChanTask{}
	},
}

type ChanTask struct {
	ID         int64
	SourceGID  uint64
	TargetGID  uint64
	ModName    string
	MethodName string
	Args       []any
	Results    []reflect.Value
	Err        error
	Status     int32
	ctxTimeOut chan struct{}
	done       chan struct{}
	// timer 是 WithTimeout 起的超时定时器。留住句柄是为了在拿到结果后把它 Stop 掉：
	// 不 Stop 的话，定时器要一直挂在运行时的定时器堆里等到期，闭包又引着 ChanTask
	// 和它的两个 channel，于是每次跨协程调用都要多占一个超时周期的内存。
	// 实测 28 万次在途调用会因此多出约 140MB 堆占用；Stop 之后降到约 2MB。
	//
	// 只由持有该任务的一方读写：WithTimeout/Await 在调用方协程上，Release 则发生在
	// 任务所有权已经转移之后，两者之间有 channel 或 go 语句建立的 happens-before。
	timer      *time.Timer
	waitResult bool
	released   int32
	gen        atomic.Uint64
}

func NewChanTask(sourceID, targetID uint64, modName, methodName string, args ...any) *ChanTask {
	task := chanTaskPool.Get().(*ChanTask)
	task.gen.Add(1)
	task.SourceGID = sourceID
	task.TargetGID = targetID
	task.ModName = modName
	task.MethodName = methodName
	task.Args = append(task.Args[:0], args...)
	task.Results = task.Results[:0]
	task.Err = nil
	atomic.StoreInt32(&task.Status, 1)
	task.done = make(chan struct{})
	task.ctxTimeOut = nil
	task.timer = nil
	task.waitResult = false
	atomic.StoreInt32(&task.released, 0)
	return task
}

func (that *ChanTask) GetGoroutineID() uint64 {
	return that.SourceGID
}

func (that *ChanTask) GetTargetID() uint64 {
	return that.TargetGID
}

func (that *ChanTask) GetStatus() int32 {
	return atomic.LoadInt32(&that.Status)
}

func (that *ChanTask) GetModName() string {
	return that.ModName
}

func (that *ChanTask) GetMethodName() string {
	return that.MethodName
}

func (that *ChanTask) GetArgs() []any {
	return that.Args
}

func (that *ChanTask) GetResults() []reflect.Value {
	return that.Results
}

func (that *ChanTask) SetWaitResult(wait bool) {
	that.waitResult = wait
}

func (that *ChanTask) ShouldWaitResult() bool {
	return that.waitResult
}

func (that *ChanTask) WithTimeout(timeout time.Duration) *ChanTask {
	if timeout <= 0 {
		return that
	}
	if that.ctxTimeOut == nil {
		that.ctxTimeOut = make(chan struct{}, 1)
	}
	ch := that.ctxTimeOut
	done := that.done
	gen := that.gen.Load()

	// 重复调用 WithTimeout 视为重设期限，先撤掉上一个，别留下孤儿定时器。
	that.stopTimer()
	that.timer = time.AfterFunc(timeout, func() {
		if that.gen.Load() != gen {
			return
		}
		select {
		case <-done:
			return
		default:
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	})
	return that
}

// stopTimer 撤掉尚未到期的超时定时器并断开引用。
// Stop 失败（定时器恰好正在触发）也无所谓：闭包里的 gen 与 done 两道检查
// 保证它不会误伤已经完成或已被复用的任务。
func (that *ChanTask) stopTimer() {
	if that.timer == nil {
		return
	}
	that.timer.Stop()
	that.timer = nil
}

func (that *ChanTask) Await() error {
	if that.ctxTimeOut == nil {
		<-that.done
		return nil
	}

	select {
	case <-that.done:
		// 成功路径：结果已经到手，定时器再没有用处。立刻 Stop，
		// 任务和它的 channel 当场就能回收，不必白白吊到超时周期结束。
		that.stopTimer()
		return nil
	case <-that.ctxTimeOut:
		return ErrTaskAwaitTimeout
	}
}

func (that *ChanTask) cancel() {
	if that.ctxTimeOut == nil {
		return
	}
	select {
	case that.ctxTimeOut <- struct{}{}:
	default:
	}
}

func (that *ChanTask) complete(results []reflect.Value, err error) {
	// Only one goroutine is allowed to settle a task once.
	// If status is no longer running(1), the task was already released/settled.
	if !atomic.CompareAndSwapInt32(&that.Status, 1, 2) {
		return
	}

	that.Results = append(that.Results[:0], results...)
	that.Err = err
	done := that.done
	if done != nil {
		close(done)
	}
}

func (that *ChanTask) Release() {
	if !atomic.CompareAndSwapInt32(&that.released, 0, 1) {
		return
	}

	// 兜底：没走 Await 成功路径的任务（投递失败、超时后由清理协程回收、
	// 无返回值任务由事件循环回收）也不能带着定时器引用回池。
	that.stopTimer()

	that.ID = 0
	that.SourceGID = 0
	that.TargetGID = 0
	that.ModName = ""
	that.MethodName = ""
	that.Args = that.Args[:0]
	that.Results = that.Results[:0]
	that.Err = nil
	atomic.StoreInt32(&that.Status, 0)
	that.ctxTimeOut = nil
	that.done = nil
	chanTaskPool.Put(that)
}
