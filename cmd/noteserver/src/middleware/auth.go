// Package middleware 放 gin 中间件。
package middleware

import (
	"errors"
	"net/http"
	"strings"

	"noteserver/src/bases"
	"noteserver/src/contract"
	"noteserver/src/logs"

	"github.com/gin-gonic/gin"
)

// CtxUID 是鉴权通过后写进 gin.Context 的键名。
const CtxUID = "uid"

// Auth 校验 Authorization: Bearer <token>，把 UID 放进 Context。
//
// 会话校验不绕 actor：它是纯读，没有需要串行化的状态，
// 投递进事件循环只是白搭一次排队，还会让每个带鉴权的请求都
// 占用某个 actor 的时间片。
func Auth(sessions contract.ISessionStore) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		token := strings.TrimSpace(strings.TrimPrefix(ctx.GetHeader("Authorization"), "Bearer "))
		if token == "" {
			bases.Fail(ctx, http.StatusUnauthorized, "缺少 Authorization: Bearer <token>")
			return
		}

		uid, err := sessions.Get(token)
		if errors.Is(err, contract.ErrNoSession) {
			bases.Fail(ctx, http.StatusUnauthorized, "登录已过期，请重新登录")
			return
		}
		if err != nil {
			logs.Warnf("读会话失败: %v", err)
			bases.Fail(ctx, http.StatusServiceUnavailable, "会话服务暂时不可用")
			return
		}

		ctx.Set(CtxUID, uid)
		ctx.Next()
	}
}

// UID 取出鉴权中间件放进去的 UID。只在 Auth 之后的处理器里调用。
func UID(ctx *gin.Context) string {
	v, _ := ctx.Get(CtxUID)
	uid, _ := v.(string)
	return uid
}
