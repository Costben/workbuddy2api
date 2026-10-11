package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCacheFixClientUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		want        float64
	}{
		{"zero_masks_details", `{"prompt_tokens":700,"completion_tokens":1,"cached_tokens":0,"prompt_tokens_details":{"cached_tokens":640,"audio_tokens":3}}`, 640},
		{"provider_hit_only", `{"prompt_tokens":200000,"completion_tokens":1,"cached_tokens":0,"prompt_cache_hit_tokens":192000,"prompt_cache_miss_tokens":8000,"credit":0.02}`, 192000},
		{"standard_details", `{"prompt_tokens":700,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":640}}`, 640},
		{"top_level_hit", `{"prompt_tokens":700,"completion_tokens":1,"cached_tokens":640}`, 640},
		{"real_miss", `{"prompt_tokens":700,"completion_tokens":1,"cached_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"id":"chatcmpl-fixture","model":"deepseek-flash","choices":[],"usage":` + tc.usage + `}`
			sse := "data: " + frame + "\r\n\r\ndata: [DONE]\r\n\r\n"
			rr := httptest.NewRecorder()
			if err := Stream(rr, iotest.OneByteReader(strings.NewReader(sse))); err != nil {
				t.Fatal(err)
			}
			if strings.Count(rr.Body.String(), "data: [DONE]") != 1 {
				t.Fatal("DONE count changed")
			}
			var out map[string]any
			line := strings.Split(rr.Body.String(), "\n")[0]
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &out); err != nil {
				t.Fatal(err)
			}
			u := out["usage"].(map[string]any)
			// Mirrors the actual JS nullish-coalescing precedence at the client.
			cached, exists := u["cached_tokens"]
			if !exists || cached == nil {
				cached = u["prompt_tokens_details"].(map[string]any)["cached_tokens"]
			}
			if cached != tc.want {
				t.Fatalf("client inputCacheRead=%v want %v", cached, tc.want)
			}
			agg, err := Aggregate(iotest.OneByteReader(strings.NewReader(sse)))
			if err != nil {
				t.Fatal(err)
			}
			au := agg["usage"].(map[string]any)
			if au["cached_tokens"] != tc.want {
				t.Fatalf("aggregate cached=%v want %v", au["cached_tokens"], tc.want)
			}
			var original map[string]any
			_ = json.Unmarshal([]byte(tc.usage), &original)
			for _, field := range []string{"prompt_tokens", "completion_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens", "credit"} {
				if !reflect.DeepEqual(original[field], u[field]) {
					t.Fatalf("changed %s", field)
				}
			}
		})
	}
}

func TestCacheFixUsageDoesNotMutateInput(t *testing.T) {
	var obj map[string]any
	_ = json.Unmarshal([]byte(`{"usage":{"cached_tokens":0,"prompt_cache_hit_tokens":640,"prompt_tokens_details":{"audio_tokens":3}}}`), &obj)
	before, _ := json.Marshal(obj)
	_ = normalizeFrame(obj)
	after, _ := json.Marshal(obj)
	if string(before) != string(after) {
		t.Fatal("upstream usage mutated")
	}
}
