package main

// 线路表的 JSON 兼容：值可以是地址字符串，也可以是面板「设置 → 代理」写出的
// {url, weight, enabled} 对象（issue #379）。
//
// 面板侧的 weight / enabled 是**池**的调度参数，只有面板自己用；上游只认地址。
// 但面板在权重非 100 或停用线路时会写成对象形，上游原来的 map[string]string
// 解析会直接失败：整份 config.json 读不进来，启动时上游起不来，热重载时也只能
// 跳过。这里兼容两种写法：对象只取 url，其余键忽略。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProxyTable 线路名 → 地址；JSON 值兼容字符串与 {url,...} 对象。
type ProxyTable map[string]string

func (t *ProxyTable) UnmarshalJSON(raw []byte) error {
	var table map[string]json.RawMessage
	if err := json.Unmarshal(raw, &table); err != nil {
		return err
	}
	out := make(ProxyTable, len(table))
	for name, value := range table {
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			out[name] = strings.TrimSpace(text)
			continue
		}
		var obj struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(value, &obj); err != nil {
			return fmt.Errorf("proxies[%s]: %w", name, err)
		}
		out[name] = strings.TrimSpace(obj.URL)
	}
	*t = out
	return nil
}
