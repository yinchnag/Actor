// Package service 是 actor 的编排层：谁该有一个 actor、什么时候创建、什么时候回收。
//
// 它是本项目里唯一同时认识 actor 框架和业务模块的地方。路由层只跟 Hub 打交道，
// 拿到一个 *actor.ActorLoader 就去 ModInvoke；存储层则完全不知道 actor 的存在。
package service

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"noteserver/src/comm"
	"noteserver/src/contract"
	"noteserver/src/logs"
	"noteserver/src/mods/auth"
	"noteserver/src/mods/mail"
	"noteserver/src/mods/note"

	"actor"
)

// Deps 是 Hub 需要的全部存储依赖。
//
// 用结构体而不是三个参数，是为了让测试替换其中一个时不必改所有调用点。
type Deps struct {
	Accounts contract.IAccountStore
	Notes    contract.INoteStore
	Mails    contract.IMailStore
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

	// shards 是所有 **_mgr.go 模块的分片组，按模块类型名索引。
	//
	// 早先这里是一个写死的 auth 数组、外加一个写死的 AuthFor 方法，生成门面的
	// 模板里也直接写着 that.AuthFor(uid)。加第二个分片 Mgr（邮件）时才发现：
	// 那份模板对第二个模块完全不可用，只能整份复制一遍改一个方法名。
	// 改成按模块名索引之后，模板里那行变成 that.ShardFor("XxxMgr", uid)，
	// 对任意分片 Mgr 都成立，加模块不再需要碰模板。
	shards   map[string][]*actor.ActorLoader
	shardsWG sync.WaitGroup

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
	// 依赖漏配会让模块拿到一个 nil 存储，然后在第一次调那个接口时以空指针的
	// 形式炸在请求协程上——离真正的原因隔着十万八千里。启动时就说清楚。
	switch {
	case deps.Accounts == nil:
		panic("service: Deps.Accounts 没有配置")
	case deps.Notes == nil:
		panic("service: Deps.Notes 没有配置")
	case deps.Mails == nil:
		panic("service: Deps.Mails 没有配置")
	case deps.Sessions == nil:
		panic("service: Deps.Sessions 没有配置")
	}

	h := &Hub{
		deps:        deps,
		users:       make(map[string]*userActor),
		shards:      make(map[string][]*actor.ActorLoader),
		stopJanitor: make(chan struct{}),
		janitorDone: make(chan struct{}),
	}

	// 规范第 7 条：**_mgr.go 的模块在启动时加载。
	// 加新的 Mgr 就在这里加一行——名字必须与模块类型名一致，
	// 生成的门面按那个名字来 ShardFor。
	h.startShards("AuthMgr", comm.AuthShards, func() actor.IModule {
		return auth.NewAuthMgr(deps.Accounts)
	})
	// MailMgr 需要把信箱推给在线用户，而那条边的门面挂在 Hub 上——
	// 把 h 自己传进去即可（mods 不能 import service，所以模块那边声明的是
	// 一个只含所需方法的小接口，见 mods/mail 里的 mailPusher）。
	h.startShards("MailMgr", comm.MailShards, func() actor.IModule {
		return mail.NewMailMgr(deps.Mails, h)
	})

	go h.janitor()
	return h
}

// startShards 拉起一组分片 actor，每片挂一个模块实例。
//
// 每片一个**独立的模块实例**，不是同一个实例挂多处：模块里的状态归它自己的
// 事件循环独占，共享实例等于把状态暴露给多条协程，actor 的保证立刻失效。
func (that *Hub) startShards(name string, n int, newMod func() actor.IModule) {
	loaders := make([]*actor.ActorLoader, n)
	for i := range loaders {
		l := actor.NewActorLoader(fmt.Sprintf("%s-%d", strings.ToLower(name), i))
		l.Init()
		l.AddModule(newMod())
		// 接管丢弃上报：无返回值调用的失败没有调用方能接，
		// 不接管的话框架只会往 stderr 打限流日志。这里全部计数并落日志。
		l.SetDiscardedErrorHandler(that.onDiscarded)
		// Start 返回时 goroutineID 已发布，此后外部调用必然走入队路径，
		// 不会退化成在调用方栈上直接改模块状态。
		l.Start(&that.shardsWG)
		loaders[i] = l
	}
	that.shards[name] = loaders
}

// Sessions 暴露会话存储，供鉴权中间件使用。
//
// 会话校验是纯读，没有需要串行化的状态，绕进 actor 只是白搭一次投递，
// 所以它不经过任何 loader。
func (that *Hub) Sessions() contract.ISessionStore { return that.deps.Sessions }

// ShardFor 按业务键选某个 Mgr 的分片。同一个键永远得到同一个 actor。
//
// 生成的门面调的就是它（见 templates/shard_export.tmpl）。模块名写错会 panic
// 而不是静默返回 nil：那种错只会在第一次调用该接口时以空指针的形式炸在
// 请求协程上，不如启动后第一次调用就说清楚。
func (that *Hub) ShardFor(mod, key string) *actor.ActorLoader {
	loaders, ok := that.shards[mod]
	if !ok || len(loaders) == 0 {
		panic("service: 模块 " + mod + " 没有注册分片——请在 NewHub 里加一行 startShards")
	}
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(key))
	return loaders[sum.Sum32()%uint32(len(loaders))]
}

// LoadUser 给某个用户挂上他的 **_mod.go 模块。
//
// 规范第 7 条：Mgr 在启动时加载，Mod 在**用户登入成功之后**加载。
// 登录成功时调它一次，用户的第一个业务请求就不必再付创建 actor 的钱。
//
// 它不占用 inFlight——只是把 actor 建出来，随后照常受空闲回收管辖。
// 于是"登录了但一直不用"的用户会在 comm.UserIdleTimeout 之后被收走，
// 不会因为登录过就永远留一条协程。
func (that *Hub) LoadUser(uid string) {
	that.AcquireUser(uid)
	that.ReleaseUser(uid)
	// 上线握手：让 MailMgr 把这个用户的信箱推到他刚建好的 MailboxMod 上。
	//
	// 由 Hub 发起而不是让 mod 自己去要——那样 mod 就得长出一条对外调用，
	// 而"它永远不调别人、因此永远不会停下来等谁"这个性质比小心写同步可靠得多。
	// 无返回值，登录应答不等它：数据到了客户端下一次拉取就能看到。
	that.MailFetch(actor.CurrentGID(), uid)
}

// TryUser 取某个用户**已经存在**的 actor，不存在就返回 nil。
//
// 与 AcquireUser 的唯一区别是不创建。这条区别很要紧：推送的语义是"他在就给他，
// 不在就算了"——离线用户的数据本来就在存储里，等他下次登录会重新拉。用
// AcquireUser 去推，会给一个刚被回收的用户重新拉起一条协程，把"在线用户数"
// 悄悄变成"登录过的用户数"。
//
// 拿到非 nil 时 inFlight 已经加过了，用完必须 ReleaseUser——语义与 AcquireUser
// 一致，生成的通知门面里就是这么配对的（见 templates/user_notify.tmpl）。
func (that *Hub) TryUser(uid string) *actor.ActorLoader {
	that.mu.Lock()
	defer that.mu.Unlock()
	ua := that.users[uid]
	if ua == nil {
		return nil
	}
	ua.inFlight.Add(1)
	ua.lastUsed.Store(time.Now().UnixNano())
	return ua.loader
}

// AcquireUser 取（必要时创建）某个用户的 actor，并把它标记为使用中。
// 用完必须调 ReleaseUser，否则这个 actor 永远不会被回收。
//
// 业务代码不该直接调它：取用与归还已经收进生成的门面里了
// （见 note_export.go）。它导出只是因为门面和测试要用。
func (that *Hub) AcquireUser(uid string) *actor.ActorLoader {
	that.mu.Lock()
	ua := that.users[uid]
	if ua == nil {
		ua = &userActor{}
		l := actor.NewActorLoader("user-" + uid)
		l.Init()
		l.AddModule(note.NewNoteMod(that.deps.Notes, uid))
		// 用户侧的信箱**只读**视图。它不碰存储、也不调任何人——
		// 数据全靠 MailMgr 推过来，写操作由 rut 直连 mgr。
		l.AddModule(mail.NewMailboxMod(uid))
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
		for _, loaders := range that.shards {
			for _, l := range loaders {
				l.Close()
			}
		}
		that.shardsWG.Wait()
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
	logs.Errorf("[discarded] %v", e)
}
