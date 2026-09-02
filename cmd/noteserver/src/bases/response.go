package bases

import "github.com/gin-gonic/gin"

// JSON 写一条成功应答。
func JSON(ctx *gin.Context, code int, payload any) {
	ctx.JSON(code, payload)
}

// Fail 写一条错误应答，并中止后续中间件。
//
// 用 AbortWithStatusJSON 而不是 JSON：鉴权中间件里写完错误还得阻止
// 业务处理器继续跑，两处用同一个函数才不会漏掉那个 Abort。
func Fail(ctx *gin.Context, code int, msg string) {
	ctx.AbortWithStatusJSON(code, gin.H{"error": msg})
}
