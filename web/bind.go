package web

import (
	"encoding/json"
	"net/http"
	"reflect"

	"github.com/gin-gonic/gin"
)

// IRequest 是请求类型完全接管参数解析时实现的接口。
//
// 返回 false 表示解析失败且**已经写好应答**，框架不再调用业务方法。
// 返回值不能省：没有它的话，解析失败时业务方法照样会被调用，
// 拿到一个半填充的参数——那种 bug 只会在线上以奇怪的方式冒出来。
type IRequest interface {
	FromWithContext(ctx *gin.Context) bool
}

// BindJSON 解析 JSON 请求体。失败时应答已经写好，调用方直接 return 即可。
//
// 两处收紧超出了 gin 的默认行为，都是有意的：
//
//   - MaxBytesReader 限制体积，免得有人拿超大 body 打内存，
//     也让"提前拒绝"不至于毁掉 keep-alive 连接（见 DefaultMaxBodyBytes）；
//   - DisallowUnknownFields 让拼错的字段名当场报错，而不是被静默忽略之后
//     变成一个"我明明传了 content 怎么说我没传"的工单。
//
// 用标准库的 encoding/json 而不是 gin 的 ShouldBindJSON：后者没有开
// DisallowUnknownFields 的入口。
func BindJSON(ctx *gin.Context, dst any, opts ...Option) bool {
	return newConfig(opts).bindJSON(ctx, dst)
}

func (that *config) bindJSON(ctx *gin.Context, dst any) bool {
	ctx.Request.Body = http.MaxBytesReader(ctx.Writer, ctx.Request.Body, that.maxBodyBytes)
	dec := json.NewDecoder(ctx.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		that.writeError(ctx, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

// bindRequest 造一个请求对象并填好参数。
func (that *routeHandler) bindRequest(ctx *gin.Context, c *config) (reflect.Value, bool) {
	ptr := reflect.New(that.ReqType)

	switch {
	case that.Custom:
		// 请求类型自己接管：解析失败时它负责写应答
		return ptr, ptr.Interface().(IRequest).FromWithContext(ctx)

	case !that.NeedsBind:
		// 只有动词标记，没有参数要填
		return ptr, true

	case bodyVerbs[that.Verb]:
		return ptr, c.bindJSON(ctx, ptr.Interface())

	default:
		// GET / DELETE 从 query 取，按字段上的 form tag 匹配
		if err := ctx.ShouldBindQuery(ptr.Interface()); err != nil {
			c.writeError(ctx, http.StatusBadRequest, "查询参数不正确: "+err.Error())
			return ptr, false
		}
		return ptr, true
	}
}
