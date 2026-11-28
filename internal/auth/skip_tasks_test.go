package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseSkipTasks 两种磁盘形态都要读到顶层 skip_tasks（issue #437）。
// 字段落在 auth/account 之外：手写与插件输出共用同一层级，避免两套 schema。
func TestParseSkipTasks(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "nested true",
			raw:  `{"auth":{"accessToken":"at"},"account":{"uid":"u1"},"skip_tasks":true}`,
			want: true,
		},
		{
			name: "nested absent",
			raw:  `{"auth":{"accessToken":"at"},"account":{"uid":"u1"}}`,
			want: false,
		},
		{
			name: "nested false",
			raw:  `{"auth":{"accessToken":"at"},"account":{"uid":"u1"},"skip_tasks":false}`,
			want: false,
		},
		{
			name: "flat true",
			raw:  `{"accessToken":"at","uid":"u1","skip_tasks":true}`,
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := Parse([]byte(c.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.SkipTasks != c.want {
				t.Errorf("SkipTasks=%v want %v", a.SkipTasks, c.want)
			}
		})
	}
}

// TestSaveAtomicPreservesSkipTasks 刷新 token 会整份重建账号文件；不保留该键
// 就等于把「不参与定时任务」静默清掉，企业账号随即又开始在签到上白打 400。
func TestSaveAtomicPreservesSkipTasks(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "u1", FilePath: fp, SkipTasks: true}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), `"skip_tasks": true`) {
		t.Fatalf("save 丢了 skip_tasks：\n%s", raw)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if !b.SkipTasks {
		t.Error("roundtrip SkipTasks=false want true")
	}
}

// TestSaveAtomicOmitsFalseSkipTasks false/缺省不落键：老文件不该被加上噪音键，
// 面板用「键不存在 = false」作默认值。
func TestSaveAtomicOmitsFalseSkipTasks(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u2.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, UID: "u2", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "skip_tasks") {
		t.Fatalf("false 不应写键：\n%s", raw)
	}
}
