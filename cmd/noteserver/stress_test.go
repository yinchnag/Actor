package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"noteserver/src/comm"
	"noteserver/src/databases"
	"noteserver/src/middleware"
	"noteserver/src/router/auth"
	"noteserver/src/router/health"
	"noteserver/src/router/note"
	"noteserver/src/service"

	"github.com/gin-gonic/gin"
	"github.com/norm/orm"
)

// 压测默认不跑，用环境变量打开：
//
//	NOTE_STRESS=1 go test -run TestStress -v -count=1              # 内存存储
//	NOTE_STRESS=1 NOTE_STRESS_REAL=1 go test -run TestStress -v    # 真实 MySQL+Redis
//
// 两种模式压的是不同的东西。内存模式把存储的耗时压到接近 0，剩下的全是
// actor 编排、HTTP 与反射调用的开销——它测的是框架本身能扛多少。
// 真实模式加进 Norm 的 Redis 同步写与 MySQL 异步刷盘，测的是这套编排在
// 真实延迟下会不会塌（比如模块方法把事件循环占住导致排队超时）。

const (
	envStress = "NOTE_STRESS"
	envReal   = "NOTE_STRESS_REAL"
)

func stressEnabled(t *testing.T) bool {
	t.Helper()
	if os.Getenv(envStress) != "1" {
		t.Skipf("跳过压测；用 %s=1 打开", envStress)
		return false
	}
	return true
}

func useRealStorage() bool { return os.Getenv(envReal) == "1" }

// --- 真实存储：整个测试进程只初始化一次 ---
//
// orm.InitPool 与 orm.Shutdown 操作的是包级单例连接池，不是每个 harness 一份。
// 用 t.Cleanup(orm.Shutdown) 的后果是第一个用例结束就把存储关掉，
// 后面所有写入都变成 "store stopped"——关服才该发生的事提前发生了。
// 所以初始化收敛到 sync.Once，关闭推迟到 TestMain 里 m.Run 返回之后。
var (
	realOnce  sync.Once
	realDeps  service.Deps
	realErr   error
	realReady atomic.Bool

	// archiveErrs 累计异步存档失败。
	//
	// 回调是注册在 orm 包上的全局钩子，绝不能捕获某个 *testing.T——
	// 它会在别的用例正在跑（甚至全部跑完）时被调用，那时候往旧的 t 上写
	// 会直接 panic："Log in goroutine after test has completed"。
	archiveErrs atomic.Int64
)

func realStorage() (service.Deps, error) {
	realOnce.Do(func() {
		if err := orm.InitPool("data/orm.json"); err != nil {
			realErr = err
			return
		}
		orm.SetArchiveErrorHandler(func(ev orm.ArchiveError) {
			archiveErrs.Add(1)
			log.Printf("[存档失败] dropped=%v %s", ev.Dropped, ev.Error())
		})
		realDeps = service.Deps{
			Accounts: databases.NewAccountStore(),
			Notes:    databases.NewNoteStore(),
			Mails:    databases.NewMailStore(),
			Sessions: databases.NewSessionStore(),
		}
		realReady.Store(true)
	})
	return realDeps, realErr
}

// shutdownRealStorage 由 TestMain 在所有用例跑完后调用。
func shutdownRealStorage() {
	if realReady.Load() {
		orm.Shutdown() // 把异步队列里没落盘的刷进 MySQL
	}
}

// checkArchive 断言本用例期间没有新的存档失败，返回本次的基线供下一段使用。
func checkArchive(t *testing.T, before int64) int64 {
	t.Helper()
	now := archiveErrs.Load()
	if now != before {
		t.Errorf("期间出现 %d 次异步存档失败——笔记只写进了 Redis，MySQL 里没有", now-before)
	}
	return now
}

// --- 压测脚手架 ---

type stressHarness struct {
	t     *testing.T
	ts    *httptest.Server
	hub   *service.Hub
	notes *memNotes // 内存模式下非 nil，用来核对存储里的真实条数
	real  bool

	closeOnce sync.Once
}

// close 停掉 HTTP 与所有 actor，可重复调用。
//
// 单独暴露而不是只挂在 t.Cleanup 上，是因为 Cleanup 要等测试函数**返回之后**
// 才跑——想在测试内部观察"关完之后协程有没有落回去"，就必须能手动关一次。
func (that *stressHarness) close() {
	that.closeOnce.Do(func() {
		that.ts.Close() // 先停 HTTP，让在途请求释放手上的 actor
		that.hub.Close()
		// 客户端空闲连接池里还攥着一批 keep-alive 连接，每条对应一对读写协程。
		// 不主动关的话它们要等自己超时，会被误判成没退出的事件循环。
		that.ts.Client().CloseIdleConnections()
	})
}

// newStressHarness 装出与 main 相同的路由。
//
// 真实模式下 orm.InitPool 只能调一次（连接池是包级单例），所以整个测试
// 进程里只允许有一个真实模式的 harness——压测本来也只跑一轮。
func newStressHarness(t *testing.T) *stressHarness {
	t.Helper()

	var deps service.Deps
	var mem *memNotes
	real := useRealStorage()

	if real {
		d, err := realStorage()
		if err != nil {
			t.Fatalf("连接存储失败（真实模式需要本机 MySQL+Redis）: %v", err)
		}
		deps = d
	} else {
		mem = newMemNotes()
		deps = service.Deps{
			Accounts: newMemAccounts(),
			Notes:    mem,
			Mails:    newMemMails(),
			Sessions: newMemSessions(),
		}
	}

	hub := service.NewHub(deps)
	engine := gin.New() // 不挂 Logger：压测下日志本身就是瓶颈
	engine.Use(gin.Recovery())
	v1 := engine.Group("/api")
	health.New(engine, hub)
	auth.New(v1, hub)
	// 与 main 一样：路径来自请求类型上的 path tag，分组只负责套鉴权
	note.New(v1.Group("", middleware.Auth(hub.Sessions())), hub)

	ts := httptest.NewServer(engine)
	// 默认的 MaxIdleConnsPerHost 是 2，压测时会逼客户端不停新建连接，
	// 测出来的其实是 TCP 握手而不是服务本身。
	ts.Client().Transport = &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		MaxConnsPerHost:     512,
	}

	h := &stressHarness{t: t, ts: ts, hub: hub, notes: mem, real: real}
	t.Cleanup(h.close)
	return h
}

// --- 统计 ---

// stat 收集一组耗时并给出分位数。
//
// 只看平均值会漏掉最重要的信息：actor 模型的典型失败形态是"绝大多数请求
// 很快，少数被事件循环排队卡住"，那在均值上几乎看不出来，p99 才会跳。
type stat struct {
	mu   sync.Mutex
	d    []time.Duration
	fail atomic.Int64
	code sync.Map // http 状态码 -> *atomic.Int64
}

func newStat(capacity int) *stat { return &stat{d: make([]time.Duration, 0, capacity)} }

func (that *stat) add(d time.Duration, code int) {
	that.mu.Lock()
	that.d = append(that.d, d)
	that.mu.Unlock()

	v, _ := that.code.LoadOrStore(code, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func (that *stat) pct(p float64) time.Duration {
	that.mu.Lock()
	defer that.mu.Unlock()
	if len(that.d) == 0 {
		return 0
	}
	i := int(float64(len(that.d)-1) * p)
	return that.d[i]
}

func (that *stat) sortOnce() {
	that.mu.Lock()
	defer that.mu.Unlock()
	sort.Slice(that.d, func(i, j int) bool { return that.d[i] < that.d[j] })
}

func (that *stat) count() int {
	that.mu.Lock()
	defer that.mu.Unlock()
	return len(that.d)
}

func (that *stat) codes() string {
	parts := make([]string, 0, 4)
	that.code.Range(func(k, v any) bool {
		parts = append(parts, fmt.Sprintf("%d×%d", k.(int), v.(*atomic.Int64).Load()))
		return true
	})
	sort.Strings(parts)
	return fmt.Sprint(parts)
}

func (that *stat) report(t *testing.T, name string, wall time.Duration) {
	t.Helper()
	that.sortOnce()
	n := that.count()
	if n == 0 {
		t.Logf("%-10s 无样本", name)
		return
	}
	qps := float64(n) / wall.Seconds()
	t.Logf("%-10s n=%-6d %7.0f req/s   p50=%-9v p95=%-9v p99=%-9v max=%-9v  %s",
		name, n, qps, that.pct(0.50).Round(time.Microsecond),
		that.pct(0.95).Round(time.Microsecond), that.pct(0.99).Round(time.Microsecond),
		that.pct(1.0).Round(time.Microsecond), that.codes())
}

// --- 请求辅助 ---

// phoneGen 按运行随机一个号段前缀，避免真实模式下与上一轮跑剩的账号撞车。
type phoneGen struct {
	base int64
}

func newPhoneGen() *phoneGen {
	// 13xxxxxxxxx：第二位固定 3，后 9 位由随机基址加序号推出
	return &phoneGen{base: rand.Int63n(900_000_000)}
}

func (that *phoneGen) at(i int) string {
	return "13" + fmt.Sprintf("%09d", (that.base+int64(i))%1_000_000_000)
}

func (that *stressHarness) post(path, token string, body string) (int, time.Duration) {
	req, err := http.NewRequest(http.MethodPost, that.ts.URL+path, newStringReader(body))
	if err != nil {
		that.t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return that.send(req)
}

func (that *stressHarness) get(path, token string) (int, time.Duration) {
	req, err := http.NewRequest(http.MethodGet, that.ts.URL+path, nil)
	if err != nil {
		that.t.Fatalf("构造请求失败: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return that.send(req)
}

func (that *stressHarness) send(req *http.Request) (int, time.Duration) {
	start := time.Now()
	resp, err := that.ts.Client().Do(req)
	if err != nil {
		return 0, time.Since(start)
	}
	// 必须读干净再关，否则连接无法复用，压测就退化成每次新建 TCP
	drain(resp)
	return resp.StatusCode, time.Since(start)
}

// tokenOf 注册并登录，返回 token。失败直接 Fatal——准备阶段出错没必要继续压。
func (that *stressHarness) tokenOf(phone, pw string) string {
	that.t.Helper()
	body := `{"phone":"` + phone + `","password":"` + pw + `"}`
	if code, _ := that.post("/api/register", "", body); code != http.StatusCreated {
		that.t.Fatalf("注册 %s 失败: code=%d", phone, code)
	}
	req, _ := http.NewRequest(http.MethodPost, that.ts.URL+"/api/login", newStringReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := that.ts.Client().Do(req)
	if err != nil {
		that.t.Fatalf("登录 %s 失败: %v", phone, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		that.t.Fatalf("登录 %s 失败: code=%d", phone, resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	decodeJSONBody(resp, &out)
	if out.Token == "" {
		that.t.Fatalf("登录 %s 没拿到 token", phone)
	}
	return out.Token
}

// --- 压测用例 ---

// TestStressRegisterStorm 注册风暴：大量不同号码同时注册。
//
// 压的是 auth 分片。所有注册请求被 fnv 哈希打散到 comm.AuthShards 条事件
// 循环上，每条都是单消费者——这个用例回答的是"4 条事件循环够不够扛注册"。
func TestStressRegisterStorm(t *testing.T) {
	if !stressEnabled(t) {
		return
	}
	h := newStressHarness(t)
	gen := newPhoneGen()

	const (
		total       = 2000
		concurrency = 64
	)
	st := newStat(total)

	before := runtime.NumGoroutine()
	archiveBase := archiveErrs.Load()
	start := time.Now()
	runConcurrent(concurrency, total, func(i int) {
		body := `{"phone":"` + gen.at(i) + `","password":"stress-password"}`
		code, d := h.post("/api/register", "", body)
		st.add(d, code)
		if code != http.StatusCreated {
			st.fail.Add(1)
		}
	})
	wall := time.Since(start)

	st.report(t, "register", wall)
	t.Logf("auth 分片数=%d  协程 %d→%d", comm.AuthShards, before, runtime.NumGoroutine())

	if n := st.fail.Load(); n != 0 {
		t.Errorf("%d 次注册失败，状态码分布 %s", n, st.codes())
	}
	if n := h.hub.DiscardedErrors(); n != 0 {
		t.Errorf("出现了 %d 次被丢弃的调用", n)
	}
	checkArchive(t, archiveBase)
}

// TestStressSamePhoneRegister 同一号码的注册风暴：只能有一个成功。
//
// 这是分片串行化的正确性底线，也是换成 Norm 之后唯一的一道防线——
// 它的异步 Save 走 ON DUPLICATE KEY UPDATE，唯一冲突既不报错也没有返回值，
// 数据库那层已经兜不住重复注册了。
func TestStressSamePhoneRegister(t *testing.T) {
	if !stressEnabled(t) {
		return
	}
	h := newStressHarness(t)
	gen := newPhoneGen()
	phone := gen.at(0)

	const attempts = 512
	var created, conflict, other atomic.Int64
	st := newStat(attempts)

	start := time.Now()
	runConcurrent(128, attempts, func(int) {
		body := `{"phone":"` + phone + `","password":"stress-password"}`
		code, d := h.post("/api/register", "", body)
		st.add(d, code)
		switch code {
		case http.StatusCreated:
			created.Add(1)
		case http.StatusConflict:
			conflict.Add(1)
		default:
			other.Add(1)
		}
	})
	st.report(t, "同号注册", time.Since(start))

	t.Logf("%d 次并发注册同一号码: 成功=%d 冲突=%d 其它=%d",
		attempts, created.Load(), conflict.Load(), other.Load())
	if created.Load() != 1 || other.Load() != 0 {
		t.Errorf("应当恰好 1 次成功、0 次其它错误")
	}
}

// TestStressUploadThroughput 多用户并发上传 + 读取。
//
// 压的是"每用户一个 actor"这条路径：写落在各自的事件循环上互不干扰，
// 而缓存与存储的一致性完全靠事件循环的串行性保证，业务代码里一把锁都没有。
// 结束后逐个用户核对条数，任何一条丢失或重复都会被抓到。
func TestStressUploadThroughput(t *testing.T) {
	if !stressEnabled(t) {
		return
	}
	h := newStressHarness(t)
	gen := newPhoneGen()

	users := 100
	perUser := 20
	if h.real {
		// 真实模式每次上传要同步写一次 Redis，量放小些，跑完在一分钟内
		users, perUser = 40, 10
	}
	total := users * perUser

	tokens := make([]string, users)
	uids := make([]string, users)
	for i := range tokens {
		uids[i] = gen.at(i)
		tokens[i] = h.tokenOf(uids[i], "stress-password")
	}
	t.Logf("准备完毕: %d 个账号，每人将上传 %d 条", users, perUser)

	// 先各读一次预热缓存，让后续上传走"改缓存 + 写存储"两条都要对齐的路径
	runConcurrent(64, users, func(i int) { h.get("/api/notes", tokens[i]) })

	up := newStat(total)
	before := runtime.NumGoroutine()
	archiveBase := archiveErrs.Load()
	start := time.Now()
	runConcurrent(64, total, func(i int) {
		u := i % users
		body := `{"content":"压测笔记 ` + strconv.Itoa(i) + ` 内容里带点中文让它不至于太短"}`
		code, d := h.post("/api/notes", tokens[u], body)
		up.add(d, code)
		if code != http.StatusCreated {
			up.fail.Add(1)
		}
	})
	wall := time.Since(start)
	up.report(t, "upload", wall)

	if n := up.fail.Load(); n != 0 {
		t.Errorf("%d 次上传失败，状态码分布 %s", n, up.codes())
	}

	// 读一轮：此时每个用户的 actor 都已存活且缓存已满
	rd := newStat(users * 2)
	start = time.Now()
	runConcurrent(64, users*2, func(i int) {
		code, d := h.get("/api/notes", tokens[i%users])
		rd.add(d, code)
		if code != http.StatusOK {
			rd.fail.Add(1)
		}
	})
	rd.report(t, "list", time.Since(start))

	t.Logf("在线用户 actor=%d  协程 %d→%d  丢弃=%d",
		h.hub.OnlineUsers(), before, runtime.NumGoroutine(), h.hub.DiscardedErrors())

	if h.hub.OnlineUsers() != users {
		t.Errorf("在线 actor 数应当是 %d, got %d", users, h.hub.OnlineUsers())
	}
	if n := h.hub.DiscardedErrors(); n != 0 {
		t.Errorf("出现了 %d 次被丢弃的调用", n)
	}
	checkArchive(t, archiveBase)

	// 内存模式下可以直接核对存储：一条不多、一条不少
	if h.notes != nil {
		for i, uid := range uids {
			if n := h.notes.count(uid); n != perUser {
				t.Fatalf("用户 %d(%s) 存储里有 %d 条，期望 %d 条", i, uid, n, perUser)
			}
		}
		t.Logf("存储核对通过: %d 个用户各 %d 条，共 %d 条", users, perUser, total)
	}
}

// TestStressEvictionUnderLoad 边打流量边回收空闲 actor。
//
// 这是最容易出事的一条路径：回收要 Close 掉 actor 并等事件循环退出，
// 而在途请求正握着同一个 loader。inFlight 计数如果没守住，
// 请求就会撞上"actor is closed"——表现为 5xx，而不是崩溃，很容易被忽略。
func TestStressEvictionUnderLoad(t *testing.T) {
	if !stressEnabled(t) {
		return
	}
	h := newStressHarness(t)
	gen := newPhoneGen()

	const users = 40
	tokens := make([]string, users)
	for i := range tokens {
		tokens[i] = h.tokenOf(gen.at(i), "stress-password")
	}

	var stop atomic.Bool
	var evicted atomic.Int64
	var janitor sync.WaitGroup
	janitor.Add(1)
	go func() {
		defer janitor.Done()
		// 把"当前时间"推到未来，让每一轮巡检都认为所有 actor 都空闲超时了。
		// 只有 inFlight 不为 0 这一条能拦住回收——正是要压的那条。
		for !stop.Load() {
			evicted.Add(int64(h.hub.EvictIdle(time.Now().Add(24 * time.Hour))))
			time.Sleep(time.Millisecond)
		}
	}()

	const total = 2000
	st := newStat(total)
	archiveBase := archiveErrs.Load()
	start := time.Now()
	runConcurrent(64, total, func(i int) {
		u := i % users
		var code int
		var d time.Duration
		if i%3 == 0 {
			code, d = h.post("/api/notes", tokens[u], `{"content":"回收压测"}`)
			if code != http.StatusCreated {
				st.fail.Add(1)
			}
		} else {
			code, d = h.get("/api/notes", tokens[u])
			if code != http.StatusOK {
				st.fail.Add(1)
			}
		}
		st.add(d, code)
	})
	wall := time.Since(start)

	stop.Store(true)
	janitor.Wait()

	st.report(t, "混合", wall)
	t.Logf("巡检期间回收了 %d 个 actor，当前在线 %d，丢弃 %d",
		evicted.Load(), h.hub.OnlineUsers(), h.hub.DiscardedErrors())

	if evicted.Load() == 0 {
		t.Error("整轮下来一个 actor 都没回收，这个用例没压到该压的东西")
	}
	if n := st.fail.Load(); n != 0 {
		t.Errorf("回收与请求并发时出现 %d 次失败，状态码分布 %s——"+
			"多半是 inFlight 没守住，actor 在服务中被关掉了", n, st.codes())
	}
	checkArchive(t, archiveBase)
}

// TestStressGoroutineSettles 压完之后协程数要能落回去。
//
// 每个在线用户一条事件循环，不回收的话协程数就随登录过的用户数单调增长。
// 这个用例验证 Close 之后所有事件循环都真的退出了。
func TestStressGoroutineSettles(t *testing.T) {
	if !stressEnabled(t) {
		return
	}
	base := runtime.NumGoroutine()

	func() {
		h := newStressHarness(t)
		gen := newPhoneGen()
		const users = 60
		tokens := make([]string, users)
		for i := range tokens {
			tokens[i] = h.tokenOf(gen.at(i), "stress-password")
		}
		runConcurrent(32, users*5, func(i int) {
			h.post("/api/notes", tokens[i%users], `{"content":"协程压测"}`)
		})
		t.Logf("峰值: 在线 actor=%d 协程=%d", h.hub.OnlineUsers(), runtime.NumGoroutine())

		// 全部回收掉，等事件循环退出
		if n := h.hub.EvictIdle(time.Now().Add(24 * time.Hour)); n != users {
			t.Errorf("应当回收 %d 个 actor, got %d", users, n)
		}
		if h.hub.OnlineUsers() != 0 {
			t.Errorf("回收后不该还有在线 actor, got %d", h.hub.OnlineUsers())
		}
		// 手动关掉：t.Cleanup 要等本测试函数返回才跑，那时已经采样完了
		h.close()
	}()

	// Close 是同步等事件循环退出的，但 HTTP 侧的连接协程是异步收敛的，
	// 给它一点时间再采样，否则测到的是残留的 keep-alive 协程而不是事件循环。
	deadline := time.Now().Add(5 * time.Second)
	var now int
	for time.Now().Before(deadline) {
		now = runtime.NumGoroutine()
		if now <= base+10 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("协程数 基线=%d 结束=%d", base, now)
	if now > base+10 {
		t.Errorf("压测结束后仍有 %d 条协程（基线 %d），可能有事件循环没退出", now, base)
	}
}

// --- 小工具 ---

// runConcurrent 用 concurrency 条协程跑完 total 次 fn(i)。
//
// 用固定协程数从计数器领任务，而不是给每次调用开一条协程：
// 后者在 total 很大时测的其实是调度器，不是被压的服务。
func runConcurrent(concurrency, total int, fn func(i int)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for c := 0; c < concurrency; c++ {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= total {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// newStringReader 把请求体包成 Reader。
//
// 不用 bytes.NewBufferString：http.NewRequest 只对 *bytes.Reader、
// *strings.Reader 等已知类型自动填 ContentLength，换成别的类型会退化成
// chunked 编码，压测里那是额外的一层开销。
func newStringReader(s string) *strings.Reader { return strings.NewReader(s) }

// drain 读干净响应体再关闭。
//
// 不读干净就 Close，连接不会回到空闲池，压测会退化成每次请求都新建 TCP——
// 测出来的是握手耗时，不是服务本身。
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func decodeJSONBody(resp *http.Response, dst any) {
	_ = json.NewDecoder(resp.Body).Decode(dst)
}
