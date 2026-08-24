package actor

import (
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// parseGIDReference 是照着 runtime.Stack 首行的老解析写法，只用于给新实现做对照。
func parseGIDReference(t *testing.T) uint64 {
	t.Helper()
	buf := make([]byte, 64)
	n := runtime.Stack(buf, false)
	parts := strings.Fields(string(buf[:n]))
	if len(parts) < 2 || parts[0] != "goroutine" {
		t.Fatalf("runtime.Stack 首行不是预期格式: %q", string(buf[:n]))
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("解析 goroutine ID 失败: %v", err)
	}
	return id
}

// TestCurrentGIDMatchesReference currentGID 改成直接在字节上取数字之后，
// 结果必须与老的 string + strings.Fields + ParseUint 写法完全一致。
// 解析错了不会崩，只会让"我是不是 actor 自己的协程"判断静默失准——
// 那会直接击穿 actor 模型，所以这条必须钉死。
func TestCurrentGIDMatchesReference(t *testing.T) {
	if got, want := currentGID(), parseGIDReference(t); got != want {
		t.Fatalf("currentGID()=%d, 参考实现=%d", got, want)
	}

	// 换几条协程再比，避免只在个位数 ID 上碰巧对上
	const goroutines = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if got, want := currentGID(), parseGIDReference(t); got != want {
				t.Errorf("currentGID()=%d, 参考实现=%d", got, want)
			}
		}()
	}
	wg.Wait()
}

// TestCurrentGIDStableAndDistinct 同一条协程内多次调用必须稳定，
// 不同协程之间必须互不相同——这两条是 ModInvokeFrom 判定"同协程"的全部依据。
func TestCurrentGIDStableAndDistinct(t *testing.T) {
	mine := currentGID()
	if mine == 0 {
		t.Fatal("currentGID 返回 0")
	}
	for i := 0; i < 100; i++ {
		if got := currentGID(); got != mine {
			t.Fatalf("同协程内 GID 变了: %d → %d", mine, got)
		}
	}

	const goroutines = 256
	ids := make(chan uint64, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ids <- currentGID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := map[uint64]bool{mine: true}
	for id := range ids {
		if id == 0 {
			t.Fatal("子协程拿到 GID 0")
		}
		if seen[id] {
			t.Fatalf("GID %d 出现在多条协程上", id)
		}
		seen[id] = true
	}
}

// BenchmarkCurrentGID 量这条"躲不掉时的"路径本身。
// 它的成本几乎全在 runtime.Stack 走栈上，解析部分只占零头——
// 所以真正的优化不是把它变快，而是别调用它（缓存 GID 走 *From 系列接口）。
func BenchmarkCurrentGID(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if currentGID() == 0 {
			b.Fatal("currentGID 返回 0")
		}
	}
}

// BenchmarkRuntimeStackOnly 只量 runtime.Stack，用来说明上面那笔开销的构成。
func BenchmarkRuntimeStackOnly(b *testing.B) {
	var buf [64]byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = runtime.Stack(buf[:], false)
	}
}
