# 调试指南 (Debugging Guide)

本项目配置了 VS Code 调试环境，支持以下场景。

## 快速开始

### 1. 调试 deadlock_demo

在 VS Code 的 **Run and Debug** 面板（快捷键 `Ctrl+Shift+D`）中：

1. 选择配置：**"Debug deadlock_demo"**
2. 按 `F5` 启动调试
3. 在代码中设置断点（点击行号左侧）
4. 按 `Ctrl+C` 或点击停止按钮退出程序

**特性**：
- 支持在 main.go、actor_loader.go 等源文件中设置断点
- 观察调用栈（Call Stack）和局部变量（Local Variables）
- 在调试控制台运行 Go 表达式（Debug Console）

---

### 2. 运行 deadlock_demo（无调试）

如果只想运行程序而不调试：

1. 打开运行配置：**"Run deadlock_demo (no debug)"**
2. 按 `F5` 或 `Ctrl+F5`
3. 输出显示在集成终端中，Ctrl+C 退出

---

### 3. 单元测试

#### 基础测试
```
配置名：Test - Actor core tests
说明：运行所有 *_test.go 文件，不包括压力测试
快捷键：F5 选择配置后回车
```

#### Race 检测（推荐）
```
配置名：Test - Race detector
说明：带 -race 标志检测数据竞争（Data Race）
预期：无输出 = 无竞争，有输出 = 发现竞争
耗时：比普通测试慢 5-10 倍
```

#### 高并发压力测试
```
配置名：Test - High concurrency (ACTOR_STRESS=1)
说明：TestActorLoaderCrossGoroutineHighConcurrencyStability
需要：设置环境变量 ACTOR_STRESS=1（配置已包含）
耗时：~30 秒
观察：panic=0, success>0 = 通过
```

---

### 4. 性能基准测试（Benchmark）

#### 已缓存 GID（推荐路径）
```
配置名：Benchmark - Cross-goroutine (cached)
说明：ModInvokeFrom(cachedGID, ...) —— 无 runtime.Stack 开销
预期：~2,750 ns/op, 13 allocs/op
```

#### 未缓存 GID（对比基线）
```
配置名：Benchmark - Cross-goroutine (uncached)
说明：ModInvoke(...) —— 每次调用 runtime.Stack
预期：~5,840 ns/op, 15 allocs/op
性能比：~2.1× 较慢
```

#### 全量对比
```
配置名：Benchmark - All comparisons
说明：同时运行缓存、未缓存、直接调用三条基准
可视化：观察三条曲线的性能梯度
```

---

## 任务（Tasks）

在 VS Code 菜单 **Terminal → Run Task** 中选择：

| 任务名 | 命令 | 说明 |
|---|---|---|
| **Build deadlock_demo** | `go build -o bin/deadlock_demo.exe ./cmd/deadlock_demo` | 编译 demo，输出到 bin 目录 |
| **Run unit tests** | `go test ./... -v -count=1` | 基础单元测试 |
| **Run tests with race detector** | `go test -race ./... -count=1` | 竞争检测（推荐运行） |
| **Run stress test (ACTOR_STRESS=1)** | 同上 + ACTOR_STRESS=1 | 高并发压力 |
| **Run benchmarks - all** | 跑所有 Benchmark | 三条基准一起比较 |
| **Run benchmarks - cached GID** | 只跑缓存版 | 快速验证优化效果 |
| **Run benchmarks - uncached GID** | 只跑未缓存版 | 对比基线 |
| **Format code** | `go fmt ./...` | 代码格式化 |
| **Run vet** | `go vet ./...` | 静态分析 |

---

## 常用快捷键

| 快捷键 | 功能 |
|---|---|
| `F5` | 启动调试（启动或继续） |
| `F10` | Step Over（单步跨入函数） |
| `F11` | Step Into（单步进入函数调用） |
| `Shift+F11` | Step Out（跳出当前函数） |
| `Ctrl+Shift+D` | 打开 Run and Debug 面板 |
| `Ctrl+Shift+` ` | 打开集成终端 |
| `Ctrl+K Ctrl+C` | 注释代码块 |
| `Ctrl+Shift+P` | 命令面板（运行 Task） |

---

## 调试案例

### 案例 1：追踪 deadlock_demo 中的互锁

1. 打开 `cmd/deadlock_demo/main.go`
2. 在 `ModA.Process` 方法的 `loaderB.ModInvokeFrom(...)` 一行设置断点
3. 启动 **Debug deadlock_demo**
4. 第一次命中断点时，观察：
   - `gid`（当前协程 ID）
   - `that.loaderB`（指向 Actor B 的引用）
   - 呼叫栈（Call Stack）显示来自哪个 Worker
5. 继续执行 (`F5`)，观察程序在 B 的 Double 方法中再次阻塞
6. 设置第二个断点在 `ModB.Double` 的 `loaderA.ModInvokeFrom(...)`
7. 比较两个协程的行为

### 案例 2：测试 Race 检测是否有效

运行 **Test - Race detector**：
```bash
go test -race ./... -count=1
```

如果输出为 `ok`，说明无竞争。如果有竞争信息如：
```
==================
WARNING: DATA RACE
Write at 0x... by goroutine ...
    actor_loader.go:123 +0x50

Previous read at 0x... by goroutine ...
    chan_task.go:86 +0x40
==================
```

说明发现了数据竞争，需要检查相关代码。

### 案例 3：性能基准对比

1. 运行 **Benchmark - All comparisons**
2. 输出类似：
   ```
   BenchmarkActorLoaderCrossGoroutineReturnValue-28     463887    2615 ns/op    882 B/op   13 allocs/op
   BenchmarkActorLoaderCrossGoroutineUncached-28        210468    5972 ns/op   1007 B/op   15 allocs/op
   BenchmarkActorLoaderDirectInvokeBaseline-28         4280808     294 ns/op    120 B/op    5 allocs/op
   ```
3. 观察：
   - 缓存 GID vs 未缓存：**2.3× 加速**
   - 缓存 GID vs 直接调用：**~9.0× 较慢** ✓（预期，跨协程的必然开销）

---

## 常见问题

### Q1. 启动调试后显示 "dap: not implemented"

**原因**：未安装 Go 扩展或 dlv 工具

**解决**：
```bash
# 1. VS Code 商店搜索"Go"，安装 golang.go 扩展
# 2. 或在终端运行：
go install github.com/go-delve/delve/cmd/dlv@latest
```

### Q2. 怎样监视一个变量的变化？

**方法**：
1. 在 Watch 面板（左侧 Run and Debug 下面）
2. 点击 "+" 按钮，输入变量名或表达式
3. 例如：`atomic.LoadInt64(&st.success)` 可观察原子计数器的值

### Q3. 能否在循环中自动设置条件断点？

**方法**：
1. 右键点击断点 → **Edit Breakpoint**
2. 输入条件，例如：`i > 100` 或 `err != nil`
3. 仅当条件成立时停止执行

### Q4. 怎样跳过某个 goroutine 的执行？

**方法**（需要 dlv 0.12+）：
1. 调试时开启多 goroutine 视图
2. （VS Code 的 Go 扩展可能不支持，需要用 CLI dlv）

### Q5. 压力测试卡住了怎么办？

**处理**：
1. 按 `Ctrl+C` 停止（集成终端中）
2. 查看 final 统计，判断是否到达预期并发量
3. 增加超时：修改 `defaultTaskTimeout` 再次运行

---

## 最佳实践

✅ **推荐做法：**
1. 每次修改代码后，先运行 **Run tests with race detector**
2. 性能优化前后用 **Benchmark** 对比
3. 压力测试定期在 stress 环境中验证（ACTOR_STRESS=1）
4. 在调试模式下设置 5-10 个关键断点，加速问题定位

❌ **避免：**
1. 忘记关闭调试时的 goroutine —— 它们会在后台占用内存
2. 在 tight loop 中设置断点 —— 会导致程序停顿
3. 调试时不使用 -race 标志 —— 无法检测竞争

---

## 反馈

若遇到调试相关问题，请检查：
- Go 扩展是否最新：`Help → About`
- dlv 版本：`dlv version` 命令
- VS Code 版本：`Code --version`
