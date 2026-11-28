package upstream

// 出口线路配置错误的可见性（issue #379）。
//
// 账号绑定的线路名在上游线路表里查不到时，ChatStreamContext 会在发请求**之前**
// 直接返回错误（设计上不静默直连）。handler 把这种无信封错误当传输层抖动处理，
// 只换号、不打印——整池都绑同一条错线路时，用户看到的是 503，容器日志里却
// 一条 ERROR 都没有。这里补一条去重后的日志：同一个错误只打一次，避免整池
// 轮转刷屏，又能让排障的人一眼看到「线路名没解析出来」。

import (
	"log"
	"sync"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

var routeErrLog struct {
	mu   sync.Mutex
	last string
}

// logRouteConfigError 记录一次出口线路配置错误（同一错误文本只记一次）。
func logRouteConfigError(a *auth.Auth, err error) {
	if err == nil {
		return
	}
	key := err.Error()
	routeErrLog.mu.Lock()
	if routeErrLog.last == key {
		routeErrLog.mu.Unlock()
		return
	}
	routeErrLog.last = key
	routeErrLog.mu.Unlock()
	label := ""
	if a != nil {
		label = logfmt.Label(a.UID, a.Nickname) + " "
	}
	log.Printf("ERR: [upstream] 出口线路配置错误，本次请求未发出 acct=%s: %v", label, err)
}
