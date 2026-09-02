package web

import "github.com/gin-gonic/gin"

// DefaultMaxBodyBytes 是请求体的默认上限。
//
// 256KB 不是随手取的整数，它对齐的是 net/http 的 maxPostHandlerReadBytes：
// handler 提前拒掉请求、请求体还没读完时，服务器会替你把剩下的读掉丢弃
// （"drain"），好让这条 keep-alive 连接停在一个干净的消息边界上、能继续复用；
// 但 drain 的工作量由对端决定，所以有上限——剩余超过 256KB 就直接断连接。
//
// 把请求体上限压到正好等于那个额度，中间那段"能收下但拒了就得断连"的灰区就没了：
//
//	≤256KB  收下。就算解析失败也只丢一个请求，连接照常复用。
//	>256KB  MaxBytesReader 当场掐断，服务器不会为一个注定被拒的请求继续缓冲。
//
// 业务的实际上限往往更小，用 WithMaxBodyBytes 覆盖即可。
const DefaultMaxBodyBytes = 256 << 10

// Option 调整路由的行为。
//
// 这两个旋钮之所以要开出来，是因为它们是**业务决定**而不是路由机制：
// 请求体多大算大、错误应答长什么样，每个服务都不一样。把它们写死在库里，
// 库就把使用者的产品决定替他做了。
type Option func(*config)

type config struct {
	maxBodyBytes int64
	writeError   func(ctx *gin.Context, code int, msg string)
}

func newConfig(opts []Option) *config {
	c := &config{
		maxBodyBytes: DefaultMaxBodyBytes,
		writeError:   defaultWriteError,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// WithMaxBodyBytes 设置请求体上限。见 DefaultMaxBodyBytes 的说明。
func WithMaxBodyBytes(n int64) Option {
	return func(c *config) {
		if n > 0 {
			c.maxBodyBytes = n
		}
	}
}

// WithErrorWriter 换掉错误应答的写法。
//
// 传进来的函数必须**自己终止请求**（gin 里就是 AbortWithStatusJSON 之类），
// 否则参数绑定失败之后业务方法还会被调用——那正是这套机制要避免的事。
func WithErrorWriter(fn func(ctx *gin.Context, code int, msg string)) Option {
	return func(c *config) {
		if fn != nil {
			c.writeError = fn
		}
	}
}

// defaultWriteError 写 {"error": "..."}。
//
// 用 AbortWithStatusJSON 而不是 JSON：写完还得阻止后续 handler 继续跑。
func defaultWriteError(ctx *gin.Context, code int, msg string) {
	ctx.AbortWithStatusJSON(code, gin.H{"error": msg})
}
