# web

基于 gin 的自动路由：**路由结构体内嵌 `Router[*自己]`，公有方法被反射扫成 HTTP 接口**，
动词与路径由请求结构体上的标记声明。全程没有一处 `group.POST("/login", ...)`。

它和同仓库的 `actor` 框架是**平级的两件东西，互不依赖** —— 本包一行 actor 的代码都
没引用，共用的只是 CRTP 这个手法（`actor.ModObj[T]` 解决"跨 goroutine 按方法名调用
模块"，这里解决"按方法签名生成 HTTP 路由"）。

拆成独立模块而不是塞进 `actor`，是为了 gin：它会拖进 26 个传递依赖（sonic、
validator、go-playground/\*、golang.org/x/net、protobuf…）。只想要 actor 并发模型、
根本不做 HTTP 的项目不该为此买单。

## 用法

```go
type Auth struct {
    web.Router[*Auth]              // 类型参数必须是自己
    svc *Service
}

type LoginRequest struct {
    web.POST                       // 动词；不写 path tag 就按方法名推出 /login
    Phone    string `json:"phone"`
    Password string `json:"password"`
}

// 扫到这个方法 → 注册 POST /api/login，req 已经绑定好
func (that *Auth) Login(req *LoginRequest, ctx *gin.Context) {
    ...
}

func main() {
    a := &Auth{svc: svc}
    a.Init(engine.Group("/api"))
    for _, r := range a.Routes() {
        log.Println(r)             // POST   /api/login   → Auth.Login
    }
}
```

加一个接口 = 加一个方法 + 一个请求类型。

## 规则

**方法签名必须是 `func(req *XxxRequest, ctx *gin.Context)`。** 参数个数、
`*gin.Context` 的位置、返回值、请求类型缺动词标记，任何一条不对都在 `Init` 就 panic。
不做静默降级：路由结构体上的公有方法只有一个用途就是当接口，"写了个方法但没成为
路由"没有合理的解释，而它的症状是启动一切正常、接口 404。

**路径默认按方法名小写推**（`Login` → `/login`），用 tag 覆盖：

```go
type ListRequest   struct{ web.GET  `path:"/notes"` }
type UploadRequest struct {
    web.POST `path:"/notes"`
    Content  string `json:"content"`
}
```

GET 与 POST 共用同一个路径就是这么写的 —— 没有 tag 就表达不出来。

**参数默认自动绑定**：POST/PUT 从 JSON 请求体，GET/DELETE 按 `form` tag 从 query。
只有动词标记、没有字段的请求类型完全跳过绑定（不跳过的话，空请求体会 EOF、
非空会"未知字段"，两条都是假报错）。

**要完全接管解析**就实现 `IRequest`：

```go
func (that *MyRequest) FromWithContext(ctx *gin.Context) bool {
    that.Code = ctx.Query("code")
    if that.Code == "" {
        web.BindJSON /* 或你自己的 */
        return false                // 返回 false = 已写应答，别再调业务方法
    }
    return true
}
```

返回值不能省：没有它的话，解析失败时业务方法照样会被调用，拿到一个半填充的参数。

**`Init` 收 `gin.IRoutes`**，所以引擎本身和任意分组都能挂 —— `/healthz` 这种要挂在
根上、不带前缀的路由不用开特例。

## 两个旋钮

库只管"怎么把方法扫成路由"，剩下两件事是**业务决定**，不替使用者做：

```go
a.Init(group,
    web.WithMaxBodyBytes(comm.MaxBodyBytes),   // 请求体多大算大
    web.WithErrorWriter(myFail),               // 错误应答长什么样
)
```

`WithErrorWriter` 传进来的函数必须**自己终止请求**（gin 里就是
`AbortWithStatusJSON` 之类），否则绑定失败之后业务方法还会被调用。

默认上限 `DefaultMaxBodyBytes = 256KB` 不是随手取的整数，它对齐 `net/http` 的
`maxPostHandlerReadBytes`：handler 提前拒掉请求、请求体没读完时，服务器会替你把
剩下的读掉丢弃，好让这条 keep-alive 连接停在干净的消息边界上；但 drain 的工作量由
对端决定，所以有上限，剩余超过 256KB 就直接断连接。把请求体上限压到正好等于那个
额度，"能收下但拒了就得断连"的灰区就没了。

## 一个检测不到的误用

类型参数写错编译期发现不了，`Init` 只挡得住一半：

- **挡得住**：`Router[*T]` 里 `T` 根本没有内嵌 `Router[*T]`（指向了普通结构体）。
  偏移查找失败，panic。
- **挡不住**：`T` 指向了另一个**也正确内嵌着自己**的路由，比如 `Note` 里写成
  `Router[*Auth]`。`Auth` 确实有那个字段，偏移查找会"成功"，恢复出一个指向 `Note`
  内存却被当成 `*Auth` 的指针；两者布局若又一致，运行期没有信息能区分。

后一种靠两层兜底，`TestWrongTypeParamThatLooksValid` 把它们钉住了：`Routes()` 打出来
的是 `Auth.*` 而不是 `Note.*`，启动日志里一眼能看出来；真装配时 `Auth` 自己也要注册，
gin 会因为同路径重复注册直接 panic。

## 反射的开销

反射出现在两处：**启动时**扫方法建路由表（一次性），**每请求时**一次 `reflect.New`
造请求对象加一次 `reflect.Value.Call` 调方法。

28 核实测（`go test -bench . -benchmem`）：

```
DispatchDirect        36 ns/op   1 allocs   不经反射，基线
DispatchReflectNew    19 ns/op   1 allocs   造请求对象
DispatchReflectCall  150 ns/op   2 allocs   调用（含参数切片与 ValueOf(ctx)）
DispatchReflectFull  183 ns/op   3 allocs   每请求的反射总开销
```

自动注册 vs 手写注册，同一个 handler 挂在同一个引擎上，**交替执行**取值：

| | 自动 | 手写 | 差值 |
|---|---|---|---|
| `ServeHTTP`（无网络） | 2673~2843 ns | 2488~2611 ns | **+205 ns，+8%，+1 alloc** |
| 完整 HTTP（含 TCP） | 7018~7548 ns | 7006~7786 ns | 测不出来（四轮两胜两负） |

205 ns 稳定可复现（六轮里自动每轮都慢），但只在摘掉网络后才看得见；带上 TCP 就落进
±500 ns 的抖动里。真要优化该盯 `Call`（~150 ns）而不是 `New`（~19 ns），差八倍。
`Init` 收的是 `gin.IRoutes`，热点接口随时可以改回手写注册，两种方式能在同一个引擎上共存。

量它的时候有三个坑，`router_bench_test.go` 的注释里记了：**响应体必须读干净再关**
（只 Close 不读，连接回不到空闲池，6µs 变 400µs）；**两条对照要交替跑**（基准顺序
执行，机器变忙会让后跑的吃亏）；**别用上层压测去量**（吞吐散布 38%，噪声比效应大一
个数量级）。

## 测试

```bash
go test ./... -count=1
go test ./... -bench . -benchmem
```

`router_test.go` 覆盖路径推导、tag 覆盖、动词、两种绑定、自定义解析，
以及五种写错签名时是否当场 panic。
