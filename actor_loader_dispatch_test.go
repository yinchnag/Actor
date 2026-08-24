package actor

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type dupMsg struct{ N int }
type lateMsg struct{ N int }

const (
	dupMsgID  = 7101
	lateMsgID = 7102
)

func init() {
	RegisterProtocol(dupMsgID, dupMsg{})
	RegisterProtocol(lateMsgID, lateMsg{})
}

// dupModAlpha 与 dupModBeta 认领同一个协议，用来验证一条消息会派发给全部认领者，
// 且顺序稳定（索引按模块名定序）。
type dupModAlpha struct {
	ModObj[*dupModAlpha]
	seen chan string
	got  atomic.Int64
}

func newDupModAlpha(seen chan string) *dupModAlpha {
	m := &dupModAlpha{seen: seen}
	m.Init()
	return m
}

func (m *dupModAlpha) OnDup(msg dupMsg) {
	m.got.Add(int64(msg.N))
	m.seen <- "dupModAlpha"
}

func (m *dupModAlpha) Total() int { return int(m.got.Load()) }

type dupModBeta struct {
	ModObj[*dupModBeta]
	seen chan string
	got  atomic.Int64
}

func newDupModBeta(seen chan string) *dupModBeta {
	m := &dupModBeta{seen: seen}
	m.Init()
	return m
}

func (m *dupModBeta) OnDup(msg dupMsg) {
	m.got.Add(int64(msg.N))
	m.seen <- "dupModBeta"
}

// lateMod 在 actor 跑起来之后才挂上去，用来验证索引确实跟着 AddModule 重建。
type lateMod struct {
	ModObj[*lateMod]
	got atomic.Int64
}

func newLateMod() *lateMod {
	m := &lateMod{}
	m.Init()
	return m
}

func (m *lateMod) OnLate(msg lateMsg) { m.got.Add(int64(msg.N)) }

func (m *lateMod) Total() int { return int(m.got.Load()) }

// TestDispatchToAllModulesClaimingSameProtocol 一个协议被多个模块认领时，
// 每个认领者都要收到，且派发顺序稳定。
//
// 老实现每条消息现遍历一遍模块 map，Go 的 map 迭代顺序是随机的，
// 于是多认领者的派发顺序每条消息都不一样；索引按模块名定序之后行为才可复现。
func TestDispatchToAllModulesClaimingSameProtocol(t *testing.T) {
	seen := make(chan string, 64)
	loader := NewActorLoader("dup-dispatch")
	loader.Init()
	alpha, beta := newDupModAlpha(seen), newDupModBeta(seen)
	loader.AddModule(alpha)
	loader.AddModule(beta)

	var wg sync.WaitGroup
	loader.Start(&wg)
	defer func() {
		loader.Close()
		wg.Wait()
	}()

	const rounds = 8
	for i := 0; i < rounds; i++ {
		loader.OnMessageHandler(NewProtocolMessage(dupMsgID, dupMsg{N: 1}, nil))
	}
	// FIFO 屏障：这次同步调用返回时，前面的协议任务必然都已执行完
	if _, err := loader.ModInvoke("dupModAlpha", "Total"); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}

	if got := alpha.got.Load(); got != rounds {
		t.Fatalf("dupModAlpha 收到 %d 条，期望 %d 条", got, rounds)
	}
	if got := beta.got.Load(); got != rounds {
		t.Fatalf("dupModBeta 收到 %d 条，期望 %d 条", got, rounds)
	}

	close(seen)
	var order []string
	for s := range seen {
		order = append(order, s)
	}
	if len(order) != rounds*2 {
		t.Fatalf("派发次数 %d，期望 %d", len(order), rounds*2)
	}
	for i := 0; i < len(order); i += 2 {
		if order[i] != "dupModAlpha" || order[i+1] != "dupModBeta" {
			t.Fatalf("第 %d 条消息的派发顺序不稳定: %v", i/2, order[i:i+2])
		}
	}
}

// TestDispatchIndexRebuiltOnAddModule 事件循环跑起来之后再挂模块，索引必须跟着重建。
// 索引是快照，忘了重建的话新模块永远收不到消息。
func TestDispatchIndexRebuiltOnAddModule(t *testing.T) {
	loader := NewActorLoader("late-add")
	loader.Init()

	var wg sync.WaitGroup
	loader.Start(&wg)
	defer func() {
		loader.Close()
		wg.Wait()
	}()

	// 还没挂模块：分发是空操作，且不能 panic（此时索引指针为 nil）
	loader.OnMessageHandler(NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))

	late := newLateMod()
	loader.AddModule(late)

	const rounds = 5
	for i := 0; i < rounds; i++ {
		loader.OnMessageHandler(NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))
	}
	if _, err := loader.ModInvoke("lateMod", "Total"); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}

	if got := late.got.Load(); got != rounds {
		t.Fatalf("后挂的模块收到 %d 条，期望 %d 条——索引没跟着 AddModule 重建", got, rounds)
	}
}

// TestOnMessageHandlerFromEquivalent 缓存 GID 的入口必须与自己取 GID 的入口
// 派发结果完全一致——它只是把 currentGID 那一步交给调用方，语义不能有任何差别。
func TestOnMessageHandlerFromEquivalent(t *testing.T) {
	loader := NewActorLoader("dispatch-from")
	loader.Init()
	late := newLateMod()
	loader.AddModule(late)

	var wg sync.WaitGroup
	loader.Start(&wg)
	defer func() {
		loader.Close()
		wg.Wait()
	}()

	gid := CurrentGID()
	const rounds = 10
	for i := 0; i < rounds; i++ {
		loader.OnMessageHandler(NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))
		loader.OnMessageHandlerFrom(gid, NewProtocolMessage(lateMsgID, lateMsg{N: 1}, nil))
	}
	// 未注册协议与 nil 消息，两个入口都必须是空操作
	loader.OnMessageHandlerFrom(gid, NewProtocolMessage(999999, lateMsg{N: 1}, nil))
	loader.OnMessageHandlerFrom(gid, nil)

	if _, err := loader.ModInvokeFrom(gid, "lateMod", "Total"); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}
	if got := late.got.Load(); got != rounds*2 {
		t.Fatalf("两个入口共派发 %d 条，期望 %d 条", got, rounds*2)
	}
}

// TestDispatchUnknownProtocolIsNoop 没人认领的协议 ID 直接丢弃，不能误派发也不能崩。
func TestDispatchUnknownProtocolIsNoop(t *testing.T) {
	loader := NewActorLoader("unknown-proto")
	loader.Init()
	late := newLateMod()
	loader.AddModule(late)

	var wg sync.WaitGroup
	loader.Start(&wg)
	defer func() {
		loader.Close()
		wg.Wait()
	}()

	loader.OnMessageHandler(NewProtocolMessage(999999, lateMsg{N: 1}, nil))
	loader.OnMessageHandler(nil)

	if _, err := loader.ModInvoke("lateMod", "Total"); err != nil {
		t.Fatalf("屏障调用失败: %v", err)
	}
	if got := late.got.Load(); got != 0 {
		t.Fatalf("未注册的协议不该派发到任何模块, got %d", got)
	}
}

// TestDispatchConcurrentWithAddModuleNoDeadlock 是这次改造的回归闸门。
//
// 分发路径上的 ModInvoke 会再次获取 modulesMu（directInvoke/shouldWaitResult
// 里都要 GetModule）。所以分发一旦持着 modulesMu 遍历模块，就是递归 RLock——
// 只要中间有人调 AddModule 请求写锁，RWMutex 为了不饿死写者会挡住后续所有读者，
// 于是分发协程卡在嵌套 RLock 上等写者、写者等分发协程放锁，双向死锁。
//
// 老实现靠"先把模块列表拷一份再放锁"绕开；现在靠只读快照绕开，
// 分发全程不碰 modulesMu。这个用例把两者的前提一起钉死：
// 边分发边 AddModule，必须能跑完。真退化了这里会挂死到超时。
func TestDispatchConcurrentWithAddModuleNoDeadlock(t *testing.T) {
	seen := make(chan string, 1<<16)
	loader := NewActorLoader("dispatch-vs-addmodule")
	loader.Init()
	loader.AddModule(newDupModAlpha(seen))

	var wg sync.WaitGroup
	loader.Start(&wg)
	defer func() {
		loader.Close()
		wg.Wait()
	}()

	const rounds = 3000
	beta := newDupModBeta(seen)
	msg := NewProtocolMessage(dupMsgID, dupMsg{N: 1}, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		var workers sync.WaitGroup
		workers.Add(2)
		go func() { // 持续分发
			defer workers.Done()
			for i := 0; i < rounds; i++ {
				loader.OnMessageHandler(msg)
			}
		}()
		go func() { // 同时反复重建索引
			defer workers.Done()
			for i := 0; i < rounds; i++ {
				loader.AddModule(beta)
			}
		}()
		workers.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("边分发边 AddModule 卡死了：分发路径又持着 modulesMu 了")
	}

	// 收尾：消费掉 seen，避免模块方法阻塞在满 channel 上把事件循环拖住
	for len(seen) > 0 {
		<-seen
	}
}

// fakeMod 是最轻的 IModule 实现，只为把模块数量堆上去。
// 手写而不是嵌 ModObj，顺带覆盖"没有 MetaHandlers 之外任何反射开销"的情形。
type fakeMod struct {
	name string
	meta map[int]string
}

func (m *fakeMod) Init()                                          {}
func (m *fakeMod) Save()                                          {}
func (m *fakeMod) Load()                                          {}
func (m *fakeMod) GameName() string                               { return m.name }
func (m *fakeMod) Invoke(string, ...any) ([]reflect.Value, error) { return nil, nil }
func (m *fakeMod) Update(int64)                                   {}
func (m *fakeMod) GetMetaHandler(msg int) string                  { return m.meta[msg] }
func (m *fakeMod) IsDirty() bool                                  { return false }
func (m *fakeMod) SetDirty(bool)                                  {}
func (m *fakeMod) MetaHandlers() map[int]string                   { return m.meta }

// benchDispatchLookup 只量"找出该派发给谁"这一段，不含投递——
// 那才是这次改造真正动到的地方。老写法每条消息都要拷一遍模块列表再逐个问
// GetMetaHandler，是 O(模块数)；新写法一次原子读加一次查表，与模块数无关。
// 两个规模对比就能看出这笔开销在多模块的 actor 上才显形。
func benchDispatchLookup(b *testing.B, modules int, indexed bool) {
	l := NewActorLoader("bench-lookup")
	l.Init()
	for i := 0; i < modules; i++ {
		meta := map[int]string{}
		if i == 0 { // 只有一个模块认领这个协议，贴近真实
			meta[dupMsgID] = "OnDup"
		}
		l.AddModule(&fakeMod{name: fmt.Sprintf("m%02d", i), meta: meta})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for k := 0; k < b.N; k++ {
		if indexed {
			idx := l.dispatchIndex.Load()
			_ = (*idx)[dupMsgID]
			continue
		}
		// 改造前的形状
		l.modulesMu.RLock()
		mods := make([]IModule, 0, len(l.modules))
		for _, m := range l.modules {
			mods = append(mods, m)
		}
		l.modulesMu.RUnlock()
		for _, m := range mods {
			_ = m.GetMetaHandler(dupMsgID)
		}
	}
}

func BenchmarkDispatchLookupCopy2(b *testing.B)   { benchDispatchLookup(b, 2, false) }
func BenchmarkDispatchLookupIndex2(b *testing.B)  { benchDispatchLookup(b, 2, true) }
func BenchmarkDispatchLookupCopy30(b *testing.B)  { benchDispatchLookup(b, 30, false) }
func BenchmarkDispatchLookupIndex30(b *testing.B) { benchDispatchLookup(b, 30, true) }
