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
- **Debug deadlock_demo**: stale — cmd/deadlock_demo no longer exists. The runnable program
  under cmd/ is now `noteserver` (see cmd/noteserver/README.md); .vscode/launch.json still
  points at the removed demo and needs updating if you use those configs.
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
- `Init()` is idempotent — the second call is a no-op. `AddModule` calls it as a safety net,
  so a constructor that forgets it still works. Idempotence is load-bearing, not just an
  optimization: re-adding an already-serving module would otherwise rebuild its method table
  while the event loop reads it (readers do not hold `modulesMu`).
- `AddModule` panics if `GameName()` is empty — that means reflection binding failed and the
  module could never be invoked; failing at startup beats a runtime `module not found`.
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
- Status (see `TaskStatus*` constants in chan_task.go):
  - 0 `TaskStatusIdle` — in the pool, unused
  - 1 `TaskStatusPending` — enqueued, waiting for the event loop to pick it up
  - 2 `TaskStatusDone` — settled (executed, drained, or canceled)
  - 3 `TaskStatusAbandoned` — caller timed out and canceled it; the event loop skips execution
  - 4 `TaskStatusRunning` — claimed by the event loop, module method is executing
- **Timeout means cancel**: `Pending → Running` (`claimForRun`, event loop) and
  `Pending → Abandoned` (`abandon`, caller) are competing CAS operations — exactly one wins.
  If the caller's `Await` times out while the task is still queued, the module method is
  never executed, and the returned error wraps both `ErrTaskAwaitTimeout` and
  `ErrTaskCanceled`. A timeout *without* `ErrTaskCanceled` means the method was already
  running (Go cannot interrupt a running function), so side effects may have happened —
  callers must dedupe before retrying.

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

### Cross-Actor Call Cycles

The event loop is a **single consumer**: while a module method blocks in `Await` waiting on
another actor, that actor's own `taskChan` is not being drained. So a synchronous call cycle
(`A→B→A`, `A→B→C→A`) deadlocks every actor on the cycle until each hop's timeout expires.

`ActorLoader` maintains a **wait graph** to catch this at call time:

- Each loader publishes `waitingFor` — the loop GID of the actor it is currently blocked on
  (0 = not waiting). Only calls that wait for a return value write it; fire-and-forget calls
  never enter the graph.
- `loaderRegistry` (a `sync.Map` keyed by loop GID) lets `ModInvokeFrom` resolve `callerGID`
  back to the calling actor. Loaders register in `SetGoroutineID` and deregister in `closeWith`.
- Before enqueuing a blocking call, the caller publishes its intent and then walks the graph
  from the target via `wouldDeadlock`. If the walk comes back to the caller, the call fails
  immediately with `ErrCallCycle` — no task is created, nothing is enqueued.

Publish-then-check ordering guarantees that two actors calling each other simultaneously
cannot both miss the cycle (they may both reject, which is the safe direction).

**Consequences for callers**:
- `ErrCallCycle` is not retryable — fix the call structure instead (make one hop a
  no-return-value call, or layer actors so calls only go one direction).
- A cycle with any fire-and-forget hop is *not* a deadlock and is deliberately allowed:
  "handle the request, then push a notification back to the caller" keeps working even while
  the caller is blocked on you.
- Calls from ordinary goroutines (network handlers, workers) never enter the graph — blocking
  such a goroutine does not stall any event loop.
- Not covered: a loop blocked on something that is not another actor (a mutex, IO, a channel).
  No wait edge exists, so the cycle is invisible and `defaultTaskTimeout` remains the backstop.
  Same for enqueue blocking on a full `taskChan`.

### Protocol & Message Routing

**Protocol Registry**: Global maps link message IDs to Go types
- RegisterProtocol(msgID, sampleInstance) caches the type
- MessageIDByType(t reflect.Type) looks up ID by type (used by ModObj to auto-register handlers)

**OnMessageHandler(IProtocol)** / **OnMessageHandlerFrom(callerGID, IProtocol)**: Dispatches incoming protocol messages
- Looks the message ID up in `dispatchIndex`, a read-only snapshot built by `AddModule`
  (one atomic load + one map lookup — no locking, no allocation, independent of module count)
- Routes each target via `ModInvokeFrom` (respects same/cross-goroutine logic)
- When several modules claim the same message ID, they are dispatched in module-name order
- **Hot path**: the GID is resolved once per *message*, not once per handler. Long-lived
  receive goroutines (network readers, message pumps) should cache it once and use
  `OnMessageHandlerFrom` — that removes the `runtime.Stack` call entirely:

  ```go
  gid := actor.CurrentGID()
  for msg := range conn.Recv() {
      loader.OnMessageHandlerFrom(gid, msg)
  }
  ```

  Measured on a 2-module actor: `OnMessageHandler` 3529 ns/op / 8 allocs vs
  `OnMessageHandlerFrom` 790 ns/op / 7 allocs — the latter matches a hand-written
  direct `ModInvokeFrom`, i.e. dispatch itself costs essentially nothing.

### Discarded Errors (no-return-value calls)

Protocol handlers and fire-and-forget `ModInvoke` calls have no caller waiting on a result, so
a failure has nowhere to be returned. Both failure points are reported through one channel:

- **`PhaseDeliver`** — never reached the module method: enqueue failed (`ErrTaskQueueTimeout`,
  actor closed), or the task was queued but drained at shutdown without running.
- **`PhaseInvoke`** — reached the method and the call itself failed: argument type mismatch,
  method not found, or a panic. This one used to be completely invisible — for a fire-and-forget
  task the error was stored in `task.Err` and went back to the pool unread, so e.g. dispatching
  a message whose payload type doesn't match the registered protocol type made the message
  vanish with no error and no crash.

```go
loader.SetDiscardedErrorHandler(func(e actor.DiscardedError) {
    metrics.Inc("actor.discarded", e.Phase.String())
    log.Warnf("%v", e)                       // e.ModName / e.MethodName / e.Err
})
n := loader.DiscardedErrors()                // 累计计数，供监控采样
```

- The handler runs **synchronously on whichever goroutine hit the failure** — the caller's
  goroutine for `PhaseDeliver`, the actor's own event loop for `PhaseInvoke`. Keep it cheap;
  never call back into the same actor from it.
- With no handler installed, the framework logs to stderr **rate-limited to one line per second**
  (a full queue would otherwise flood the log). The counter is always exact.
- `DiscardedError` implements `Unwrap`, so `errors.Is(e, ErrTaskQueueTimeout)` works.
- A message ID that no module claims is *not* an error — it is dropped silently by design
  (broadcasting to many actors where only some care is normal).

### Goroutine ID Tracking

- currentGID(): Parses the goroutine ID out of runtime.Stack() output. **~1730 ns/op, 1 alloc**,
  of which ~1550 ns is runtime.Stack itself walking the stack — that part is irreducible.
  The parse reads the digits straight out of the byte buffer (no string conversion, no
  strings.Fields), so only the 64-byte buffer allocates; it escapes because runtime.Stack
  takes a []byte.
- CurrentGID(): Public wrapper. **The optimization is not to make this faster, it is to not
  call it.** Cache it once per long-lived goroutine and pass it to ModInvokeFrom /
  OnMessageHandlerFrom.
- Inside a module method you never need it at all: the method already runs on the actor's own
  goroutine, so `loader.GetGoroutineID()` (one atomic load) is the same value. Measured on
  500-level reentrant recursion: 266 µs/level via ModInvoke vs 421 ns/level via
  ModInvokeFrom(GetGoroutineID()) — the gap widens with stack depth, because runtime.Stack
  walks the whole stack.
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

- **GID caching**: currentGID() parses runtime.Stack and costs ~1730 ns. For any goroutine that
  calls in repeatedly, capture the GID once and use the `*From` variants:
  - `ModInvoke` ~7000 ns/op / 14 allocs → `ModInvokeFrom` ~2560 ns/op / 13 allocs
  - `OnMessageHandler` ~3529 ns/op / 8 allocs → `OnMessageHandlerFrom` ~790 ns/op / 7 allocs
  - inside a module method: `ModInvokeFrom(loader.GetGoroutineID(), ...)` — three orders of
    magnitude cheaper than ModInvoke on deep stacks

- **Protocol dispatch**: `AddModule` builds a `msgID -> handlers` snapshot published via
  atomic.Pointer, so dispatch never touches `modulesMu` and never allocates. Just the lookup
  segment: 2 modules 64 ns / 1 alloc → 2.2 ns / 0 alloc; 30 modules 488 ns / 480 B / 1 alloc →
  4.7 ns / 0 alloc. Dispatch cost is now independent of how many modules an actor carries.

- **Task pooling**: ChanTask uses sync.Pool to reuse allocations. Always call Release() to return tasks to the pool.

- **Task timeout**: Default is 3 seconds (defaultTaskTimeout). Adjust if needed in actor_loader.go.

## Debugging Tips

- Use VS Code breakpoints in actor_loader.go (around RunUpdateLoop, handleTask) and mod_obj.go (around Invoke) to trace task flow
- Watch variables: atomic.LoadInt64(&stat) for atomic counters during stress tests
- Check task channel buffer size (default 64) in NewActorLoader if tasks are being rejected

## Chinese Documentation

REAMDE.md is the framework usage guide and caveats — read it before writing module code;
it covers the reflection registration rules, the error semantics, and the pitfalls that
follow from the event loop being a single consumer (blocking calls, call cycles, GID caching,
discarded-error handling, state not leaking out of a module).
开发文档_基础版.md holds the original design requirements and explains the rationale for the
CRTP pattern, protocol routing, and cross-goroutine task semantics.
cmd/noteserver/ is a worked example (HTTP service on real MySQL + Redis) with its own README.
