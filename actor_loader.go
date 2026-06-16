// Package actor provides a concurrent actor model framework for game servers.
// It enables modular game logic organization with cross-goroutine communication
// via channels and task queues.
package actor

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
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
)

type ActorLoader struct {
	name        string
	goroutineID uint64
	closed      atomic.Bool
	taskChan    chan ITask
	stopChan    chan struct{}

	modulesMu sync.RWMutex
	modules   map[string]IModule
	closeOnce sync.Once
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
	atomic.StoreUint64(&that.goroutineID, id)
}

func (that *ActorLoader) GetTaskChan() chan<- ITask {
	return that.taskChan
}

func (that *ActorLoader) Close() {
	that.closeOnce.Do(func() {
		that.closed.Store(true)
		close(that.stopChan)
	})
}

func (that *ActorLoader) RunUpdateLoop(wg *sync.WaitGroup) {
	defer wg.Done()
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
			for {
				select {
				case task := <-that.taskChan:
					if ct, ok := task.(*ChanTask); ok {
						waitResult := ct.ShouldWaitResult()
						ct.complete(nil, errActorClosed)
						if !waitResult {
							ct.Release()
						}
					}
				default:
					return
				}
			}
		case <-ticker.C:
			that.updateAll(1000)
		case task := <-that.taskChan:
			that.handleTask(task)
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
	// goroutineID == 0 means the loop has not started yet; treat as same-goroutine.
	if gid == 0 || callerGID == gid {
		return that.directInvoke(modName, methodName, args...)
	}

	task := NewChanTask(callerGID, gid, modName, methodName, args...)
	task.SetWaitResult(that.shouldWaitResult(modName, methodName))

	if err := that.enqueueTask(task); err != nil {
		task.Release()
		return nil, err
	}

	if task.ShouldWaitResult() {
		err := task.WithTimeout(defaultTaskTimeout).Await()
		if err != nil {
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

func (that *ActorLoader) handleTask(task ITask) {
	if task == nil {
		return
	}

	results, err := that.directInvoke(task.GetModName(), task.GetMethodName(), task.GetArgs()...)
	if ct, ok := task.(*ChanTask); ok {
		waitResult := ct.ShouldWaitResult()
		ct.complete(results, err)
		if !waitResult {
			ct.Release()
		}
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
