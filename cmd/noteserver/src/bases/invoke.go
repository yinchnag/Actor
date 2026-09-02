package bases

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"actor"
)

// FailInvoke 把框架层的失败翻译成诚实的 HTTP 语义。
//
// 关键在于区分"确定没执行"和"可能已执行"：前者客户端可以放心重试，
// 后者重试就有重复写入的风险，必须如实告诉它。这个区分是 actor 框架
// 特意做出来的（超时即取消），在 HTTP 层把它抹平掉就白费了。
func FailInvoke(ctx *gin.Context, err error, op string) {
	switch {
	case errors.Is(err, actor.ErrTaskCanceled):
		// 等待超时，且任务在被取用之前就已取消——模块方法一次都没执行
		log.Printf("%s 超时（已取消，未执行）: %v", op, err)
		Fail(ctx, http.StatusServiceUnavailable, op+"超时，操作未执行，可以直接重试")

	case errors.Is(err, actor.ErrTaskAwaitTimeout):
		// 同样是超时，但没带取消标记：方法可能正在执行或已执行完
		log.Printf("%s 超时（可能已执行）: %v", op, err)
		Fail(ctx, http.StatusGatewayTimeout, op+"超时，操作可能已生效，重试前请先确认")

	case errors.Is(err, actor.ErrTaskQueueTimeout):
		log.Printf("%s 排队超时: %v", op, err)
		Fail(ctx, http.StatusServiceUnavailable, "服务繁忙，请稍后重试")

	case errors.Is(err, actor.ErrCallCycle):
		// 环形同步调用，属于服务端的编排 bug，不该让客户端重试
		log.Printf("%s 出现跨 actor 调用环，这是服务端 bug: %v", op, err)
		Fail(ctx, http.StatusInternalServerError, "服务器内部错误")

	default:
		// 走到这里的多半是 actor 已关闭（框架未导出该哨兵错误，无法用
		// errors.Is 精确判定），或存储返回的错误。统一按暂时不可用处理。
		log.Printf("%s 失败: %v", op, err)
		Fail(ctx, http.StatusServiceUnavailable, op+"失败，请稍后重试")
	}
}
