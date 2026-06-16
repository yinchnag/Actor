# Actor 框架测试策略

## 现有测试覆盖范围

| 测试文件 | 覆盖内容 |
|----------|----------|
| `actor_loader_test.go` | 同协程调用、跨协程调用、协议分发 |
| `actor_loader_stress_test.go` | 高并发压测（`ACTOR_STRESS=1`）、超时路径、单链路 benchmark |
| `chan_task_test.go` | 任务完成、Await、超时 |
| `mod_obj_test.go` | 方法绑定、元数据、Dirty 标记 |

## 当前盲区

**稳定性：**
- 超时路径会启动清理 goroutine（`actor_loader.go:147`），若任务长期不完成会泄漏，目前无检测
- 没有长跑 soak 测试，无法发现内存缓慢增长
- 没有 Close 竞态测试——在有任务 in-flight 时关闭 loader 是否安全退出

**承载性：**
- 现有 benchmark 为单 caller goroutine，测的是单链路延迟，不是吞吐量
- 没有多 actor 互调拓扑测试
- 没有任务队列打满后的行为观测

---

## 五个测试方向

### 方向一：Goroutine 泄漏检测（优先级最高）

**背景：** 超时路径是框架最复杂的执行分支，当前完全无覆盖。超时后会启动一个后台 goroutine 等待任务完成并释放，若任务卡死则该 goroutine 永远挂起。

**方案一：** 引入 `go.uber.org/goleak`，在 `TestMain` 统一检测：

```go
func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m)
}
```

**方案二：** 手动对比 `runtime.NumGoroutine()`：

```go
func TestNoGoroutineLeakOnTimeout(t *testing.T) {
    before := runtime.NumGoroutine()

    loader := NewActorLoader("leak-test")
    loader.Init()
    loader.AddModule(NewSlowCalcMod())

    var wg sync.WaitGroup
    wg.Add(1)
    go loader.RunUpdateLoop(&wg)

    // 制造 N 个超时任务（SlowSum 执行 4s，默认超时 3s）
    for i := 0; i < 5; i++ {
        loader.ModInvoke("SlowCalcMod", "SlowSum", 1, 2)
    }

    // 等待 SlowSum 实际完成，清理 goroutine 退出
    time.Sleep(5 * time.Second)
    loader.Close()
    wg.Wait()

    after := runtime.NumGoroutine()
    if after > before+2 {
        t.Fatalf("goroutine leak: before=%d after=%d", before, after)
    }
}
```

**运行时机：** 每次 PR，纳入常规 `go test -race ./...`。

---

### 方向二：并发吞吐量 Benchmark

**背景：** 现有 benchmark 是单 goroutine 串行调用，只能测单链路延迟。实际业务场景是多个 goroutine 同时向同一个 actor 发起调用。

**方案：** 用 `b.RunParallel` 模拟多 caller，配合 `-cpu` flag 观察扩展性：

```go
func BenchmarkActorLoaderConcurrentCallers(b *testing.B) {
    loader := NewActorLoader("bench-concurrent")
    loader.Init()
    loader.AddModule(NewSlowCalcMod())

    var wg sync.WaitGroup
    wg.Add(1)
    go loader.RunUpdateLoop(&wg)
    defer func() { loader.Close(); wg.Wait() }()

    b.ReportAllocs()
    b.ResetTimer()

    b.RunParallel(func(pb *testing.PB) {
        gid := CurrentGID() // 每个 goroutine 缓存一次
        for pb.Next() {
            out, err := loader.ModInvokeFrom(gid, "SlowCalcMod", "Sum", 1, 2)
            if err != nil {
                b.Errorf("invoke failed: %v", err)
                return
            }
            if len(out) != 1 || int(out[0].Int()) != 3 {
                b.Errorf("unexpected result")
            }
        }
    })
}
```

**运行方式：**

```bash
go test -bench=BenchmarkActorLoaderConcurrentCallers -cpu=1,2,4,8 -benchmem -count=3
```

通过 `-cpu` 参数对比不同并发度下的 ops/sec，判断吞吐是否随并发线性扩展，以及在哪个点出现瓶颈（通常是 task 队列或锁竞争）。

---

### 方向三：任务队列饱和行为测试

**背景：** task 队列容量为 64（`NewActorLoader` 中硬编码）。队列打满时返回 `ErrTaskQueueTimeout`，需要验证饱和后**不影响后续正常请求的恢复**。

```go
func TestTaskQueueSaturationRecovery(t *testing.T) {
    loader := NewActorLoader("sat-test")
    loader.Init()
    loader.AddModule(NewSlowCalcMod())
    // 不启动 RunUpdateLoop，使队列无人消费

    var wg sync.WaitGroup
    saturated := make(chan struct{})

    // 并发填满队列
    for i := 0; i < 70; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            loader.ModInvoke("SlowCalcMod", "Sum", 1, 2)
        }()
    }

    // 等一段时间让队列填满，然后应能观察到 ErrTaskQueueTimeout
    time.Sleep(200 * time.Millisecond)
    close(saturated)

    // 现在启动 loop，队列应当正常排空
    var loopWG sync.WaitGroup
    loopWG.Add(1)
    go loader.RunUpdateLoop(&loopWG)

    wg.Wait()

    // 队列排空后，新请求应当正常响应
    out, err := loader.ModInvoke("SlowCalcMod", "Sum", 3, 4)
    if err != nil {
        t.Fatalf("loader unhealthy after saturation: %v", err)
    }
    if len(out) != 1 || int(out[0].Int()) != 7 {
        t.Fatalf("unexpected result after saturation")
    }

    loader.Close()
    loopWG.Wait()
}
```

---

### 方向四：多 Actor 互调拓扑测试

**背景：** 实际游戏服务器中 actor 之间会互相调用（玩家 actor → 场景 actor → 战斗 actor）。这是框架最容易出死锁的场景，目前完全没有测试覆盖。

**典型拓扑：**

```
调用方 goroutine ──→ Actor A（角色）──→ Actor B（场景）
```

```go
// SceneMod 属于 Actor B
type SceneMod struct {
    ModObj[*SceneMod]
}

func (m *SceneMod) GetPosition() int { return 42 }

// PlayerMod 属于 Actor A，持有对 Actor B 的引用
type PlayerMod struct {
    ModObj[*PlayerMod]
    sceneActor IModLoader
}

func (m *PlayerMod) QueryScene() int {
    out, _ := m.sceneActor.ModInvoke("SceneMod", "GetPosition")
    if len(out) == 0 { return -1 }
    return int(out[0].Int())
}

func TestMultiActorTopology(t *testing.T) {
    sceneLoader := NewActorLoader("scene")
    sceneLoader.Init()
    sceneLoader.AddModule(&SceneMod{})

    playerMod := &PlayerMod{sceneActor: sceneLoader}
    playerMod.Init()
    playerLoader := NewActorLoader("player")
    playerLoader.Init()
    playerLoader.AddModule(playerMod)

    var wg sync.WaitGroup
    wg.Add(2)
    go sceneLoader.RunUpdateLoop(&wg)
    go playerLoader.RunUpdateLoop(&wg)
    defer func() {
        sceneLoader.Close()
        playerLoader.Close()
        wg.Wait()
    }()

    time.Sleep(20 * time.Millisecond)

    out, err := playerLoader.ModInvoke("PlayerMod", "QueryScene")
    if err != nil {
        t.Fatalf("cross-actor invoke failed: %v", err)
    }
    if len(out) != 1 || int(out[0].Int()) != 42 {
        t.Fatalf("unexpected cross-actor result")
    }
}
```

**需要特别注意的死锁场景：** A 等 B 返回，同时 B 又在等 A——两个 actor 互相等对方时，3s 超时会把其中一个唤醒，但需要验证超时后系统仍然健康。

---

### 方向五：长跑 Soak 测试（内存稳定性）

**背景：** 短促的压测无法发现内存缓慢泄漏。Soak 测试持续运行一段时间，每隔固定周期采样 `runtime.MemStats`，验证堆内存没有单调增长趋势。

```go
func TestSoak(t *testing.T) {
    duration := 2 * time.Minute
    if d := os.Getenv("ACTOR_SOAK_DURATION"); d != "" {
        var err error
        duration, err = time.ParseDuration(d)
        if err != nil {
            t.Fatalf("invalid ACTOR_SOAK_DURATION: %v", err)
        }
    }
    if os.Getenv("ACTOR_STRESS") != "1" {
        t.Skip("set ACTOR_STRESS=1 to run soak test")
    }

    loader := NewActorLoader("soak-actor")
    loader.Init()
    loader.AddModule(NewSlowCalcMod())

    var wg sync.WaitGroup
    wg.Add(1)
    go loader.RunUpdateLoop(&wg)
    defer func() { loader.Close(); wg.Wait() }()

    deadline := time.Now().Add(duration)
    var samples []uint64
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    // 后台持续发请求
    go func() {
        gid := CurrentGID()
        for time.Now().Before(deadline) {
            loader.ModInvokeFrom(gid, "SlowCalcMod", "Sum", 1, 2)
        }
    }()

    for time.Now().Before(deadline) {
        <-ticker.C
        var ms runtime.MemStats
        runtime.ReadMemStats(&ms)
        samples = append(samples, ms.HeapInuse)
        t.Logf("HeapInuse: %d KB", ms.HeapInuse/1024)
    }

    // 简单趋势判断：最后 1/3 均值不应超过最初 1/3 均值的 150%
    n := len(samples)
    if n >= 6 {
        early := avg(samples[:n/3])
        late := avg(samples[n-n/3:])
        if late > early*3/2 {
            t.Fatalf("memory growth detected: early avg=%d KB, late avg=%d KB",
                early/1024, late/1024)
        }
    }
}

func avg(xs []uint64) uint64 {
    var s uint64
    for _, x := range xs {
        s += x
    }
    return s / uint64(len(xs))
}
```

**运行方式：**

```bash
# 默认 2 分钟
ACTOR_STRESS=1 go test -run TestSoak -v -timeout 10m

# 自定义时长
ACTOR_STRESS=1 ACTOR_SOAK_DURATION=10m go test -run TestSoak -v -timeout 20m
```

---

## 执行计划

| 优先级 | 测试 | 触发时机 | 预计耗时 |
|--------|------|----------|----------|
| P0 | Goroutine 泄漏检测 | 每次 PR（`go test -race ./...`） | < 10s |
| P1 | 并发吞吐 Benchmark | 每次 PR（`-cpu=1,2,4,8`） | < 30s |
| P1 | 队列饱和恢复 | 每次 PR | < 5s |
| P2 | 多 Actor 拓扑 | 每次 PR | < 5s |
| P3 | Soak 长跑 | Nightly / 手动 | 2~10 分钟 |