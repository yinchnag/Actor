package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// --- 一个用来被扫的样板路由 ---

type pingRequest struct {
	GET
}

type echoRequest struct {
	POST
	Text string `json:"text"`
}

type aliasRequest struct {
	GET `path:"/custom/path"`
}

type queryRequest struct {
	GET  `path:"/search"`
	Word string `form:"word"`
}

type demo struct {
	Router[*demo]
	seen []string
}

func (that *demo) Ping(_ *pingRequest, ctx *gin.Context) {
	that.seen = append(that.seen, "Ping")
	ctx.String(http.StatusOK, "pong")
}

func (that *demo) Echo(req *echoRequest, ctx *gin.Context) {
	ctx.String(http.StatusOK, req.Text)
}

func (that *demo) Alias(_ *aliasRequest, ctx *gin.Context) {
	ctx.String(http.StatusOK, "alias")
}

func (that *demo) Search(req *queryRequest, ctx *gin.Context) {
	ctx.String(http.StatusOK, "找到:"+req.Word)
}

// hidden 是私有方法，不该被注册成路由。
func (that *demo) hidden() {}

func newDemo(t *testing.T) (*demo, *httptest.Server) {
	t.Helper()
	engine := gin.New()
	d := &demo{}
	d.Init(engine)
	ts := httptest.NewServer(engine)
	t.Cleanup(ts.Close)
	return d, ts
}

func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp)
}

func post(t *testing.T, ts *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := ts.Client().Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp)
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// --- 测试 ---

// TestRouterDerivesPathFromMethodName 没写 path tag 时按方法名小写推路径。
func TestRouterDerivesPathFromMethodName(t *testing.T) {
	_, ts := newDemo(t)
	if code, body := get(t, ts, "/ping"); code != http.StatusOK || body != "pong" {
		t.Fatalf("GET /ping = %d %q", code, body)
	}
}

// TestRouterPathTagOverrides path tag 优先于方法名。
//
// 这条是本项目相对 roleSvr 加的能力：没有它，GET 和 POST 就没法共用
// /notes 这一个路径。
func TestRouterPathTagOverrides(t *testing.T) {
	_, ts := newDemo(t)
	if code, _ := get(t, ts, "/custom/path"); code != http.StatusOK {
		t.Fatalf("GET /custom/path = %d，path tag 没生效", code)
	}
	// 按方法名推出来的那条不该存在
	if code, _ := get(t, ts, "/alias"); code != http.StatusNotFound {
		t.Fatalf("GET /alias = %d，写了 path tag 就不该再注册默认路径", code)
	}
}

// TestRouterVerbFromMarker 动词由内嵌标记决定，不是所有路由都是 GET。
func TestRouterVerbFromMarker(t *testing.T) {
	_, ts := newDemo(t)
	if code, body := post(t, ts, "/echo", `{"text":"你好"}`); code != http.StatusOK || body != "你好" {
		t.Fatalf("POST /echo = %d %q", code, body)
	}
	// Echo 声明的是 POST，用 GET 打就该是 405
	if code, _ := get(t, ts, "/echo"); code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
		t.Fatalf("GET /echo = %d，POST 路由不该接受 GET", code)
	}
}

// TestRouterBindsJSONBody 请求体自动绑定，不必手写 FromWithContext。
func TestRouterBindsJSONBody(t *testing.T) {
	_, ts := newDemo(t)
	// 未知字段要被拒——BindJSON 开了 DisallowUnknownFields
	if code, _ := post(t, ts, "/echo", `{"text":"x","typo":1}`); code != http.StatusBadRequest {
		t.Fatalf("带未知字段的请求体 = %d，应当 400", code)
	}
	if code, _ := post(t, ts, "/echo", `不是 JSON`); code != http.StatusBadRequest {
		t.Fatalf("非法 JSON = %d，应当 400", code)
	}
}

// TestRouterBindsQuery GET 从 query 绑定，按 form tag 匹配。
func TestRouterBindsQuery(t *testing.T) {
	_, ts := newDemo(t)
	if code, body := get(t, ts, "/search?word=笔记"); code != http.StatusOK || body != "找到:笔记" {
		t.Fatalf("GET /search = %d %q", code, body)
	}
}

// TestRouterNoBodyForFieldlessRequest 只有标记、没有字段的请求类型不做绑定。
//
// 不跳过的话，空请求体会 EOF、非空会"未知字段"，两条都是假报错。
func TestRouterNoBodyForFieldlessRequest(t *testing.T) {
	_, ts := newDemo(t)
	if code, _ := get(t, ts, "/ping"); code != http.StatusOK {
		t.Fatalf("无字段的请求类型不该要求请求体, got %d", code)
	}
}

// TestRouterSkipsPrivateAndBaseMethods 私有方法与 Router 自己的方法不注册。
func TestRouterSkipsPrivateAndBaseMethods(t *testing.T) {
	d, ts := newDemo(t)
	for _, p := range []string{"/hidden", "/init", "/routes"} {
		if code, _ := get(t, ts, p); code != http.StatusNotFound {
			t.Errorf("GET %s = %d，不该被注册成路由", p, code)
		}
	}
	if n := len(d.Routes()); n != 4 {
		t.Fatalf("应当注册 4 条路由, got %d: %v", n, d.Routes())
	}
}

// TestRouterRoutesDescription Routes() 给出可读的注册结果，启动日志靠它。
func TestRouterRoutesDescription(t *testing.T) {
	d, _ := newDemo(t)
	joined := strings.Join(d.Routes(), "\n")
	for _, want := range []string{"GET", "POST", "/ping", "/custom/path", "demo.Ping"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Routes() 里缺少 %q:\n%s", want, joined)
		}
	}
}

// --- 自定义解析 ---

type customRequest struct {
	POST `path:"/custom"`
	Name string
}

// FromWithContext 完全接管参数解析。返回 false 表示已写应答、别再调业务方法。
func (that *customRequest) FromWithContext(ctx *gin.Context) bool {
	that.Name = ctx.Query("name")
	if that.Name == "" {
		defaultWriteError(ctx, http.StatusBadRequest, "name 不能为空")
		return false
	}
	return true
}

type customRouter struct {
	Router[*customRouter]
	called int
}

func (that *customRouter) Do(req *customRequest, ctx *gin.Context) {
	that.called++
	ctx.String(http.StatusOK, "收到:"+req.Name)
}

// TestRouterCustomIRequest 请求类型实现 IRequest 时由它自己解析。
func TestRouterCustomIRequest(t *testing.T) {
	engine := gin.New()
	r := &customRouter{}
	r.Init(engine)
	ts := httptest.NewServer(engine)
	defer ts.Close()

	code, body := post(t, ts, "/custom?name=张三", "")
	if code != http.StatusOK || body != "收到:张三" {
		t.Fatalf("= %d %q", code, body)
	}
	// 解析失败必须拦住业务方法。roleSvr 的同名接口没有返回值，
	// 于是解析失败时业务方法照样会被调用，拿到一个半填充的参数。
	if code, _ := post(t, ts, "/custom", ""); code != http.StatusBadRequest {
		t.Fatalf("缺少 name 时 = %d，应当 400", code)
	}
	if r.called != 1 {
		t.Fatalf("业务方法被调用了 %d 次，解析失败的那次不该进来", r.called)
	}
}

// --- 写错了要当场炸 ---

func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("应当 panic 并提到 %q，但没有 panic", want)
		}
		if msg := toString(r); !strings.Contains(msg, want) {
			t.Fatalf("panic 信息里应当提到 %q，实际是 %q", want, msg)
		}
	}()
	fn()
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

// strayTarget 不是路由，没有内嵌 Router。
type strayTarget struct{ Name string }

// stray 的类型参数指向了一个根本不是路由的类型。
type stray struct {
	Router[*strayTarget]
}

// TestPanicOnTypeParamWithoutRouter 类型参数指向的类型里没有 Router 字段。
//
// 编译期发现不了：Router[*strayTarget] 是个合法类型。运行期的表现是
// 偏移查找失败、恢复不出宿主对象——所以在 Init 当场炸。
func TestPanicOnTypeParamWithoutRouter(t *testing.T) {
	mustPanic(t, "类型参数应当写成自己", func() {
		s := &stray{}
		s.Init(gin.New())
	})
}

// otherRouter 是一个正常的路由，用来演示下面那种检测不到的误用。
type otherRouter struct{ Router[*otherRouter] }

func (that *otherRouter) Whoami(_ *pingRequest, ctx *gin.Context) {
	ctx.String(http.StatusOK, "other")
}

// mimic 的类型参数写成了**另一个也内嵌着自己的路由**。
type mimic struct {
	Router[*otherRouter] // 写错了，应当是 Router[*mimic]
}

func (that *mimic) Mine(_ *aliasRequest, ctx *gin.Context) {
	ctx.String(http.StatusOK, "mine")
}

// TestWrongTypeParamThatLooksValid 记录一条**检测不到**的误用，别高估这套校验。
//
// otherRouter 自己也内嵌着 Router[*otherRouter]，所以按偏移找字段会"成功"，
// 恢复出一个指向 mimic 内存、却被当成 *otherRouter 用的指针。两个结构体的
// 布局又恰好一样，运行期没有任何信息能把它们区分开。
//
// 兜底的是另外两层，测试在这里把它们钉住：
//  1. 注册出来的是 otherRouter 的接口而不是 mimic 的——启动日志里
//     Routes() 打的是 otherRouter.Whoami，一眼就能看出不对；
//  2. 真装配时 otherRouter 自己也要注册，gin 会因为同路径重复注册而 panic。
func TestWrongTypeParamThatLooksValid(t *testing.T) {
	m := &mimic{}
	m.Init(gin.New())

	routes := strings.Join(m.Routes(), " | ")
	if !strings.Contains(routes, "otherRouter.Whoami") {
		t.Fatalf("扫出来的应当是 otherRouter 的方法（这正是误用的症状）: %s", routes)
	}
	if strings.Contains(routes, "mimic.Mine") {
		t.Fatalf("类型参数写错时不该扫到自己的方法: %s", routes)
	}

	// 第二层兜底：同一个引擎上再注册真正的 otherRouter，gin 直接 panic
	engine := gin.New()
	m2 := &mimic{}
	m2.Init(engine)
	mustPanic(t, "already registered for path", func() {
		o := &otherRouter{}
		o.Init(engine)
	})
}

type badArity struct{ Router[*badArity] }

func (that *badArity) Oops(ctx *gin.Context) {}

// TestPanicOnBadArity 少一个参数就不是合法的路由方法。
func TestPanicOnBadArity(t *testing.T) {
	mustPanic(t, "签名不对", func() {
		b := &badArity{}
		b.Init(gin.New())
	})
}

type badReturn struct{ Router[*badReturn] }

func (that *badReturn) Oops(_ *pingRequest, ctx *gin.Context) error { return nil }

// TestPanicOnReturnValue 有返回值也不行——没人接得住。
func TestPanicOnReturnValue(t *testing.T) {
	mustPanic(t, "签名不对", func() {
		b := &badReturn{}
		b.Init(gin.New())
	})
}

type badSecond struct{ Router[*badSecond] }

func (that *badSecond) Oops(_ *pingRequest, _ string) {}

// TestPanicOnNonContextSecondParam 第二个参数必须是 *gin.Context。
func TestPanicOnNonContextSecondParam(t *testing.T) {
	mustPanic(t, "必须是 *gin.Context", func() {
		b := &badSecond{}
		b.Init(gin.New())
	})
}

type badFirst struct{ Router[*badFirst] }

func (that *badFirst) Oops(_ string, _ *gin.Context) {}

// TestPanicOnNonStructFirstParam roleSvr 在这种情况下会静默退化成 GET 路由。
func TestPanicOnNonStructFirstParam(t *testing.T) {
	mustPanic(t, "必须是指向请求结构体的指针", func() {
		b := &badFirst{}
		b.Init(gin.New())
	})
}

type noVerbRequest struct {
	Word string `json:"word"`
}

type noVerb struct{ Router[*noVerb] }

func (that *noVerb) Oops(_ *noVerbRequest, _ *gin.Context) {}

// TestPanicOnMissingVerb 请求类型忘了内嵌动词标记。
func TestPanicOnMissingVerb(t *testing.T) {
	mustPanic(t, "没有内嵌动词标记", func() {
		b := &noVerb{}
		b.Init(gin.New())
	})
}
