# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Actor** is a game server actor framework written in Go. It provides a concurrent actor model with module-based architecture for coordinating gameplay logic across multiple goroutines. The framework is designed for:

- Modular game logic organization (modules handle distinct game systems)
- Cross-goroutine communication via channels and task queues
- Reflection-based method invocation with protocol-based message dispatch
- Optional persistence (save/load) hooks for module state

**Key Design**: Each IModLoader instance wraps a goroutine and manages a set of IModule objects. Goroutines communicate by passing ITask objects through channels, allowing synchronous (with results) or asynchronous (fire-and-forget) module method calls.

## Build & Test Commands

### Basic Commands
```go
# Format all Go code
go fmt ./...

# Run static analysis
go vet ./...

# Run all unit tests
go test ./... -v -count=1

# Run tests with data race detection (strongly recommended)
go test -race ./... -count=1

# Run a specific test
go test -run TestActorLoaderModInvoke -v
```

### Benchmarks & Stress Tests
```bash
# Benchmark: cross-goroutine call with cached GID (optimized path)
go test -bench=BenchmarkActorLoaderCrossGoroutineReturnValue -benchmem -count=3

# Benchmark: cross-goroutine call with uncached GID (baseline for comparison)
go test -bench=BenchmarkActorLoaderCrossGoroutineUncached -benchmem -count=3

# Benchmark: all comparisons (cached, uncached, direct)
go test -bench=BenchmarkActorLoader -benchmem -count=3

# Heavy stress test (high concurrency stability check) - requires ACTOR_STRESS=1
ACTOR_STRESS=1 go test -run TestActorLoaderCrossGoroutineHighConcurrencyStability -v -count=1
```

### VS Code Integration
All debug configurations and tasks are pre-configured in .vscode/launch.json and .vscode/tasks.json. Use Ctrl+Shift+D to open the Run and Debug panel or Ctrl+Shift+P to access tasks. Key configurations:
- **Debug deadlock_demo**: Debug the cmd/deadlock_demo program
- **Test - Race detector**: Run tests with -race flag to catch data races

## Architecture & Core Concepts

### Module System

**IModule**: The contract for a module. Implementations should embed ModObj[T] using composition (CRTP pattern):

```go
type MyModule struct {
    ModObj[*MyModule]
    // ... custom fields
}

func NewMyModule() *MyModule {
    m := &MyModule{}
    m.Init()  // Must call Init before use
    return m
}

// Public methods are automatically reflected by ModObj
func (m *MyModule) MyMethod(arg int) int {
    return arg * 2
}

// Protocol handler: single param (registered in protocol registry), no return
func (m *MyModule) OnMyProtocol(msg MyProtocolMessage) {
    // handles message if registered with RegisterProtocol()
}
```

**ModObj[T] Design**:
- Uses unsafe pointer arithmetic to recover the embedding struct (heir) from field offset
- Reflects all public methods (excluding base IModule methods) and stores in invokers map
- Automatically identifies protocol handlers: methods with exactly 1 param whose type is in the protocol registry and 0 return values
- Builds metaMsgHandler map linking protocol IDs to handler method names
- See mod_obj.go for pointer offset magic and reflection logic

### Task & Goroutine System

**ChanTask**: Represents a cross-goroutine method invocation. Key lifecycle:
- Created via NewChanTask(sourceGID, targetGID, modName, methodName, args...)
- Task is sent through the target goroutine's task channel
- When executed, results are filled in via complete(results, err)
- If caller needs results, it blocks on Await() (with optional timeout via WithTimeout())
- Tasks are pooled via sync.Pool for efficiency; always call Release() when done
- Status: 0 (released), 1 (in-flight), 2 (completed)

**ActorLoader (IModLoader)**: Main coordination hub for a goroutine:
- Stores modules by name (via AddModule, GetModule)
- Runs RunUpdateLoop(wg) which:
  - Fires a 1-second ticker to call Update(dt) on all modules
  - Processes incoming ITask objects from taskChan
  - Cleanly exits when stopChan is closed
- ModInvoke(modName, methodName, args...) is the primary call entry point:
  - Automatically detects if caller is on same goroutine (direct invoke) or different goroutine (enqueue task)
  - Caller goroutine ID is obtained via currentGID() or passed via ModInvokeFrom(cachedGID, ...)
  - For hot-path code: cache the GID once with CurrentGID() and use ModInvokeFrom to avoid repeated runtime.Stack calls
  - Returns results immediately on same-goroutine, blocks with 3-second timeout on cross-goroutine if method has return values

### Protocol & Message Routing

**Protocol Registry**: Global maps link message IDs to Go types
- RegisterProtocol(msgID, sampleInstance) caches the type
- MessageIDByType(t reflect.Type) looks up ID by type (used by ModObj to auto-register handlers)

**OnMessageHandler(IProtocol)**: Dispatches incoming protocol messages
- Iterates all modules, checks if each has a handler for the message ID
- Routes to handler via ModInvoke (respects same/cross-goroutine logic)

### Goroutine ID Tracking

- currentGID(): Parses goroutine ID from runtime.Stack() output (expensive, only call once per goroutine lifetime)
- CurrentGID(): Public wrapper; cache its result and pass to ModInvokeFrom for hot-path efficiency
- SetGoroutineID(role string) in ActorLoader: Initializes the loader's goroutine ID and role name
- Task status check: compares source GID with loader's stored goroutine ID to decide direct vs. enqueue

## Key Files & Responsibilities

| File | Purpose |
|------|---------|
| actor_loader.go | ActorLoader impl, goroutine loop, ModInvoke logic, GID tracking |
| mod_obj.go | ModObj[T] impl, reflection binding, method invocation, heir recovery |
| chan_task.go | ChanTask impl, task pool, completion & await logic |
| imodule.go, igoroutine.go, imodloader.go | Core interfaces |
| protocol_registry.go | Protocol ID to type mapping |
| inject.go | Unsafe field injection utility (pointer offset-based) |
| *_test.go | Unit & stress tests demonstrating module creation and cross-goroutine calls |

## Testing Strategy

1. **Unit tests** (actor_loader_test.go, mod_obj_test.go, etc.): Test basic module registration, same-goroutine invoke, cross-goroutine invoke, message dispatch
2. **Race detection**: Always run with -race to catch data race bugs (especially around task completion and module state)
3. **Stress test**: TestActorLoaderCrossGoroutineHighConcurrencyStability validates correctness under high concurrency (requires ACTOR_STRESS=1)
4. **Benchmarks**: Compare cached vs. uncached GID performance; cached path avoids runtime.Stack overhead

## Common Patterns

### Creating a Module
```go
type GameMod struct {
    ModObj[*GameMod]
    score int
}

func NewGameMod() *GameMod {
    m := &GameMod{}
    m.Init()
    return m
}

func (m *GameMod) AddScore(val int) {
    m.score += val
}
```

### Registering & Using
```go
loader := NewActorLoader("my-actor")
loader.Init()
loader.AddModule(NewGameMod())

// Same goroutine: direct call
results, err := loader.ModInvoke("GameMod", "AddScore", 10)

// Cross-goroutine: enqueued + awaited
var wg sync.WaitGroup
wg.Add(1)
go loader.RunUpdateLoop(&wg)
time.Sleep(10 * time.Millisecond)
results, err := loader.ModInvoke("GameMod", "AddScore", 20)
```

### Protocol Handler (Fire & Forget)
```go
type ScoreMsg struct { Points int }

RegisterProtocol(5001, ScoreMsg{})

func (m *GameMod) OnScoreMsg(msg ScoreMsg) {
    m.score += msg.Points
}

msg := NewProtocolMessage(5001, ScoreMsg{Points: 100}, nil)
loader.OnMessageHandler(msg)
```

## Performance Considerations

- **GID caching**: currentGID() parses runtime.Stack and is expensive. For loops calling ModInvoke repeatedly, capture the GID once and use ModInvokeFrom to avoid overhead. Reduces call overhead from ~5,840 ns/op to ~2,750 ns/op (2.1x speedup).

- **Task pooling**: ChanTask uses sync.Pool to reuse allocations. Always call Release() to return tasks to the pool.

- **Task timeout**: Default is 3 seconds (defaultTaskTimeout). Adjust if needed in actor_loader.go.

## Debugging Tips

- Use VS Code breakpoints in actor_loader.go (around RunUpdateLoop, handleTask) and mod_obj.go (around Invoke) to trace task flow
- Watch variables: atomic.LoadInt64(&stat) for atomic counters during stress tests
- Check task channel buffer size (default 64) in NewActorLoader if tasks are being rejected

## Chinese Documentation

The original design requirements are in REAMD.md and 开发文档_基础版.md. These files explain the rationale for the CRTP pattern, protocol routing, and cross-goroutine task semantics.
