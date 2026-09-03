package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"noteserver/src/bases"

	"github.com/gin-gonic/gin"
)

// OpsAuth 校验运维令牌。
//
// 邮件下发能凭空发放道具，是这个服务里唯一一个"不经过用户身份就能改用户数据"的
// 接口，所以它必须有自己的门禁——用用户的 Bearer 令牌不行，那是另一种身份。
//
// 令牌用**定长时间比较**。普通的 == 会在第一个不同的字节上返回，攻击者据此可以
// 一个字节一个字节地把令牌试出来；subtle.ConstantTimeCompare 无论内容如何都走完
// 全部字节。这在本地是几十纳秒的差别，跨网络本来就淹没在抖动里——但这类防护的
// 代价太低，没有不做的理由。
//
// 令牌为空时**拒绝一切请求**而不是放行。配置漏填是最容易发生的事，
// 而它的后果是任何人都能给任何用户发道具。宁可服务不可用，不可默认敞开。
func OpsAuth(token string) gin.HandlerFunc {
	want := []byte(token)
	return func(ctx *gin.Context) {
		if len(want) == 0 {
			bases.Fail(ctx, http.StatusServiceUnavailable,
				"运维接口未配置令牌（data/server.json 的 ops_token），已禁用")
			return
		}
		got := []byte(strings.TrimSpace(strings.TrimPrefix(ctx.GetHeader("Authorization"), "Bearer ")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			bases.Fail(ctx, http.StatusUnauthorized, "运维令牌不正确")
			return
		}
		ctx.Next()
	}
}
