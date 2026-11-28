package scheduler

// skip_tasks（账号级「只转发、不跑定时任务」, issue #437）回归：
//   · 账号被标记后**仍在号池里**，选号照常能选中它（转发不受影响）；
//   · 签到 / 保活两类任务逐号跳过，不向上游发任何请求；
//   · 开学季 / 夜猫子这类脚本任务只把未被标记的账号传给脚本；
//   · 缺省 / false 时行为与引入前逐字一致（由既有全量测试覆盖）。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestSkipTasksAccountRemainsSelectable 号池选号**不看**这个字段：
// 标记只影响定时任务，转发必须照常。池里只有这一个号时 Pick 必须选中它。
func TestSkipTasksAccountRemainsSelectable(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{
		UID: "skip-only", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(), SkipTasks: true,
	})
	picked := p.Pick("")
	if picked == nil {
		t.Fatal("skip_tasks 账号被选号排除：转发会受影响")
	}
	if picked.UID != "skip-only" {
		t.Fatalf("picked=%q want skip-only", picked.UID)
	}
}

// TestSkipTasksAccountSkippedByCheckin 签到循环跳过标记账号：它不进
// /daily-checkin，也不做余额查询；正常账号仍照常签到。
func TestSkipTasksAccountSkippedByCheckin(t *testing.T) {
	var checkins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			checkins.Add(1)
			_, _ = io.WriteString(w, `{"code":0}`)
			return
		}
		// 余额查询（只读）正常返回，避免 404 触发 fallback 的第二次请求。
		// 外层信封必须带 code=0：doJSON 解信封后只把 data 交给调用方。
		_, _ = io.WriteString(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":100}]}}}}`)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "normal", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "skip", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(), SkipTasks: true})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	byUID := map[string]CheckinOutcome{}
	for _, row := range out {
		byUID[row.UID] = row
	}
	if row, ok := byUID["skip"]; !ok {
		t.Fatal("skip 账号没有出现在签到回执里（手动触发时用户看不到它被跳过）")
	} else if row.Status != CheckinSkipped || row.Detail != "skip_tasks" {
		t.Errorf("skip row=%+v want status=skipped detail=skip_tasks", row)
	}
	if row, ok := byUID["normal"]; !ok || row.Status != CheckinOK {
		t.Errorf("normal row=%+v want status=ok", row)
	}
	if n := checkins.Load(); n != 1 {
		t.Errorf("daily-checkin calls=%d want 1（只有 normal 账号）", n)
	}
}

// TestSkipTasksAccountSkippedByKeepalive 保活循环同样跳过：即使令牌已临近
// 过期、按需刷新判定会命中，也不得对标记账号发起 /token/refresh。
func TestSkipTasksAccountSkippedByKeepalive(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			refreshes.Add(1)
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	normalPath := filepath.Join(dir, "workbuddy-normal.json")
	skipPath := filepath.Join(dir, "workbuddy-skip.json")
	p := pool.New("")
	p.Add(&auth.Auth{UID: "normal", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Minute).Unix(), FilePath: normalPath})
	p.Add(&auth.Auth{UID: "skip", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Minute).Unix(), FilePath: skipPath, SkipTasks: true})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunKeepaliveNow()

	if n := refreshes.Load(); n != 1 {
		t.Errorf("token refresh calls=%d want 1（只有 normal 账号）", n)
	}
	if _, err := os.Stat(normalPath); err != nil {
		t.Errorf("normal 账号刷新后应写回文件: %v", err)
	}
	if _, err := os.Stat(skipPath); !os.IsNotExist(err) {
		t.Errorf("skip 账号不应被刷新/写回（文件不应存在），err=%v", err)
	}
}

// TestScriptTasksPassOnlyTaskAccounts 开学季 / 夜猫子不再以 ALL 扫目录，而是
// 把过滤后的账号文件名显式传给脚本 —— 两个脚本的 load_auth 都接受该形态。
func TestScriptTasksPassOnlyTaskAccounts(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	dir := t.TempDir()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "keep", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		FilePath:  filepath.Join(dir, "workbuddy-keep.json")})
	p.Add(&auth.Auth{UID: "skip", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		FilePath:  filepath.Join(dir, "workbuddy-skip.json"), SkipTasks: true})
	s := New(Config{Pool: p})

	s.RunSchoolNow()
	wantSchool := []string{"scripts/school_open_day_2026.py", "workbuddy-keep.json", "--run", "--yes"}
	if !equalArgs(f.lastArgs, wantSchool) {
		t.Errorf("school args=%v want %v", f.lastArgs, wantSchool)
	}

	s.RunCatNow()
	wantCat := []string{"scripts/task_runner.py", "workbuddy-keep.json", "--yes", "--only", "black_cat"}
	if !equalArgs(f.lastArgs, wantCat) {
		t.Errorf("cat args=%v want %v", f.lastArgs, wantCat)
	}
	if f.runN != 2 {
		t.Errorf("script runs=%d want 2", f.runN)
	}
}

// TestScriptTasksSkipEntirelyWhenAllSkip 全部账号都标记 skip_tasks 时不再拉起
// 脚本（否则脚本拿到空账号列表会以非零码退出并刷 WARN）。
func TestScriptTasksSkipEntirelyWhenAllSkip(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "skip", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		FilePath:  filepath.Join(t.TempDir(), "workbuddy-skip.json"), SkipTasks: true})
	s := New(Config{Pool: p})

	s.RunSchoolNow()
	s.RunCatNow()
	if f.runN != 0 {
		t.Errorf("全部 skip_tasks 仍拉起了脚本（runN=%d）", f.runN)
	}
}
