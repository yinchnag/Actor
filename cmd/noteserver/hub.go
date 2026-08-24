package main

import (
	"fmt"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	"actor"
)

const (
	// authShards 是 auth actor 的分片数。分片的目的不是提高吞吐，
	// 而是让"同一个手机号的注册请求落到同一个 actor"从而天然串行；
	// 分成多片则是为了别让所有注册/登录挤在一条事件循环上。
	authShards = 4

	// userIdleTimeout 用户 actor 空闲多久后回收。
	// 在线用户数决定 goroutine 数，不回收的话每个登录过的用户都会留一条协程。
	userIdleTimeout = 5 * time.Minute
	// janitorInterval 空闲回收的巡检间隔。
	janitorInterval = 30 * time.Second
)

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
	store Store

	auth   [authShards]*actor.ActorLoader
	authWG sync.WaitGroup

	mu    sync.Mutex
	users map[int64]*userActor

	stopJanitor chan struct{}
	janitorDone chan struct{}
	closeOnce   sync.Once
}

func NewHub(store Store) *Hub {
	h := &Hub{
		store:       store,
		users:       make(map[int64]*userActor),
		stopJanitor: make(chan struct{}),
		janitorDone: make(chan struct{}),
	}

	for i := range h.auth {
		l := actor.NewActorLoader(fmt.Sprintf("auth-%d", i))
		l.Init()
		l.AddModule(NewAuthMod(store))
		// 接管丢弃上报：无返回值调用的失败没有调用方能接，
		// 不接管的话框架只会往 stderr 打限流日志。这里全部计数并落日志。
		l.SetDiscardedErrorHandler(logDiscarded)
		// Start 返回时 goroutineID 已发布，此后外部调用必然走入队路径，
		// 不会退化成在调用方栈上直接改模块状态。
		l.Start(&h.authWG)
		h.auth[i] = l
	}

	go h.janitor()
	return h
}

// AuthFor 按手机号选分片。同一个号码永远得到同一个 actor。
func (h *Hub) AuthFor(phone string) *actor.ActorLoader {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(phone))
	return h.auth[sum.Sum32()%authShards]
}

// AcquireUser 取（必要时创建）某个用户的 actor，并把它标记为使用中。
// 用完必须调 ReleaseUser，否则这个 actor 永远不会被回收。
func (h *Hub) AcquireUser(userID int64) *actor.ActorLoader {
	h.mu.Lock()
	ua := h.users[userID]
	if ua == nil {
		ua = &userActor{}
		l := actor.NewActorLoader(fmt.Sprintf("user-%d", userID))
		l.Init()
		l.AddModule(NewNoteMod(h.store, userID))
		l.SetDiscardedErrorHandler(logDiscarded)
		l.Start(&ua.wg)
		ua.loader = l
		h.users[userID] = ua
	}
	// 在锁内自增：巡检协程也持这把锁做"inFlight 是否为 0"的判断，
	// 这样"取到 actor"和"决定回收它"就不可能同时发生。
	ua.inFlight.Add(1)
	ua.lastUsed.Store(time.Now().UnixNano())
	h.mu.Unlock()
	return ua.loader
}

func (h *Hub) ReleaseUser(userID int64) {
	h.mu.Lock()
	if ua := h.users[userID]; ua != nil {
		ua.inFlight.Add(-1)
		ua.lastUsed.Store(time.Now().UnixNano())
	}
	h.mu.Unlock()
}

// OnlineUsers 返回当前存活的用户 actor 数，用于观测。
func (h *Hub) OnlineUsers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.users)
}

func (h *Hub) janitor() {
	defer close(h.janitorDone)
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stopJanitor:
			return
		case <-t.C:
			h.evictIdle(time.Now())
		}
	}
}

func (h *Hub) evictIdle(now time.Time) int {
	// 两段式：先在锁内挑出该回收的并从表里摘掉，再到锁外关闭。
	// 关闭要排空队列并等事件循环退出，持着锁做会把所有请求堵住。
	// 摘出表之后没人能再 Acquire 到它，而 inFlight 已经是 0，所以是安全的。
	h.mu.Lock()
	var doomed []*userActor
	for id, ua := range h.users {
		if ua.inFlight.Load() != 0 {
			continue
		}
		if now.Sub(time.Unix(0, ua.lastUsed.Load())) < userIdleTimeout {
			continue
		}
		delete(h.users, id)
		doomed = append(doomed, ua)
	}
	h.mu.Unlock()

	for _, ua := range doomed {
		ua.loader.Close()
		ua.wg.Wait()
	}
	return len(doomed)
}

// Close 停掉所有 actor。等全部事件循环退出后才返回。
func (h *Hub) Close() {
	h.closeOnce.Do(func() {
		close(h.stopJanitor)
		<-h.janitorDone

		h.mu.Lock()
		all := make([]*userActor, 0, len(h.users))
		for id, ua := range h.users {
			all = append(all, ua)
			delete(h.users, id)
		}
		h.mu.Unlock()

		for _, ua := range all {
			ua.loader.Close()
			ua.wg.Wait()
		}
		for _, l := range h.auth {
			l.Close()
		}
		h.authWG.Wait()
	})
}
