// Package service 是 actor 的编排层：谁该有一个 actor、什么时候创建、什么时候回收。
//
// 它是本项目里唯一同时认识 actor 框架和业务模块的地方。路由层只跟 Hub 打交道，
// 拿到一个 *actor.ActorLoader 就去 ModInvoke；存储层则完全不知道 actor 的存在。
package service

import (
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"noteserver/src/comm"
	"noteserver/src/contract"
	"noteserver/src/mods"

	"actor"
)

// Deps 是 Hub 需要的全部存储依赖。
//
// 用结构体而不是三个参数，是为了让测试替换其中一个时不必改所有调用点。
type Deps struct {
	Accounts contract.IAccountStore
	Notes    contract.INoteStore
	Sessions contract.ISessionStore
}

// userActor 是一个用户的 actor 及其生命周期状态。
type userActor struct {
	loader *actor.ActorLoader
	wg     sync.WaitGroup

	// inFlight 是正在这个 actor 上执行的请求数。回收只在它为 0 时发生，
	// 否则会关掉一个正在服务的 actor，把请求打成 500。
	inFlight atomic.Int64
	lastUsed atomic.Int64 // UnixNano
}

// Hub 管理所有 actor 的创建与回收。
type Hub struct {
	deps Deps

	auth   [comm.AuthShards]*actor.ActorLoader
	authWG sync.WaitGroup

	mu    sync.Mutex
	users map[string]*userActor

	stopJanitor chan struct{}
	janitorDone chan struct{}
	closeOnce   sync.Once

	// discarded 累计所有 loader 上被丢弃的错误数。
	//
	// 计数放在 Hub 而不是逐个问 loader：用户 actor 被回收时连同它的计数一起
	// 消失，逐个求和会把回收前发生的失败算漏——而那恰恰是最该被看见的一类。
	discarded atomic.Uint64
}

// NewHub 建 Hub 并把 auth 分片全部拉起来。
func NewHub(deps Deps) *Hub {
	h := &Hub{
		deps:        deps,
		users:       make(map[string]*userActor),
		stopJanitor: make(chan struct{}),
		janitorDone: make(chan struct{}),
	}

	for i := range h.auth {
		l := actor.NewActorLoader(fmt.Sprintf("auth-%d", i))
		l.Init()
		l.AddModule(mods.NewAuthMod(deps.Accounts))
		// 接管丢弃上报：无返回值调用的失败没有调用方能接，
		// 不接管的话框架只会往 stderr 打限流日志。这里全部计数并落日志。
		l.SetDiscardedErrorHandler(h.onDiscarded)
		// Start 返回时 goroutineID 已发布，此后外部调用必然走入队路径，
		// 不会退化成在调用方栈上直接改模块状态。
		l.Start(&h.authWG)
		h.auth[i] = l
	}

	go h.janitor()
	return h
}

// Sessions 暴露会话存储，供鉴权中间件使用。
//
// 会话校验是纯读，没有需要串行化的状态，绕进 actor 只是白搭一次投递，
// 所以它不经过任何 loader。
func (that *Hub) Sessions() contract.ISessionStore { return that.deps.Sessions }

// AuthFor 按 UID 选分片。同一个号码永远得到同一个 actor。
func (that *Hub) AuthFor(uid string) *actor.ActorLoader {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(uid))
	return that.auth[sum.Sum32()%comm.AuthShards]
}

// AcquireUser 取（必要时创建）某个用户的 actor，并把它标记为使用中。
// 用完必须调 ReleaseUser，否则这个 actor 永远不会被回收。
func (that *Hub) AcquireUser(uid string) *actor.ActorLoader {
	that.mu.Lock()
	ua := that.users[uid]
	if ua == nil {
		ua = &userActor{}
		l := actor.NewActorLoader("user-" + uid)
		l.Init()
		l.AddModule(mods.NewNoteMod(that.deps.Notes, uid))
		l.SetDiscardedErrorHandler(that.onDiscarded)
		l.Start(&ua.wg)
		ua.loader = l
		that.users[uid] = ua
	}
	// 在锁内自增：巡检协程也持这把锁做"inFlight 是否为 0"的判断，
	// 这样"取到 actor"和"决定回收它"就不可能同时发生。
	ua.inFlight.Add(1)
	ua.lastUsed.Store(time.Now().UnixNano())
	that.mu.Unlock()
	return ua.loader
}

// ReleaseUser 归还一次 AcquireUser。
func (that *Hub) ReleaseUser(uid string) {
	that.mu.Lock()
	if ua := that.users[uid]; ua != nil {
		ua.inFlight.Add(-1)
		ua.lastUsed.Store(time.Now().UnixNano())
	}
	that.mu.Unlock()
}

// OnlineUsers 返回当前存活的用户 actor 数，用于观测。
func (that *Hub) OnlineUsers() int {
	that.mu.Lock()
	defer that.mu.Unlock()
	return len(that.users)
}

func (that *Hub) janitor() {
	defer close(that.janitorDone)
	t := time.NewTicker(comm.JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-that.stopJanitor:
			return
		case <-t.C:
			that.EvictIdle(time.Now())
		}
	}
}

// EvictIdle 回收空闲超时且没有在途请求的用户 actor，返回回收数量。
// 导出是为了让测试不必等 30 秒的巡检周期。
func (that *Hub) EvictIdle(now time.Time) int {
	// 两段式：先在锁内挑出该回收的并从表里摘掉，再到锁外关闭。
	// 关闭要排空队列并等事件循环退出，持着锁做会把所有请求堵住。
	// 摘出表之后没人能再 Acquire 到它，而 inFlight 已经是 0，所以是安全的。
	that.mu.Lock()
	var doomed []*userActor
	for id, ua := range that.users {
		if ua.inFlight.Load() != 0 {
			continue
		}
		if now.Sub(time.Unix(0, ua.lastUsed.Load())) < comm.UserIdleTimeout {
			continue
		}
		delete(that.users, id)
		doomed = append(doomed, ua)
	}
	that.mu.Unlock()

	for _, ua := range doomed {
		ua.loader.Close()
		ua.wg.Wait()
	}
	return len(doomed)
}

// Close 停掉所有 actor。等全部事件循环退出后才返回。
func (that *Hub) Close() {
	that.closeOnce.Do(func() {
		close(that.stopJanitor)
		<-that.janitorDone

		that.mu.Lock()
		all := make([]*userActor, 0, len(that.users))
		for id, ua := range that.users {
			all = append(all, ua)
			delete(that.users, id)
		}
		that.mu.Unlock()

		for _, ua := range all {
			ua.loader.Close()
			ua.wg.Wait()
		}
		for _, l := range that.auth {
			l.Close()
		}
		that.authWG.Wait()
	})
}

// DiscardedErrors 返回累计的丢弃错误数，用于观测。
//
// 正常运行时它应当一直是 0：非 0 意味着有无返回值的调用失败了，
// 而那类失败没有调用方能接，只会在这里留下痕迹。
func (that *Hub) DiscardedErrors() uint64 { return that.discarded.Load() }

// onDiscarded 是丢弃错误的处理器。
//
// 它跑在**出事的那条 goroutine 上**——投递失败在调用方协程，调用失败在
// actor 自己的事件循环上。所以必须廉价，而且绝不能回头再调这个 actor。
func (that *Hub) onDiscarded(e actor.DiscardedError) {
	that.discarded.Add(1)
	log.Printf("[discarded] %v", e)
}
