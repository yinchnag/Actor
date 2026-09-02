package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 这组基准回答一个具体问题：自动注册路由的反射，到底值多少钱。
//
// 结论先写在这里，免得后来人重新推一遍：**在 HTTP 请求的尺度上可以忽略**。
// 28 核机器实测（go1.24）。分发本身：
//
//	DispatchDirect        36 ns/op    1 allocs   不经反射，基线
//	DispatchReflectNew    19 ns/op    1 allocs   造请求对象
//	DispatchReflectCall  150 ns/op    2 allocs   调用（含参数切片与 ValueOf(ctx)）
//	DispatchReflectFull  183 ns/op    3 allocs   每请求的反射总开销
//
// 自动 vs 手写，同一个 handler 挂在同一个引擎上，交替执行 6 轮取值：
//
//	ServeAutoRoute    2673 2694 2702 2718 2772 2843 ns/op   32 allocs
//	ServeManualRoute  2488 2492 2499 2511 2526 2611 ns/op   31 allocs
//
// 差 **约 205 ns、8%、1 次分配**，六轮里自动每一轮都慢，是个稳定可复现的差值。
// 但这是摘掉网络之后的数字；带上 TCP 栈（HTTPAutoRoute/HTTPManualRoute）就是
// 7018~7548 对 7006~7786，四轮两胜两负——205ns 落在 ~7200ns 里只有 3%，
// 被网络的 ±500ns 噪声完全盖住。放进真实服务（MySQL+Redis，每请求 ~70µs）
// 更是 0.3%。
//
// 真要优化该盯 Call（~150ns）而不是 New（~19ns），前者贵八倍。
//
// 量它的时候有三个会被坑的地方：
//
//  1. **别用上层压测（NOTE_STRESS）去量**。那套东西的吞吐在同一份代码上有 38%
//     的散布（34451~47572 req/s），p50 还在 1.0ms 与 1.5ms 之间跳——那是调度与
//     计时粒度，噪声比要测的效应大一个数量级。
//  2. **响应体必须读干净再关**。只 Close 不读，连接回不到空闲池，每次请求重开
//     TCP；实测会把 6µs 变成 400µs，还会让两条路由的快慢顺序反过来。
//  3. **两条对照要交替跑，别靠 -count**。基准是顺序执行的，机器在过程中变忙会
//     让后跑的那条吃亏：实测同一条 ServeManualRoute 在 -count=10 里从 2515 一路
//     漂到 4925，纯粹是顺序偏差。交替执行才看得到真实差值。

type benchReq struct {
	POST `path:"/auto"`
	Text string `json:"text"`
}

type benchRouter struct {
	Router[*benchRouter]
}

func (that *benchRouter) Auto(req *benchReq, ctx *gin.Context) {
	ctx.String(http.StatusOK, req.Text)
}

func benchCtx() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/auto", nil)
	return c
}

// BenchmarkDispatchDirect 基线：不经反射直接调，编译器会把它内联掉。
func BenchmarkDispatchDirect(b *testing.B) {
	r := &benchRouter{}
	ctx := benchCtx()
	b.ReportAllocs()
	for b.Loop() {
		r.Auto(&benchReq{}, ctx)
	}
}

// BenchmarkDispatchReflectNew 只测造请求对象那一步。
func BenchmarkDispatchReflectNew(b *testing.B) {
	t := reflect.TypeFor[benchReq]()
	b.ReportAllocs()
	for b.Loop() {
		_ = reflect.New(t)
	}
}

// BenchmarkDispatchReflectCall 只测调用那一步（含参数切片与 ValueOf(ctx)）。
//
// 它比 reflect.New 贵七倍——两处反射里真正花钱的是这一处，
// 想优化的话该盯着它，而不是盯着对象创建。
func BenchmarkDispatchReflectCall(b *testing.B) {
	r := &benchRouter{}
	r.Init(gin.New())
	mv := reflect.ValueOf(r).MethodByName("Auto")
	req := reflect.New(reflect.TypeFor[benchReq]())
	ctx := benchCtx()
	b.ReportAllocs()
	for b.Loop() {
		mv.Call([]reflect.Value{req, reflect.ValueOf(ctx)})
	}
}

// BenchmarkDispatchReflectFull 完整的每请求反射路径（不含 JSON 解码）。
func BenchmarkDispatchReflectFull(b *testing.B) {
	r := &benchRouter{}
	r.Init(gin.New())
	mv := reflect.ValueOf(r).MethodByName("Auto")
	t := reflect.TypeFor[benchReq]()
	ctx := benchCtx()
	b.ReportAllocs()
	for b.Loop() {
		mv.Call([]reflect.Value{reflect.New(t), reflect.ValueOf(ctx)})
	}
}

// --- 对照：同一个 handler，一条自动注册、一条手写注册，走完整 HTTP 栈 ---

func newBenchServer() *httptest.Server {
	engine := gin.New()
	r := &benchRouter{}
	r.Init(engine) // 自动注册 → POST /auto

	// 手写注册，逻辑与上面完全一致，只是不经反射
	engine.POST("/manual", func(ctx *gin.Context) {
		req := &benchReq{}
		if !BindJSON(ctx, req) {
			return
		}
		r.Auto(req, ctx)
	})
	return httptest.NewServer(engine)
}

func benchHTTP(b *testing.B, path string) {
	b.Helper()
	ts := newBenchServer()
	defer ts.Close()
	// 默认 MaxIdleConnsPerHost 是 2，不放开的话测的是 TCP 握手
	ts.Client().Transport = &http.Transport{MaxIdleConnsPerHost: 256, MaxConnsPerHost: 256}
	url := ts.URL + path

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := ts.Client().Post(url, "application/json", strings.NewReader(`{"text":"hello"}`))
			if err != nil {
				b.Fatal(err)
			}
			// 必须读干净再关。不读的话连接回不到空闲池，每次请求都重开 TCP，
			// 测出来的是握手和 TIME_WAIT 堆积——实测会把 6µs 变成 400µs，
			// 而且噪声大到把要测的 2% 彻底淹掉。
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
}

// BenchmarkHTTPAutoRoute / BenchmarkHTTPManualRoute 是那个 2% 的直接证据：
// 两者的耗时区间完全重叠，只差 1 次分配。
func BenchmarkHTTPAutoRoute(b *testing.B)   { benchHTTP(b, "/auto") }
func BenchmarkHTTPManualRoute(b *testing.B) { benchHTTP(b, "/manual") }

// --- 去掉网络层的对照：直接 ServeHTTP ---
//
// 上面的 HTTPAutoRoute/HTTPManualRoute 带着完整 TCP 栈，噪声 ±10%，
// 分辨不出百分之几的差别。这两条把网络摘掉，只剩 gin 的路由匹配、
// 中间件链、请求体绑定和 handler 调用——两条路径唯一的区别就是
// "反射分发"还是"直接调"，于是那个差值才测得出来。

func serveEngine() *gin.Engine {
	engine := gin.New()
	r := &benchRouter{}
	r.Init(engine) // 自动注册 → POST /auto

	engine.POST("/manual", func(ctx *gin.Context) {
		req := &benchReq{}
		if !BindJSON(ctx, req) {
			return
		}
		r.Auto(req, ctx)
	})
	return engine
}

func benchServe(b *testing.B, path string) {
	b.Helper()
	engine := serveEngine()
	body := `{"text":"hello"}`
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("code=%d", w.Code)
		}
	}
}

// BenchmarkServeAutoRoute / BenchmarkServeManualRoute 是"自动 vs 手写"最干净的对照。
func BenchmarkServeAutoRoute(b *testing.B)   { benchServe(b, "/auto") }
func BenchmarkServeManualRoute(b *testing.B) { benchServe(b, "/manual") }
