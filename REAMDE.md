# Actor 框架 —— 使用指南与注意事项

游戏服务器用的 actor 框架：**一个 actor = 一条 goroutine + 一组 Module**。
外部通过反射按方法名调用模块，跨协程调用自动走任务队列，同协程调用直接在栈上执行。

> 本文档的性能数字均为本仓库基准与压测的实测值（28 核 Windows，Go 1.24）。
> 基准本身有 ±10% 的运行间波动，表中取的是多次运行的代表值；
> 关心的是数量级差异而不是具体数字，可用 `go test -bench=. -benchmem ./...` 自行复现。
> 原始设计要求见 `开发文档_基础版.md`，调试技巧见 `DEBUG_GUIDE.md`，
> 一个跑通真实 MySQL/Redis 的完整示例见 `cmd/noteserver/`。

---

## 一、核心模型

```
        ┌─────────────────── 一个 actor = 一条 goroutine ────────────────────┐
        │                                                                    │
 外部   │   taskChan(64)          事件循环 select                             │
 协程 ──┼──▶ [task][task]… ──▶  ├─ <-stopChan  → 排空退出                    │
        │                        ├─ <-ticker.C  → Update(dt) 所有模块         │
        │                        └─ <-taskChan  → 反射调用模块方法             │
        │                                                                    │
        │   Module A   Module B   Module C   ← 状态由这条协程独占，无需加锁    │
        └────────────────────────────────────────────────────────────────────┘
```

**这条协程是单消费者。** 整个框架的收益和陷阱都源自这一条：模块状态天然免锁，
但模块方法里任何一次阻塞都会让**整个 actor 停摆**。

---

## 二、快速开始

### 1. 定义模块

```go
type ScoreMod struct {
    actor.ModObj[*ScoreMod]  // CRTP：把自己的类型传给基类
    score int                // 普通字段，不需要锁
}

func NewScoreMod() *ScoreMod {
    m := &ScoreMod{}
    m.Init()   // 必须调用，且此后不能再拷贝这个结构体（见注意事项 9）
    return m
}

// 公开方法自动被反射注册，可通过方法名调用
func (m *ScoreMod) Add(delta int) int { m.score += delta; return m.score }
func (m *ScoreMod) Get() int          { return m.score }
```

模块名取自宿主结构体的类型名，这里就是 `"ScoreMod"`。

### 2. 启动、调用、关闭

```go
loader := actor.NewActorLoader("player-1001")
loader.Init()
loader.AddModule(NewScoreMod())

var wg sync.WaitGroup
loader.Start(&wg)          // 用 Start，不要 go RunUpdateLoop（见注意事项 10）

out, err := loader.ModInvoke("ScoreMod", "Add", 10)
if err != nil { /* 见第五节的错误语义 */ }
score := int(out[0].Int())

loader.Close()
wg.Wait()                  // Close 只是发信号，Wait 才是真正退出
```

### 3. 协议分发（前端消息）

```go
type ScoreMsg struct{ Points int }

func init() { actor.RegisterProtocol(5001, ScoreMsg{}) }

// 单入参 + 无返回值 + 入参类型已注册 → 自动认成 5001 的处理器
func (m *ScoreMod) OnScoreMsg(msg ScoreMsg) { m.score += msg.Points }

// 派发（热路径请用 From 变体，见注意事项 5）
gid := actor.CurrentGID()
for msg := range conn.Recv() {
    loader.OnMessageHandlerFrom(gid, msg)
}
```

---

## 三、方法反射的注册规则

`ModObj.Init()` 扫描宿主类型的方法，按以下规则登记：

| 规则 | 说明 |
|---|---|
| 只登记**公开**方法 | 小写方法名不会被注册，可安全用作内部辅助 |
| 跳过 `baseMethodSet` | `Init/Save/Load/Invoke/Update/GetMetaHandler/IsDirty/SetDirty/GetNumOut/GetNumIn/MetaHandlers` |
| **协议处理器判定** | 恰好 1 个入参 + 0 个返回值 + 入参类型已 `RegisterProtocol` |
| 其余公开方法 | 普通 invoker，可用 `ModInvoke` 按名调用 |

两点容易踩空：

- `GameName` 不在 `baseMethodSet` 里，会被登记成一个 invoker。无害，但列举方法时会看到它。
- 给 `ModObj` 新增基类方法时**必须同步加进 `baseMethodSet`**，否则它会被当成业务方法暴露出去。

---

## 四、调用路径

```go
// 便捷版：内部自己解析 GID（每次约 1690ns）
loader.ModInvoke("Mod", "Method", args...)

// 热路径版：GID 由调用方缓存后传入
gid := actor.CurrentGID()
loader.ModInvokeFrom(gid, "Mod", "Method", args...)
```

框架按 `callerGID` 与 actor 自身 GID 是否相同来决定走哪条路：

| 情形 | 行为 |
|---|---|
| 同协程（模块方法内部调自己的 loader） | `directInvoke`，直接在当前栈上执行，不入队 |
| 跨协程 + 方法**有**返回值 | 入队 + `Await`，最长等 3 秒 |
| 跨协程 + 方法**无**返回值 | 入队即返回，结果与错误都没人接（见注意事项 7） |
| 事件循环尚未启动 | 只放行第一个使用它的协程，其余返回 `ErrActorNotStarted` |

---

## 五、错误语义

这套区分是给**写操作**用的，别丢：

| 错误 | 含义 | 该怎么办 |
|---|---|---|
| `ErrTaskCanceled`（与 `ErrTaskAwaitTimeout` 一同返回） | 等待超时，任务在被取用前就已取消，**方法一次都没执行** | 可以直接安全重试 |
| `ErrTaskAwaitTimeout`（不带取消） | 同样超时，但方法**可能正在执行或已执行完** | 重试前必须自行去重 |
| `ErrTaskQueueTimeout` | 队列满，3 秒内没投递进去 | 过载，退避重试 |
| `ErrCallCycle` | 跨 actor 同步调用成环 | **重试无用**，得改调用结构 |
| `ErrActorNotStarted` | 循环未启动且调用方不是初始化持有者 | 用 `Start` 启动后再调 |

有返回值的方法是两层错误，两层都要看：

```go
out, err := loader.ModInvokeFrom(gid, "Mod", "Method", arg)
if err != nil { /* 投递/调度层失败 */ }
if bizErr, _ := out[1].Interface().(error); bizErr != nil { /* 方法自己返回的业务错误 */ }
```

---

## 六、注意事项

### 1. 模块方法里的任何阻塞，都会钉住整个 actor

事件循环是单消费者。一次 3 秒的慢查询，就是这个 actor 上**所有**排队请求都多等 3 秒；
队列只有 64 槽，满了之后投递方还要各自再卡 3 秒。

对策：模块方法只做**内存里的状态变更**和**受限时长的 I/O**。
真正的重活拆出去异步做，做完再用无返回值方法把结果投递回来。

### 2. 数据库/网络调用必须显著快于 3 秒

`defaultTaskTimeout` 写死 3 秒。超过它，调用方已经拿着超时错误走人，
而 actor 还在那儿干等——后面排队的请求全被连累。

给外部 I/O 设一个明显更小的超时（示例里用 2 秒），别指望框架帮你兜。

### 3. CPU 密集的活别放进模块方法

典型是密码哈希这类**刻意设计成慢**的算法（bcrypt 在默认 cost 下是数十毫秒量级）。
放进事件循环，这个 actor 在这段时间里什么都干不了。

`cmd/noteserver` 的做法：哈希的计算与比对都留在 HTTP 协程，actor 只碰数据库。

### 4. 跨 actor **同步**调用会成环死锁

A 的模块方法同步调 B，此刻 A 的事件循环停摆、消费不了自己的队列；
B 若再回调 A，那个任务就一直躺在 A 的队列里——`A→B→A`、`A→B→C→A` 都是死锁。

框架有等待图检测，成环会**当场**返回 `ErrCallCycle`（实测两环 618µs、三环 <1ms），
而不是让整条环停摆 3 秒。但检测只是止损，**正确做法是别让调用成环**：

- 分层：玩家 actor → 公会 actor → 全局 actor，只允许单向调用；
- 或者把环上任意一跳改成**无返回值**调用——不构成等待边，环自然断开。
  「处理完顺手回推一条通知给调用方」这种写法始终是安全的，不会被误判。

检测覆盖不到两种情况，它们仍由 3 秒超时兜底：
事件循环卡在**非 actor** 的东西上（互斥锁、IO、没人写的 channel）；
以及投递阻塞在满队列上。

### 5. 缓存 GID，别让 `currentGID` 进热路径

`currentGID` 靠 `runtime.Stack` 解析调用栈，实测约 **1690 ns/op**，
其中约 **83% 是 `runtime.Stack` 本身，压不动**。唯一的优化是**别调用它**：

```go
gid := actor.CurrentGID()        // 每条长期存活的协程只取一次
for { loader.ModInvokeFrom(gid, ...) }
```

### 6. 模块方法内调自己的 loader，用 `GetGoroutineID()`

模块方法本来就跑在 actor 自己的协程上，`loader.GetGoroutineID()` 就是当前 GID，
一次原子读而已，没必要再解析一次栈：

```go
func (m *Mod) Recurse(n int) int {
    // 不要用 ModInvoke —— 每层都要解析一次调用栈，栈越深越贵
    out, _ := m.self.ModInvokeFrom(m.self.GetGoroutineID(), "Mod", "Recurse", n-1)
    ...
}
```

实测 500 层递归：`ModInvoke` 每层 **250µs**，改用 `GetGoroutineID()` 后 **361ns**——
差约 **690 倍**，且深度越大差距越明显（`runtime.Stack` 要走完整条栈）。

### 7. 无返回值调用的失败，必须自己接管

无返回值调用没有调用方在等结果，失败没有任何地方可以返回。两类失败都很隐蔽：

- **送不到**（队列满、actor 已关闭）：消息直接丢了；
- **执行失败**（参数类型不匹配、方法找不到、panic）：错误产生在事件循环那侧，
  存进任务里就随任务回池了——最典型的是**协议 ID 与消息载荷类型对不上**，
  消息凭空消失，不报错也不崩。

```go
loader.SetDiscardedErrorHandler(func(e actor.DiscardedError) {
    metrics.Inc("actor.discarded", e.Phase.String())  // PhaseDeliver / PhaseInvoke
    log.Warnf("%v", e)
})
n := loader.DiscardedErrors()   // 累计计数，供监控采样
```

不接管时框架会往 stderr 打日志，但**限流到每秒一条**（队列打满时一条消息一行会淹掉日志）。
回调在发现失败的那条协程上**同步**执行，务必轻量，更不要在里面回调进同一个 actor。

### 8. 别把模块内部的切片/map 直接返回出去

返回值会被调用方（另一条协程）拿走，而底层数组还是这个 actor 的：

```go
func (m *Mod) List() []Item {
    out := make([]Item, len(m.cache))
    copy(out, m.cache)    // 必须拷贝
    return out
}
```

直接 `return m.cache` 的话，下一次往 `m.cache` 追加就是数据竞争。
actor 免锁的前提是**状态不外泄**。

### 9. `Init()` 之后模块结构体不能再被拷贝

`ModObj` 靠字段偏移反推宿主对象指针。拷贝之后新对象里的指针还指向旧对象，
所有反射调用都会打到错误的实例上，而且**不报错**。

始终用 `&T{}` 构造、按指针传递，别把模块放进会发生值拷贝的容器。

`Init()` 本身是幂等的，重复调用是空操作——`AddModule` 会兜底调一次，
所以构造函数里调不调都能正常工作：

```go
func NewBagMod(p *PlayerEnt) *BagMod { return &BagMod{host: p} }          // 可以
func NewBagMod(p *PlayerEnt) *BagMod { m := &BagMod{host: p}; m.Init(); return m }  // 也可以
```

区别只在**模块名什么时候可用**：不在构造时调 Init 的话，`GameName()` 在
`AddModule` 之前一直是空串。想让模块脱离 loader 单独跑单元测试，就在构造函数里调。

注册时拿不到模块名会直接 panic——那说明反射绑定失败（最常见是 `ModObj` 的类型参数
写错成了别的类型），这种模块永远不可能被调用到，与其运行期对着 `module not found`
排查，不如启动就炸。

### 10. 用 `Start` 启动，不要 `go RunUpdateLoop`

`go RunUpdateLoop` 到循环内部写入 `goroutineID` 之间有一个窗口。
窗口内的调用会被当成「未启动」处理，而框架此时只放行第一个调用方，其余返回
`ErrActorNotStarted`。`Start` 返回时 `goroutineID` 已发布，窗口不存在。

`Start` 会自己 `wg.Add(1)`，调用方只需 `Wait`，不要重复 Add。

### 11. 关闭要讲顺序：先停上游，再关 actor

反了的话在途请求会撞上一堆「actor 已关闭」。`cmd/noteserver` 的做法是先
`http.Server.Shutdown` 等在途请求跑完，再关 actor。

`Close()` 只是发信号并排空队列，**必须 `wg.Wait()`** 才能确认事件循环真的退出了。
关闭时队列里的任务会被结算成错误，无返回值的那些会走丢弃上报（见注意事项 7）。

### 12. 模块方法 panic 会关掉整个 actor

`handleTask` 会 recover，把 panic 当错误回传给调用方，然后**关闭这个 actor**。
这是刻意的——模块状态在 panic 之后已经不可信了，继续服务只会扩散错误。
但意味着一个没处理好的边界条件会让这个玩家/公会直接掉线，模块方法里该防的还得防。

### 13. 结构相同的协议类型可以互相冒充

`ModObj.Invoke` 在类型不能直接赋值时会退回 `ConvertibleTo`。于是
`type A struct{N int}` 和 `type B struct{N int}` 底层一致，互相传入不会报错。
协议类型建议加上区分性字段，别只靠类型名。

### 14. 这两个值写死在代码里，不可配置

- `defaultTaskTimeout = 3 * time.Second`（`actor_loader.go`）
- `taskChan` 容量 64（`NewActorLoader`）

不同类型的 actor 需要的预算差别很大，目前只能改源码。

### 15. `errActorClosed` 未导出

`ErrTaskQueueTimeout` / `ErrTaskAwaitTimeout` / `ErrTaskCanceled` / `ErrCallCycle` /
`ErrActorNotStarted` 都导出了，唯独「actor 已关闭」没有，
外部包无法用 `errors.Is` 精确判定，只能靠 `IsClose()` 或归入兜底分支。

---

## 七、实测性能

| 路径 | 耗时 | 分配 |
|---|---|---|
| 同协程 `directInvoke` | 272 ns/op | 5 allocs |
| `ModInvokeFrom`（缓存 GID） | 2540 ns/op | 13 allocs |
| `ModInvoke`（自己解析 GID） | 6020 ns/op | 14 allocs |
| `currentGID` 单独 | 1690 ns/op | 1 alloc |
| └ 其中 `runtime.Stack` | 1390 ns/op | 0 allocs |
| `OnMessageHandlerFrom` | 649 ns/op | 7 allocs |
| `OnMessageHandler` | 3280 ns/op | 8 allocs |

| 场景 | 结果 |
|---|---|
| 单 actor 吞吐 | 96 万 ops/s，均值 29µs，p99 1.0ms |
| 2000 actor 同时在线 | 171 万 ops/s 聚合，goroutine 精确 2→2002 |
| 28 万次在途调用的堆增量 | +4.0MB（约 15 B/次），静置后回落基线 |
| 环形调用检出 | 两环 618µs，三环 <1ms（无检测时卡满 3s） |
| 协议分发查表 | 与模块数无关：30 个模块 3.8ns / 0 分配 |

协议分发的查表是 O(该协议的处理器数) 而非 O(模块数)——索引在 `AddModule` 时建好，
派发侧只做一次原子读，不加锁不分配。所以给 actor 挂几十个模块不会拖慢消息派发。

---

## 八、完整示例

`cmd/noteserver/` 是一个基于本框架的 HTTP 服务（手机号注册、登录、上传/获取笔记），
跑通过真实 MySQL + Redis。它演示了本文档里几乎所有注意事项的落地方式：

- 按业务键分片的 actor（注册的「查重—插入」串行化）
- 每用户一个 actor + 空闲回收（在线用户数决定协程数）
- CPU 密集的活留在调用方协程
- 超时语义直接映射成 HTTP 状态码
- 哪些操作**不**该进 actor

详见 `cmd/noteserver/README.md`。

---

## 九、测试与调试

```bash
go test ./... -count=1                        # 全部用例
go test -race ./... -count=1                  # 竞态检测，强烈建议常开
ACTOR_STRESS=1 go test -race ./... -count=1   # 加上重压用例
go test ./... -short                          # 跳过需要等满 3s 超时的用例
go test -bench=. -benchmem ./...              # 全部基准
```

`tests/goroutine_leak/` 用 goleak 逐用例检查协程泄漏，覆盖队列打满、模块方法 panic、
关闭时排空、跨 actor 关闭、超时取消、actor 大批量创建销毁等边界。
新增任何涉及任务结算或生命周期的改动，都应先在这里加一条用例。

测试策略详见 `docs/testing-strategy.md`，断点与排查技巧见 `DEBUG_GUIDE.md`。
