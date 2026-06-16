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

	time.AfterFunc(timeout, func() {
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

func (that *ChanTask) Await() error {
	if that.ctxTimeOut == nil {
		<-that.done
		return nil
	}

	select {
	case <-that.done:
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
