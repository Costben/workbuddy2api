// cache_key.go 注入上游 prompt_cache_key 字段（P0 费用优化）。
//
// 逆向实测（buddy-adapter.ts:706-714）：上游服务端支持 prompt_cache_key，
// 同一段 8k token 前缀：
//   - 不带 → prompt_cache_hit_tokens=0, credit≈0.34
//   - 带   → prompt_cache_hit_tokens=7808, credit≈0.02（费用降 ~17×）
//
// 网关在此为每个出站 chat 请求注入一个稳定、按账号隔离的 cache key，
// 让同一客户端对同一账号的连续请求复用上游前缀缓存。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// InjectPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. handler 已解析稳定会话源 → 用入站 conversationID 参数做会话哈希源
//  3. 参数为空 → 用 body 的 conversation_id / conversationId
//
// 安全约束——按账号隔离：
//   - 生成键格式 `wb2a-<uid8>-<convHex>`
//   - uid8 仅供辨识；会话段的哈希包含完整 UID，保证不同账号使用不同命名空间
//   - 相同的缓存键不会让不同账号共享上游缓存；上游仍按账号 UID 隔离
//
// 入参 uid 为账号 UID（空则用 "-"，但仍会注入键；调用方应保证传真实 UID）。
// 入参 conversationID 为网关解析出的稳定会话标识，账号粘性与缓存键共用这个源。
// 两源都空时 convHex 由 UID 单独派生，同账号得到同一个键；是否命中仍取决于内容前缀。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不二次错误化）。
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// 优先级 1：客户端已显式带 key → 绝不覆盖。
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	// 使用 handler 已解析的会话标识，使账号绑定与缓存键共享同一会话源。
	// 独立调用时才回落 body 的 conversation_id / conversationId。
	conv := conversationID
	if conv == "" {
		if v := strField(obj, "conversation_id"); v != "" {
			conv = v
		} else if v := strField(obj, "conversationId"); v != "" {
			conv = v
		}
	}
	key := buildCacheKey(uid, conv)
	obj["prompt_cache_key"] = key
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildCacheKey 生成 `wb2a-<uid8>-<convHex>` 格式的稳定 cache key。
//
// uid8 提供账号隔离段；convHex = sha256(uid + conversationID)[:16] 的 hex 提供会话段
// （同账号同会话稳定、不同会话不同）。会话源为空时 convHex 仍由 uid 单独哈希，
// 保持账号命名空间；同账号的空会话源会产生相同键。
func buildCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	convHex := hex.EncodeToString(sum[:16])
	return "wb2a-" + uid8 + "-" + convHex
}

// strField 从 map 取 string 字段，非 string 或空串返回 ""。
func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
