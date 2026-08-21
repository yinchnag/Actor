package actor

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type SlowCalcMod struct {
	ModObj[*SlowCalcMod]
}

func NewSlowCalcMod() *SlowCalcMod {
	m := &SlowCalcMod{}
	m.Init()
	return m
}

func (that *SlowCalcMod) Sum(a, b int) int {
	return a + b
}

func (that *SlowCalcMod) SlowSum(a, b int) int {
	time.Sleep(4 * time.Second)
	return a + b
}

// TestActorLoaderCrossGoroutineHighConcurrencyStability verifies correctness and no panic under pressure.
func TestActorLoaderCrossGoroutineHighConcurrencyStability(t *testing.T) {
	if os.Getenv("ACTOR_STRESS") != "1" {
		t.Skip("set ACTOR_STRESS=1 to run heavy stress test")
	}

	loader := NewActorLoader("user-actor")
	loader.Init()
	loader.AddModule(NewSlowCalcMod())

	var loopWG sync.WaitGroup
	loader.Start(&loopWG)
	defer func() {
		loader.Close()
		loopWG.Wait()
	}()

	workers := runtime.NumCPU() * 8
	if workers < 16 {
		workers = 16
	}
	perWorkerCalls := 300

	var bizErrCount int64
	var timeoutCount int64
	var successCount int64
	var panicCount int64

	var callWG sync.WaitGroup
	callWG.Add(workers)

	for w := 0; w < workers; w++ {
		go func() {
			defer callWG.Done()
			defer func() {
				if recover() != nil {
					atomic.AddInt64(&panicCount, 1)
				}
			}()

			myGID := currentGID() // cached once per worker goroutine
			for i := 0; i < perWorkerCalls; i++ {
				out, err := loader.ModInvokeFrom(myGID, "SlowCalcMod", "Sum", i, i)
				if err != nil {
					if errors.Is(err, ErrTaskQueueTimeout) || errors.Is(err, ErrTaskAwaitTimeout) {
						atomic.AddInt64(&timeoutCount, 1)
						continue
					}
					atomic.AddInt64(&bizErrCount, 1)
					continue
				}
				if len(out) != 1 || int(out[0].Int()) != i+i {
					atomic.AddInt64(&bizErrCount, 1)
					continue
				}
				atomic.AddInt64(&successCount, 1)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		callWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("high concurrency invoke test timeout")
	}

	if panicCount > 0 {
		t.Fatalf("panic count > 0: %d", panicCount)
	}
	if bizErrCount > 0 {
		t.Fatalf("unexpected error count > 0: %d", bizErrCount)
	}
	if successCount == 0 {
		t.Fatalf("success count should be > 0")
	}
	t.Logf("high-concurrency stats: success=%d timeout=%d", successCount, timeoutCount)
}

// TestActorLoaderTaskTimeoutPath checks that timeout path returns and does not crash.
func TestActorLoaderTaskTimeoutPath(t *testing.T) {
	loader := NewActorLoader("user-actor")
	loader.Init()
	loader.AddModule(NewSlowCalcMod())

	var loopWG sync.WaitGroup
	loader.Start(&loopWG) // 返回时 goroutineID 已发布，不需要轮询等待
	defer func() {
		loader.Close()
		loopWG.Wait()
	}()

	start := time.Now()
	_, err := loader.ModInvoke("SlowCalcMod", "SlowSum", 1, 2)
	if !errors.Is(err, ErrTaskAwaitTimeout) {
		t.Fatalf("expected timeout error, got: %v", err)
	}

	elapsed := time.Since(start)
	if elapsed > 3500*time.Millisecond {
		t.Fatalf("timeout path too slow, elapsed=%v", elapsed)
	}

	// Wait a little for late completion + pool release, then call again to ensure loader stays healthy.
	time.Sleep(1500 * time.Millisecond)
	out, err := loader.ModInvoke("SlowCalcMod", "Sum", 3, 4)
	if err != nil {
		t.Fatalf("loader unhealthy after timeout path: %v", err)
	}
	if len(out) != 1 || int(out[0].Int()) != 7 {
		t.Fatalf("unexpected result after timeout path")
	}
}

// BenchmarkActorLoaderCrossGoroutineReturnValue measures cross-goroutine invoke
// with callerGID cached once per goroutine (the optimized hot path).
func BenchmarkActorLoaderCrossGoroutineReturnValue(b *testing.B) {
	loader := NewActorLoader("bench-actor")
	loader.Init()
	loader.AddModule(NewSlowCalcMod())

	var loopWG sync.WaitGroup
	loader.Start(&loopWG)
	defer func() {
		loader.Close()
		loopWG.Wait()
	}()

	myGID := currentGID() // cached once, simulating a long-lived caller goroutine
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out, err := loader.ModInvokeFrom(myGID, "SlowCalcMod", "Sum", 1, 2)
		if err != nil {
			b.Fatalf("invoke failed: %v", err)
		}
		if len(out) != 1 || int(out[0].Int()) != 3 {
			b.Fatalf("unexpected result")
		}
	}
}

// BenchmarkActorLoaderCrossGoroutineUncached measures the same cross-goroutine invoke
// but calls currentGID() via ModInvoke on every iteration (baseline for comparison).
func BenchmarkActorLoaderCrossGoroutineUncached(b *testing.B) {
	loader := NewActorLoader("bench-actor-uncached")
	loader.Init()
	loader.AddModule(NewSlowCalcMod())

	var loopWG sync.WaitGroup
	loader.Start(&loopWG)
	defer func() {
		loader.Close()
		loopWG.Wait()
	}()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out, err := loader.ModInvoke("SlowCalcMod", "Sum", 1, 2)
		if err != nil {
			b.Fatalf("invoke failed: %v", err)
		}
		if len(out) != 1 || int(out[0].Int()) != 3 {
			b.Fatalf("unexpected result")
		}
	}
}

func BenchmarkActorLoaderDirectInvokeBaseline(b *testing.B) {
	loader := NewActorLoader("bench-direct")
	loader.Init()
	loader.AddModule(NewSlowCalcMod())

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out, err := loader.directInvoke("SlowCalcMod", "Sum", 1, 2)
		if err != nil {
			b.Fatalf("direct invoke failed: %v", err)
		}
		if len(out) != 1 || int(out[0].Int()) != 3 {
			b.Fatalf("unexpected result")
		}
	}
}
