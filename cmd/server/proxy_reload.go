package main

// config.json 的 proxies 段热重载（issue #379）。
//
// 面板「设置 → 代理」把线路表写进上游 config.json；上游此前只在启动时
// ConfigureProxies 一次。于是「加线路 → 点测试（面板发出）→ 请求模型」这条最常见的
// 操作里，测试是通的、聊天却报错：线路名不在启动快照里，上游按设计明确失败
// （不静默直连），而错误路径几乎不留日志。重启容器能解决，但每改一次线路都要求
// 用户记得重启，不该是配置的默认成本。
//
// 这里与 sanitize_reload.go 同一套做法：轮询 config.json 的 mtime+size，变了就
// 重新 Load 并整体替换线路表（ConfigureProxies 幂等，并会清空按线路缓存的连接池，
// 地址改了不会复用旧代理连接）。解析失败只记 WARN、保留旧表——一次手误不能把
// 正在服务的出口打空。

import (
	"log"
	"os"
	"time"

	"workbuddy2api/internal/upstream"
)

// proxyWatchInterval 线路表轮询间隔：面板保存后最多这么久生效。与 auths 热加载
// （5s）同量级；这段时间旧表仍在服务，不会因为等待热加载而中断在途请求。
const proxyWatchInterval = 5 * time.Second

// reloadProxyConfig 读一次配置并注入线路表；返回是否成功。
func reloadProxyConfig(path string) bool {
	cfg, err := Load(path)
	if err != nil {
		log.Printf("WARN: 出口线路表热重载跳过（配置读取失败）：%v", err)
		return false
	}
	upstream.ConfigureProxies(cfg.Proxies)
	names := upstream.RouteNames()
	if len(names) > 0 {
		log.Printf("出口线路表已热重载：%d 条（%v）", len(names), names)
	} else {
		log.Printf("出口线路表已热重载：未配置线路（config.json 的 proxies 段）")
	}
	return true
}

// watchProxyConfig 后台轮询配置文件；stop 关闭时退出。
func watchProxyConfig(path string, stop <-chan struct{}) {
	if path == "" {
		return
	}
	stamp := func() (int64, int64) {
		st, err := os.Stat(path)
		if err != nil {
			return 0, 0
		}
		return st.ModTime().UnixNano(), st.Size()
	}
	lastMod, lastSize := stamp()
	ticker := time.NewTicker(proxyWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			mod, size := stamp()
			if mod == 0 {
				continue // 原子替换的间隙：下轮再看
			}
			if mod == lastMod && size == lastSize {
				continue
			}
			lastMod, lastSize = mod, size
			reloadProxyConfig(path)
		}
	}
}
