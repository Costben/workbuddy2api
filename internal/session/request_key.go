package session

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ExtractRequestKey accepts session IDs that survive history compaction.
// Request/turn IDs, user IDs and system prompts are never session identities.
func ExtractRequestKey(header http.Header, body []byte) string {
	for _, name := range []string{"X-Session-ID", "X-Conversation-ID"} {
		if v := strings.TrimSpace(header.Get(name)); v != "" && len(v) <= 256 {
			return v
		}
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		meta, _ := obj["metadata"].(map[string]any)
		for _, container := range []map[string]any{meta, obj} {
			for _, name := range []string{"session_id", "sessionId"} {
				if v := strOrEmpty(container[name]); v != "" {
					return v
				}
			}
		}
	}
	return ExtractKey(body)
}
