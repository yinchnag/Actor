package web

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"unsafe"

	"github.com/gin-gonic/gin"
)

// Router 是自动路由的基座。宿主内嵌 Router[*自己]，公有方法被扫成 HTTP 接口。
//
// 用法见包注释。这里说三条设计取舍：
//
//  1. **路径可以由标记上的 tag 指定**，而不是只按方法名小写推。没有它，
//     GET 与 POST 共用同一个路径（很常见，比如 /notes）就表达不出来。
//  2. **请求体默认自动绑定**，不必每个请求类型手写解析；需要完全接管时
//     实现 IRequest 即可，那条口子仍然留着。
//  3. **签名不合规当场 panic**，不静默降级。路由结构体上的公有方法只有一个
//     用途就是当接口，"写了个方法但没成为路由"没有合理的解释，
//     而它的症状是启动一切正常、接口 404——最不该静默的一类错误。
type Router[T any] struct {
	name   string
	heir   T
	routes []*routeHandler
}

// baseMethodSet 是 Router 自己提升到宿主上的公有方法，不能被当成路由。
var baseMethodSet = map[string]struct{}{
	"Init":   {},
	"Routes": {},
}

// --- HTTP 动词标记 ---
//
// 内嵌到请求结构体里，用来声明这个请求走哪个 HTTP 方法：
//
//	type LoginRequest struct {
//	    web.POST
//	    Phone string `json:"phone"`
//	}
//
// 空结构体，不占空间。可以带一个 path tag 覆盖默认路径：
//
//	type UploadRequest struct {
//	    web.POST `path:"/notes"`
//	    ...
//	}
type (
	// GET 声明这个请求走 HTTP GET，参数从 query 绑定。
	GET struct{}
	// POST 声明这个请求走 HTTP POST，参数从 JSON 请求体绑定。
	POST struct{}
	// PUT 声明这个请求走 HTTP PUT，参数从 JSON 请求体绑定。
	PUT struct{}
	// DELETE 声明这个请求走 HTTP DELETE，参数从 query 绑定。
	DELETE struct{}
)

// verbOf 把标记类型映射成 HTTP 方法名。
//
// 用 map 而不是 switch：加一个动词只需要在这里和上面各加一行，
// 不会漏掉某个分支——漏了的表现是"路由静默不注册"，很难查。
var verbOf = map[reflect.Type]string{
	reflect.TypeFor[GET]():    http.MethodGet,
	reflect.TypeFor[POST]():   http.MethodPost,
	reflect.TypeFor[PUT]():    http.MethodPut,
	reflect.TypeFor[DELETE](): http.MethodDelete,
}

// bodyVerbs 是需要读请求体的动词。GET/DELETE 从 query 取参数。
var bodyVerbs = map[string]bool{
	http.MethodPost: true,
	http.MethodPut:  true,
}

var (
	ginCtxType  = reflect.TypeFor[*gin.Context]()
	requestType = reflect.TypeFor[IRequest]()
)

// routeHandler 是一条已解析好的路由。
type routeHandler struct {
	RouterName string        // 路由结构体名，如 Auth
	MethodName string        // 方法名，如 Login
	Verb       string        // HTTP 方法
	Path       string        // 注册路径
	Ref        reflect.Value // 已绑定接收者的方法值
	ReqType    reflect.Type  // 请求结构体（已剥指针）
	Custom     bool          // 请求类型是否实现了 IRequest
	NeedsBind  bool          // 除标记外还有没有要绑定的字段

	// full 是算上分组前缀之后的完整路径，只用于 Routes() 的展示。
	// 注册时用的仍然是 Path——前缀由 gin 的分组自己加。
	full string
}

// Init 扫描宿主的公有方法并把它们注册到 group 上。
//
// 参数是 gin.IRoutes 而不是 *gin.RouterGroup：引擎本身也实现了它，
// 于是 /healthz 这种要挂在根上、不带前缀的路由也能走同一套。
func (that *Router[T]) Init(group gin.IRoutes, opts ...Option) {
	cfg := newConfig(opts)
	that.bindHeirByOffset()
	that.parseRoutes()
	prefix := basePathOf(group)
	for _, r := range that.routes {
		r.full = prefix + r.Path
		that.register(group, r, cfg)
	}
}

// Routes 返回已注册路由的可读描述，供启动日志或排障使用。
//
// 路由不再写在代码里之后，这就成了唯一能一眼看全"到底开了哪些接口"的地方，
// 值得在启动时打出来。
func (that *Router[T]) Routes() []string {
	out := make([]string, 0, len(that.routes))
	for _, r := range that.routes {
		out = append(out, fmt.Sprintf("%-6s %-20s → %s.%s", r.Verb, r.full, r.RouterName, r.MethodName))
	}
	return out
}

// basePathOf 取分组的前缀，用于日志展示。
//
// gin.IRoutes 没有声明 BasePath，但 *gin.Engine 和 *gin.RouterGroup 都实现了它，
// 所以做一次可选断言。取不到就当没有前缀——展示不准总好过为此换掉接口类型，
// 那会让挂在引擎根上的路由用不了这套。
func basePathOf(group gin.IRoutes) string {
	bp, ok := group.(interface{ BasePath() string })
	if !ok {
		return ""
	}
	return strings.TrimSuffix(bp.BasePath(), "/")
}

// bindHeirByOffset 由 Router 字段的偏移量反推出外层路由对象。
//
// Router[T] 的方法接收者是内嵌字段本身，而反射方法表要从宿主类型上取，
// 所以必须先拿到宿主指针：宿主基址 = Router 字段地址 - 该字段的偏移量。
//
// 类型参数写错编译期发现不了，这里只能挡住一部分，别高估它：
//
//   - **挡得住**：T 指向的类型里根本没有 Router[T] 字段（比如指向了一个
//     普通结构体）。偏移查找失败，当场 panic。
//   - **挡不住**：T 指向了另一个**也正确内嵌着自己**的路由，比如 Note 里
//     写成 Router[*Auth]。Auth 自己确实有 Router[*Auth] 字段，偏移查找会
//     "成功"，恢复出一个指向 Note 内存却被当成 *Auth 用的指针；两者布局
//     若又一致，运行期没有任何信息能区分。
//
// 后一种靠另外两层兜：Routes() 打出来的路由名是 Auth.* 而不是 Note.*，
// 启动日志里一眼能看出来；而真装配时 Auth 自己也要注册，gin 会因为同路径
// 重复注册直接 panic。测试见 TestWrongTypeParamThatLooksValid。
func (that *Router[T]) bindHeirByOffset() {
	var zero T
	heirType := reflect.TypeOf(zero)
	if heirType == nil || heirType.Kind() != reflect.Ptr || heirType.Elem().Kind() != reflect.Struct {
		panic("web: Router[T] 的 T 必须是指向结构体的指针，例如 Router[*Auth]")
	}
	ownerType := heirType.Elem()

	selfType := reflect.TypeOf(Router[T]{})
	offset := uintptr(0)
	found := false
	for i := range ownerType.NumField() {
		f := ownerType.Field(i)
		if f.Type == selfType {
			offset, found = f.Offset, true
			break
		}
	}
	if !found {
		panic(fmt.Sprintf("web: %s 里没有内嵌 %s——类型参数应当写成自己，即 Router[*%s]",
			ownerType.Name(), selfType.String(), ownerType.Name()))
	}

	ownerPtr := unsafe.Pointer(uintptr(unsafe.Pointer(that)) - offset)
	heir, ok := reflect.NewAt(ownerType, ownerPtr).Interface().(T)
	if !ok {
		panic(fmt.Sprintf("web: 无法从 %s 恢复宿主对象", ownerType.Name()))
	}
	that.heir = heir
	that.name = ownerType.Name()
}

// parseRoutes 扫描宿主的公有方法，把符合签名的登记成路由。
func (that *Router[T]) parseRoutes() {
	hv := reflect.ValueOf(that.heir)
	ht := hv.Type()

	for i := range ht.NumMethod() {
		m := ht.Method(i)
		if m.PkgPath != "" {
			continue // 私有方法
		}
		if _, skip := baseMethodSet[m.Name]; skip {
			continue
		}
		that.routes = append(that.routes, that.parseOne(hv.MethodByName(m.Name), m.Name))
	}
}

func (that *Router[T]) parseOne(mv reflect.Value, name string) *routeHandler {
	where := that.name + "." + name
	mt := mv.Type() // 已绑定接收者，In(0) 就是第一个真实参数

	if mt.NumIn() != 2 || mt.NumOut() != 0 {
		panic(fmt.Sprintf("web: %s 的签名不对，路由方法必须是 "+
			"func(req *XxxRequest, ctx *gin.Context)", where))
	}
	if mt.In(1) != ginCtxType {
		panic(fmt.Sprintf("web: %s 的第二个参数必须是 *gin.Context，实际是 %s", where, mt.In(1)))
	}
	reqPtr := mt.In(0)
	if reqPtr.Kind() != reflect.Ptr || reqPtr.Elem().Kind() != reflect.Struct {
		panic(fmt.Sprintf("web: %s 的第一个参数必须是指向请求结构体的指针，实际是 %s", where, reqPtr))
	}
	reqType := reqPtr.Elem()

	verb, path, ok := verbAndPath(reqType)
	if !ok {
		panic(fmt.Sprintf("web: %s 的请求类型 %s 没有内嵌动词标记，"+
			"应当内嵌 web.GET / POST / PUT / DELETE 之一", where, reqType.Name()))
	}
	if path == "" {
		path = "/" + strings.ToLower(name)
	}

	return &routeHandler{
		RouterName: that.name,
		MethodName: name,
		Verb:       verb,
		Path:       path,
		Ref:        mv,
		ReqType:    reqType,
		Custom:     reqPtr.Implements(requestType),
		NeedsBind:  hasBindableField(reqType),
	}
}

// verbAndPath 从请求结构体的内嵌标记里取出动词和（可选的）路径。
func verbAndPath(reqType reflect.Type) (verb, path string, ok bool) {
	for i := range reqType.NumField() {
		f := reqType.Field(i)
		if !f.Anonymous {
			continue
		}
		v, isVerb := verbOf[f.Type]
		if !isVerb {
			continue
		}
		return v, f.Tag.Get("path"), true
	}
	return "", "", false
}

// hasBindableField 判断请求结构体除了动词标记之外还有没有字段。
//
// 没有字段就不做绑定。这条不是优化：JSON 解码开了 DisallowUnknownFields，
// 拿一个只有标记的空结构体去解，空请求体会 EOF、非空请求体会"未知字段"，
// 两条都是假报错。
func hasBindableField(reqType reflect.Type) bool {
	for i := range reqType.NumField() {
		f := reqType.Field(i)
		if f.Anonymous {
			if _, isVerb := verbOf[f.Type]; isVerb {
				continue
			}
		}
		if f.PkgPath == "" { // 公有字段
			return true
		}
	}
	return false
}

// register 把一条路由挂到 gin 上。
func (that *Router[T]) register(group gin.IRoutes, r *routeHandler, cfg *config) {
	h := func(ctx *gin.Context) {
		req, ok := r.bindRequest(ctx, cfg)
		if !ok {
			return // 绑定失败时应答已经写好了
		}
		r.Ref.Call([]reflect.Value{req, reflect.ValueOf(ctx)})
	}

	switch r.Verb {
	case http.MethodGet:
		group.GET(r.Path, h)
	case http.MethodPost:
		group.POST(r.Path, h)
	case http.MethodPut:
		group.PUT(r.Path, h)
	case http.MethodDelete:
		group.DELETE(r.Path, h)
	default:
		panic("web: 未知的 HTTP 方法 " + r.Verb)
	}
}
