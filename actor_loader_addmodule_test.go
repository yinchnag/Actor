package actor

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// lazyMod 的构造函数**不调** Init，模拟"业务方忘了初始化"这种最常见的写法。
// AddModule 会兜底调一次。
type lazyMod struct {
	ModObj[*lazyMod]
	n int
}

func (m *lazyMod) Bump(d int) int { m.n += d; return m.n }

// TestAddModuleInitsUninitializedModule 构造函数没调 Init 也要能正常工作。
//
// 在 AddModule 兜底之前，这种模块的注册名是空串：两个模块会互相覆盖，
// 所有 ModInvoke 都返回 module not found，而无返回值调用连错误都传不出来——
// 业务上表现为"这个功能就是没反应"，极难排查。
func TestAddModuleInitsUninitializedModule(t *testing.T) {
	m := &lazyMod{} // 故意不调 Init

	if got := m.GameName(); got != "" {
		t.Fatalf("Init 之前不该有模块名, got %q", got)
	}

	loader := NewActorLoader("lazy")
	loader.Init()
	loader.AddModule(m)

	if got := m.GameName(); got != "lazyMod" {
		t.Fatalf("AddModule 之后模块名应为 lazyMod, got %q", got)
	}
	if loader.GetModule("lazyMod") == nil {
		t.Fatal("模块没能按名字注册进去")
	}

	var wg sync.WaitGroup
	loader.Start(&wg)
	defer func() { loader.Close(); wg.Wait() }()

	out, err := loader.ModInvoke("lazyMod", "Bump", 7)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got := int(out[0].Int()); got != 7 {
		t.Fatalf("结果不对: %d", got)
	}
}

// TestModObjInitIsIdempotent Init 必须可以重复调用且第二次是空操作。
//
// 这不只是省一次反射。AddModule 会兜底调 Init，而模块构造函数通常也自己调过；
// 若每次都重建方法表，对一个**已注册、正在被事件循环调用**的模块再 AddModule 一次，
// 就会在读方（shouldWaitResult→GetNumOut、handleTask→Invoke，都不持 modulesMu）
// 读方法表的同时把它整个换掉——实打实的数据竞争。
func TestModObjInitIsIdempotent(t *testing.T) {
	m := &lazyMod{}
	m.Init()

	handlers := m.invokers
	bump := m.invokers["Bump"]
	meta := m.metaMsgHandler
	if handlers == nil || bump == nil {
		t.Fatal("首次 Init 没有建起方法表")
	}

	m.Init()
	m.Init()

	// 比较的是 map 和 handler 的身份，不是内容——重建出来的内容一样，但地址会变，
	// 而竞争恰恰来自"换了一张新表"这个动作本身。
	if !sameMap(handlers, m.invokers) {
		t.Fatal("重复 Init 重建了 invokers：并发下会与事件循环的读打架")
	}
	if m.invokers["Bump"] != bump {
		t.Fatal("重复 Init 换掉了 FuncHandler")
	}
	if !sameMetaMap(meta, m.metaMsgHandler) {
		t.Fatal("重复 Init 重建了 metaMsgHandler")
	}
}

func sameMap(a, b map[string]*FuncHandler) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sameMetaMap(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestAddModuleRejectsNamelessModule 拿不到模块名的模块必须当场炸掉。
//
// 走到这一步说明反射绑定没成功（ModObj 类型参数写错、或手写 IModule 没给名字），
// 这种模块永远不可能被调用到。静默注册的话，故障会推迟到运行期变成
// "module not found"，而无返回值调用连这个错误都不会浮上来。
func TestAddModuleRejectsNamelessModule(t *testing.T) {
	loader := NewActorLoader("nameless")
	loader.Init()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("注册一个没有名字的模块应当 panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "没有模块名") {
			t.Fatalf("panic 信息没说清原因: %v", r)
		}
		// 炸完之后锁必须是放开的，否则整个 loader 就废了
		done := make(chan struct{})
		go func() { defer close(done); loader.GetModule("whatever") }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("panic 之后 modulesMu 没有释放")
		}
	}()

	loader.AddModule(&fakeMod{name: "", meta: map[int]string{}})
}

// TestAddModuleNilIsNoop 传 nil 不该崩，也不该注册任何东西。
func TestAddModuleNilIsNoop(t *testing.T) {
	loader := NewActorLoader("nil-mod")
	loader.Init()
	loader.AddModule(nil)
	if n := len(loader.modules); n != 0 {
		t.Fatalf("传 nil 之后不该有模块, got %d", n)
	}
}
