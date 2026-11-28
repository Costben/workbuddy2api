package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/upstream"
)

// issue #379：面板改 config.json 的 proxies 段后，上游不重启也要能解析新线路；
// 解析失败时保留旧表。
func TestReloadProxyConfig(t *testing.T) {
	upstream.ConfigureProxies(nil)
	t.Cleanup(func() { upstream.ConfigureProxies(nil) })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"proxies":{"route-a":"socks5h://user:pass@a.example:1080"}}`)
	if !reloadProxyConfig(path) {
		t.Fatal("首次热重载失败")
	}
	if names := upstream.RouteNames(); len(names) != 1 || names[0] != "route-a" {
		t.Fatalf("线路表不对：%v", names)
	}
	tr, err := upstream.TransportFor("route-a")
	if err != nil {
		t.Fatal(err)
	}
	u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "api.example"}})
	if err != nil || u == nil || u.Host != "a.example:1080" {
		t.Fatalf("route-a 代理不对：%v %v", u, err)
	}

	// 改地址：必须重建 transport，不能复用旧代理连接池。
	write(`{"proxies":{"route-a":"http://b.example:8080"}}`)
	if !reloadProxyConfig(path) {
		t.Fatal("第二次热重载失败")
	}
	tr2, err := upstream.TransportFor("route-a")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := tr2.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "api.example"}})
	if err != nil || u2 == nil || u2.Host != "b.example:8080" {
		t.Fatalf("改地址后代理没更新：%v %v", u2, err)
	}

	// 坏配置：返回失败，旧线路仍在（不能把正在服务的表清空）。
	write(`{"proxies":`)
	if reloadProxyConfig(path) {
		t.Fatal("坏配置应返回 false")
	}
	if names := upstream.RouteNames(); len(names) != 1 || names[0] != "route-a" {
		t.Fatalf("坏配置把旧线路表清空了：%v", names)
	}
}

// 面板在权重非 100 / 停用线路时会写对象形；上游必须也能读，否则整份配置解析失败。
func TestProxyTableAcceptsObjectAndString(t *testing.T) {
	var cfg struct {
		Proxies ProxyTable `json:"proxies"`
	}
	raw := `{"proxies":{"a":"http://a.example:8080",` +
		`"b":{"url":"socks5h://user:pass@b.example:1080","weight":30,"enabled":false}}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("对象形线路表解析失败：%v", err)
	}
	if got := cfg.Proxies["a"]; got != "http://a.example:8080" {
		t.Errorf("字符串形地址不对：%q", got)
	}
	if got := cfg.Proxies["b"]; got != "socks5h://user:pass@b.example:1080" {
		t.Errorf("对象形地址不对：%q", got)
	}
}
