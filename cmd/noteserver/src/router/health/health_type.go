package health

import "web"

// StatusRequest 探活请求。
//
// path tag 写成 /healthz 而不是按方法名推出的 /status：负载均衡和容器编排
// 探活时习惯打这个路径。它也是唯一一个挂在引擎根上、不带 /api 前缀的接口——
// Init 收的是 gin.IRoutes，引擎本身就实现了它，所以不必为此开特例。
type StatusRequest struct {
	web.GET `path:"/healthz"`
}
