package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"noteserver/src/comm"
	"noteserver/src/middleware"
	"noteserver/src/router/auth"
	"noteserver/src/router/health"
	"noteserver/src/router/mail"
	"noteserver/src/router/note"
	"noteserver/src/service"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	// 关掉 gin 的调试输出，否则每装一次路由都刷一屏
	gin.SetMode(gin.TestMode)
	code := m.Run()
	// 真实模式的压测用的是包级连接池，只能在所有用例跑完之后关一次，
	// 不能挂在某个用例的 Cleanup 上——见 stress_test.go 里 realStorage 的注释。
	shutdownRealStorage()
	os.Exit(code)
}

// --- 测试脚手架 ---

type harness struct {
	t     *testing.T
	ts    *httptest.Server
	hub   *service.Hub
	accs  *memAccounts
	notes *memNotes
	mails *memMails
}

// opsToken 是测试里运维接口用的令牌。
const opsToken = "test-ops-token"

// newHarness 装出一套与 main 完全相同的路由，只是把三个存储换成内存实现。
//
// 引擎是当场新建的，不用 bases.R：那是个包级变量，多个测试往同一个引擎上
// 装同一批路径，第二次就会 panic。路由构造函数收 *gin.RouterGroup 而不是
// 直接用全局分组，就是为了这里。
func newHarness(t *testing.T) *harness {
	t.Helper()

	accs, notes, mails := newMemAccounts(), newMemNotes(), newMemMails()
	hub := service.NewHub(service.Deps{
		Accounts: accs,
		Notes:    notes,
		Mails:    mails,
		Sessions: newMemSessions(),
	})

	engine := gin.New()
	engine.Use(gin.Recovery())
	v1 := engine.Group("/api")
	health.New(engine, hub)
	auth.New(v1, hub)
	// 与 main 一样：路径来自请求类型上的 path tag，分组只负责套鉴权
	userGroup := v1.Group("", middleware.Auth(hub.Sessions()))
	note.New(userGroup, hub)
	mail.NewUser(userGroup, hub)
	mail.NewOps(v1.Group("", middleware.OpsAuth(opsToken)), hub)

	ts := httptest.NewServer(engine)
	h := &harness{t: t, ts: ts, hub: hub, accs: accs, notes: notes, mails: mails}
	t.Cleanup(func() {
		ts.Close() // 先停 HTTP，让在途请求释放手上的 actor
		hub.Close()
	})
	return h
}

func (that *harness) do(method, path, token string, body any) (int, map[string]any) {
	that.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			that.t.Fatalf("序列化请求失败: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, that.ts.URL+path, rdr)
	if err != nil {
		that.t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := that.ts.Client().Do(req)
	if err != nil {
		that.t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// register 注册并返回 uid。
func (that *harness) register(phone, pw string) string {
	that.t.Helper()
	code, body := that.do(http.MethodPost, "/api/register", "", map[string]string{
		"phone": phone, "password": pw,
	})
	if code != http.StatusCreated {
		that.t.Fatalf("注册失败: code=%d body=%v", code, body)
	}
	uid, ok := body["uid"].(string)
	if !ok || uid == "" {
		that.t.Fatalf("响应缺少 uid: %v", body)
	}
	return uid
}

// login 登录并返回 token。
func (that *harness) login(phone, pw string) string {
	that.t.Helper()
	code, body := that.do(http.MethodPost, "/api/login", "", map[string]string{
		"phone": phone, "password": pw,
	})
	if code != http.StatusOK {
		that.t.Fatalf("登录失败: code=%d body=%v", code, body)
	}
	token, ok := body["token"].(string)
	if !ok || token == "" {
		that.t.Fatalf("响应缺少 token: %v", body)
	}
	return token
}

// --- 测试 ---

// TestFullFlow 注册 → 登录 → 上传 → 获取，走通一整条链路。
func TestFullFlow(t *testing.T) {
	h := newHarness(t)

	uid := h.register("13800138000", "sup3r-secret")
	if uid != "13800138000" {
		t.Fatalf("uid 应当就是手机号, got %q", uid)
	}
	token := h.login("13800138000", "sup3r-secret")

	// 上传两条，第二条应当排在前面（按上传时间倒序）
	for _, text := range []string{"第一条笔记", "第二条笔记"} {
		code, body := h.do(http.MethodPost, "/api/notes", token, map[string]string{"content": text})
		if code != http.StatusCreated {
			t.Fatalf("上传失败: code=%d body=%v", code, body)
		}
		if body["created_at"] == nil {
			t.Fatalf("响应里没有上传日期: %v", body)
		}
		if body["content"] != text {
			t.Fatalf("回显内容不对: %v", body["content"])
		}
		time.Sleep(2 * time.Millisecond) // 让两条的毫秒时间戳不同
	}

	code, body := h.do(http.MethodGet, "/api/notes", token, nil)
	if code != http.StatusOK {
		t.Fatalf("获取失败: code=%d body=%v", code, body)
	}
	notes, _ := body["notes"].([]any)
	if len(notes) != 2 {
		t.Fatalf("应当有 2 条笔记, got %d: %v", len(notes), body)
	}
	first, _ := notes[0].(map[string]any)
	if first["content"] != "第二条笔记" {
		t.Fatalf("最新的一条应当排在最前, got %v", first["content"])
	}
	if first["created_at"] == nil {
		t.Fatal("笔记缺少上传日期")
	}
}

// TestNote800ChineseChars 需求明确要求"至少能存 800 汉字"。
// 800 个汉字在 utf8mb4 下是 2400 字节，这里验证原样往返、一个字不少。
func TestNote800ChineseChars(t *testing.T) {
	h := newHarness(t)
	h.register("13900139000", "another-secret")
	token := h.login("13900139000", "another-secret")

	// 用不重复的内容，避免"截断了但看不出来"
	var sb strings.Builder
	runes := []rune("锦瑟无端五十弦一弦一柱思华年庄生晓梦迷蝴蝶望帝春心托杜鹃沧海月明珠有泪蓝田日暖玉生烟此情可待成追忆只是当时已惘然")
	for i := 0; i < 800; i++ {
		sb.WriteRune(runes[i%len(runes)])
	}
	content := sb.String()
	if n := utf8.RuneCountInString(content); n != 800 {
		t.Fatalf("测试数据不是 800 字: %d", n)
	}
	t.Logf("800 汉字 = %d 字节", len(content))

	code, body := h.do(http.MethodPost, "/api/notes", token, map[string]string{"content": content})
	if code != http.StatusCreated {
		t.Fatalf("上传 800 汉字失败: code=%d body=%v", code, body)
	}

	code, body = h.do(http.MethodGet, "/api/notes", token, nil)
	if code != http.StatusOK {
		t.Fatalf("获取失败: code=%d", code)
	}
	notes, _ := body["notes"].([]any)
	if len(notes) != 1 {
		t.Fatalf("应当有 1 条笔记, got %d", len(notes))
	}
	got, _ := notes[0].(map[string]any)["content"].(string)
	if utf8.RuneCountInString(got) != 800 {
		t.Fatalf("取回的字数不对: %d", utf8.RuneCountInString(got))
	}
	if got != content {
		t.Fatal("取回的内容与上传的不一致")
	}
}

// TestRegisterValidation 注册必须用手机号，密码有长度约束。
func TestRegisterValidation(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name, phone, pw string
		wantCode        int
	}{
		{"正常", "13712345678", "goodpassword", http.StatusCreated},
		{"非手机号-邮箱", "a@b.com", "goodpassword", http.StatusBadRequest},
		{"非手机号-位数不足", "1371234567", "goodpassword", http.StatusBadRequest},
		{"非手机号-开头非1", "23712345678", "goodpassword", http.StatusBadRequest},
		{"非手机号-带国际前缀", "+8613712345678", "goodpassword", http.StatusBadRequest},
		{"密码太短", "13712345679", "short", http.StatusBadRequest},
		// bcrypt 只认前 72 字节，超长必须显式拒绝而不是静默截断
		{"密码超 72 字节", "13712345670", strings.Repeat("a", 73), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.do(http.MethodPost, "/api/register", "", map[string]string{
				"phone": c.phone, "password": c.pw,
			})
			if code != c.wantCode {
				t.Fatalf("code=%d want=%d body=%v", code, c.wantCode, body)
			}
		})
	}
}

// TestDuplicateRegister 同一手机号不能注册两次。
func TestDuplicateRegister(t *testing.T) {
	h := newHarness(t)
	h.register("13600136000", "first-password")

	code, body := h.do(http.MethodPost, "/api/register", "", map[string]string{
		"phone": "13600136000", "password": "second-password",
	})
	if code != http.StatusConflict {
		t.Fatalf("重复注册应当返回 409, got %d body=%v", code, body)
	}
}

// TestConcurrentRegisterSamePhone 并发注册同一个手机号，只能有一个成功。
//
// 这是 auth actor 按手机号分片的意义所在：同一号码永远落到同一个 actor，
// "查重—建号"两步天然串行。换成 Norm 之后这条更关键了——它的异步 Save
// 走 INSERT ... ON DUPLICATE KEY UPDATE，唯一冲突既不报错也没有返回值，
// 数据库那一层已经兜不住重复注册，只剩应用层这一道。
func TestConcurrentRegisterSamePhone(t *testing.T) {
	h := newHarness(t)

	const attempts = 16
	codes := make([]int, attempts)
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			codes[i], _ = h.do(http.MethodPost, "/api/register", "", map[string]string{
				"phone": "13500135000", "password": "concurrent-pw",
			})
		}(i)
	}
	wg.Wait()

	created, conflict, other := 0, 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			other++
		}
	}
	if created != 1 || conflict != attempts-1 || other != 0 {
		t.Fatalf("%d 次并发注册: 成功 %d 冲突 %d 其它 %d，应当是 1/%d/0",
			attempts, created, conflict, other, attempts-1)
	}
}

// TestLoginFailures 登录失败不能泄露手机号是否注册过。
func TestLoginFailures(t *testing.T) {
	h := newHarness(t)
	h.register("13400134000", "right-password")

	for _, c := range []struct{ name, phone, pw string }{
		{"密码错误", "13400134000", "wrong-password"},
		{"账号不存在", "13400134001", "right-password"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.do(http.MethodPost, "/api/login", "", map[string]string{
				"phone": c.phone, "password": c.pw,
			})
			if code != http.StatusUnauthorized {
				t.Fatalf("应当返回 401, got %d", code)
			}
			// 两种失败必须给完全相同的文案
			if body["error"] != "手机号或密码错误" {
				t.Fatalf("错误文案泄露了账号是否存在: %v", body["error"])
			}
		})
	}
}

// TestNotesRequireAuth 没有有效 token 拿不到任何笔记。
func TestNotesRequireAuth(t *testing.T) {
	h := newHarness(t)

	for _, c := range []struct {
		name, token string
	}{
		{"无 token", ""},
		{"伪造 token", "deadbeef"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code, _ := h.do(http.MethodGet, "/api/notes", c.token, nil); code != http.StatusUnauthorized {
				t.Fatalf("GET 应当 401, got %d", code)
			}
			if code, _ := h.do(http.MethodPost, "/api/notes", c.token,
				map[string]string{"content": "x"}); code != http.StatusUnauthorized {
				t.Fatalf("POST 应当 401, got %d", code)
			}
		})
	}
}

// TestNotesIsolatedPerUser 一个用户看不到另一个用户的笔记。
func TestNotesIsolatedPerUser(t *testing.T) {
	h := newHarness(t)
	h.register("13311331133", "alice-password")
	h.register("13322332233", "bob-password")
	alice := h.login("13311331133", "alice-password")
	bob := h.login("13322332233", "bob-password")

	h.do(http.MethodPost, "/api/notes", alice, map[string]string{"content": "爱丽丝的秘密"})

	code, body := h.do(http.MethodGet, "/api/notes", bob, nil)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if n, _ := body["count"].(float64); n != 0 {
		t.Fatalf("Bob 不该看到任何笔记, got %v: %v", n, body)
	}
}

// TestNoteContentValidation 空内容与超长内容都要拒掉。
func TestNoteContentValidation(t *testing.T) {
	h := newHarness(t)
	h.register("13288328832", "carol-password")
	token := h.login("13288328832", "carol-password")

	for _, c := range []struct {
		name     string
		content  string
		wantCode int
	}{
		{"空", "", http.StatusBadRequest},
		{"全是空白", "   \n\t  ", http.StatusBadRequest},
		{"刚好到上限", strings.Repeat("字", comm.MaxNoteRunes), http.StatusCreated},
		{"超出上限", strings.Repeat("字", comm.MaxNoteRunes+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.do(http.MethodPost, "/api/notes", token,
				map[string]string{"content": c.content})
			if code != c.wantCode {
				t.Fatalf("code=%d want=%d body=%v", code, c.wantCode, body)
			}
		})
	}
}

// TestConcurrentUploadSameUser 同一个账号多设备并发上传。
//
// 每个用户一个 actor，该用户的所有写操作排在同一条事件循环上，
// 所以内存缓存和存储不可能对不上——业务代码里一把锁都没有。
func TestConcurrentUploadSameUser(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13177317731", "dave-password")
	token := h.login("13177317731", "dave-password")

	// 先读一次把缓存预热，这样后续上传走的是"改缓存 + 写库"两条都要对齐的路径
	if code, _ := h.do(http.MethodGet, "/api/notes", token, nil); code != http.StatusOK {
		t.Fatal("预热读取失败")
	}

	const uploads = 60
	var wg sync.WaitGroup
	failed := make([]int, uploads)
	wg.Add(uploads)
	for i := 0; i < uploads; i++ {
		go func(i int) {
			defer wg.Done()
			code, _ := h.do(http.MethodPost, "/api/notes", token,
				map[string]string{"content": fmt.Sprintf("并发笔记 %d", i)})
			failed[i] = code
		}(i)
	}
	wg.Wait()

	for i, c := range failed {
		if c != http.StatusCreated {
			t.Fatalf("第 %d 次上传返回 %d", i, c)
		}
	}
	if n := h.notes.count(uid); n != uploads {
		t.Fatalf("存储里有 %d 条，期望 %d 条", n, uploads)
	}

	code, body := h.do(http.MethodGet, "/api/notes", token, nil)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	// 缓存与存储必须一致
	if n, _ := body["count"].(float64); int(n) != uploads {
		t.Fatalf("读到 %v 条，期望 %d 条——缓存与存储不一致", n, uploads)
	}
	// 内容不能重复或丢失
	notes, _ := body["notes"].([]any)
	seen := make(map[string]bool, uploads)
	for _, raw := range notes {
		c, _ := raw.(map[string]any)["content"].(string)
		if seen[c] {
			t.Fatalf("内容重复: %s", c)
		}
		seen[c] = true
	}
	if len(seen) != uploads {
		t.Fatalf("去重后只有 %d 条", len(seen))
	}
}

// TestIdleUserActorEvicted 空闲的用户 actor 会被回收，且回收不丢数据——
// 下次访问重新建 actor 并从存储预热缓存。
func TestIdleUserActorEvicted(t *testing.T) {
	h := newHarness(t)
	h.register("13066306630", "erin-password")
	token := h.login("13066306630", "erin-password")

	h.do(http.MethodPost, "/api/notes", token, map[string]string{"content": "回收前写的"})
	if n := h.hub.OnlineUsers(); n != 1 {
		t.Fatalf("应当有 1 个用户 actor 存活, got %d", n)
	}

	// 直接推进"当前时间"触发回收，不用真等 5 分钟
	if evicted := h.hub.EvictIdle(time.Now().Add(comm.UserIdleTimeout + time.Minute)); evicted != 1 {
		t.Fatalf("应当回收 1 个 actor, got %d", evicted)
	}
	if n := h.hub.OnlineUsers(); n != 0 {
		t.Fatalf("回收后不该还有存活的用户 actor, got %d", n)
	}

	// 再访问：actor 重建，缓存从存储预热，数据一条不少
	code, body := h.do(http.MethodGet, "/api/notes", token, nil)
	if code != http.StatusOK {
		t.Fatalf("回收后再访问失败: code=%d", code)
	}
	if n, _ := body["count"].(float64); n != 1 {
		t.Fatalf("回收后数据丢了: count=%v", n)
	}
	if h.hub.OnlineUsers() != 1 {
		t.Fatal("再访问应当重建 actor")
	}
}

// TestInFlightUserActorNotEvicted 正在服务的 actor 不能被回收，
// 否则在途请求会撞上 actor is closed。
func TestInFlightUserActorNotEvicted(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13055305530", "frank-password")

	h.hub.AcquireUser(uid) // 模拟一个还没结束的请求
	if evicted := h.hub.EvictIdle(time.Now().Add(24 * time.Hour)); evicted != 0 {
		t.Fatalf("使用中的 actor 不该被回收, evicted=%d", evicted)
	}
	h.hub.ReleaseUser(uid)

	if evicted := h.hub.EvictIdle(time.Now().Add(24 * time.Hour)); evicted != 1 {
		t.Fatalf("释放之后应当可以回收, evicted=%d", evicted)
	}
}

// TestStoreFailureSurfacedNotSwallowed 存储报错必须变成 5xx 传出去，
// 不能被吞掉让客户端以为写成功了。
func TestStoreFailureSurfacedNotSwallowed(t *testing.T) {
	h := newHarness(t)
	h.register("13044304430", "grace-password")
	token := h.login("13044304430", "grace-password")

	h.notes.mu.Lock()
	h.notes.onCall = func(op string) error {
		if op == "Insert" {
			return fmt.Errorf("模拟数据库故障")
		}
		return nil
	}
	h.notes.mu.Unlock()

	code, body := h.do(http.MethodPost, "/api/notes", token, map[string]string{"content": "写不进去的笔记"})
	if code < 500 {
		t.Fatalf("存储故障应当返回 5xx, got %d body=%v", code, body)
	}
}

// TestHealthz 探活接口报当前存活的用户 actor 数。
func TestHealthz(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(http.MethodGet, "/healthz", "", nil)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if n, ok := body["online_users"].(float64); !ok || n != 0 {
		t.Fatalf("online_users 应当是 0, got %v", body)
	}
}
