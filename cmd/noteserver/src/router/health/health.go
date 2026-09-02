// Package health 是探活接口。
package health

import (
	"net/http"

	"noteserver/src/bases"

	"github.com/gin-gonic/gin"

	"web"
)

// Health 持有路由需要的依赖。
type Health struct {
	web.Router[*Health]
	hub service
}

// service 是 Health 用到的 Hub 能力。
type service interface {
	OnlineUsers() int
	DiscardedErrors() uint64
}

// StatusRequest 探活请求。
//
// path tag 写成 /healthz 而不是按方法名推出的 /status：负载均衡和容器编排
// 探活时习惯打这个路径。它也是唯一一个挂在引擎根上、不带 /api 前缀的接口——
// Init 收的是 gin.IRoutes，引擎本身就实现了它，所以不必为此开特例。
type StatusRequest struct {
	web.GET `path:"/healthz"`
}

// New 建路由并挂到 group 上。
func New(group gin.IRoutes, hub service) *Health {
	h := &Health{hub: hub}
	h.Init(group, bases.RouterOpts()...)
	return h
}

// Status 报当前状态。→ GET /healthz
func (that *Health) Status(_ *StatusRequest, ctx *gin.Context) {
	// 两个数都值得盯：online_users 等于当前存活的用户事件循环数，
	// 是最直观的容量指标；discarded 正常应当恒为 0，一旦开始涨，
	// 说明有无返回值的调用在失败——那类失败没有调用方能接，
	// 不在这里露头就彻底看不见了。
	bases.JSON(ctx, http.StatusOK, gin.H{
		"online_users": that.hub.OnlineUsers(),
		"discarded":    that.hub.DiscardedErrors(),
	})
}
