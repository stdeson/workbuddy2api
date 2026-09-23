package scheduler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScriptExec 记录命令构建参数并按需模拟执行失败，替代真实 exec 拉起 python3 子进程。
type fakeScriptExec struct {
	lastName string
	lastArgs []string
	lastDir  string
	output   io.Writer
	runN     int
	err      error
}

func (f *fakeScriptExec) SetDir(dir string)     { f.lastDir = dir }
func (f *fakeScriptExec) SetOutput(w io.Writer) { f.output = w }
func (f *fakeScriptExec) Run() error            { f.runN++; return f.err }

// installFakeExec 替换 newScriptCmd，测试结束还原。
// 同时把脚本日志目录指向 t.TempDir()：runScript 会真实打开 <repoRoot>/data/logs/*.log，
// 不隔离就会往工作树里写日志文件（并让"唯一日志文件"类断言互相污染）。
func installFakeExec(t *testing.T) *fakeScriptExec {
	t.Helper()
	t.Setenv("WB2A_SCRIPT_LOG_DIR", t.TempDir())
	f := &fakeScriptExec{}
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		f.lastName, f.lastArgs = name, args
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
	return f
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNextWakeSchoolSlot 开学季任务在 school_hours（默认 12 点）处有独立时点。
func TestNextWakeSchoolSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school 12:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskSchool {
		t.Errorf("kinds=%v want [school]", kinds)
	}
}

// TestNextWakeCatSlot 夜猫子任务在 cat_hours（默认 1 点）处有独立时点。
func TestNextWakeCatSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatHours:          []int{1},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 15, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（cat 01:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCat {
		t.Errorf("kinds=%v want [cat]", kinds)
	}
}

// TestNextWakeSchoolCatDisabled 显式禁用 school/cat 后排程只剩签到时点（互不影响）。
func TestNextWakeSchoolCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		SchoolDisabled:    true,
		CatDisabled:       true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school/cat 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestRunSchoolNowBuildsCommand RunSchoolNow 构造
// python3 scripts/school_open_day_2026.py ALL --run --yes，工作目录设为仓库根。
func TestRunSchoolNowBuildsCommand(t *testing.T) {
	// 防环境泄漏：WB2A_PYTHON 若在测试机已设置会改写 pythonCmd()，使默认值断言失败。
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunSchoolNow()
	if f.lastName != "python3" {
		t.Errorf("name=%q want python3", f.lastName)
	}
	want := []string{"scripts/school_open_day_2026.py", "ALL", "--run", "--yes"}
	if !equalArgs(f.lastArgs, want) {
		t.Errorf("args=%v want %v", f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
	if _, err := os.Stat(filepath.Join(f.lastDir, "scripts", "school_open_day_2026.py")); err != nil {
		t.Errorf("仓库根 %q 内应有 scripts/school_open_day_2026.py: %v", f.lastDir, err)
	}
}

// TestRunCatNowBuildsCommand RunCatNow 构造
// python3 scripts/task_runner.py ALL --yes --only black_cat，工作目录为仓库根。
func TestRunCatNowBuildsCommand(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunCatNow()
	want := []string{"scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"}
	if f.lastName != "python3" || !equalArgs(f.lastArgs, want) {
		t.Errorf("cmd=%s %v want python3 %v", f.lastName, f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
}

// TestDispatchSchoolCatAndFailureWarnsOnly dispatch 把 school/cat 分发给对应脚本；
// 脚本失败只记 WARN（不 panic/不向上抛），且不影响后续任务继续分发。
func TestDispatchSchoolCatAndFailureWarnsOnly(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	f.err = errors.New("boom boom")
	s := New(Config{})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	s.dispatch(context.Background(), taskSchool)
	if f.runN != 1 || f.lastArgs[0] != "scripts/school_open_day_2026.py" {
		t.Errorf("dispatch(school) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	s.dispatch(context.Background(), taskCat)
	if f.runN != 2 || f.lastArgs[0] != "scripts/task_runner.py" {
		t.Errorf("dispatch(cat) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "scripts/school_open_day_2026.py") {
		t.Errorf("school 失败未按 WARN 记录:\n%s", out)
	}
	if !strings.Contains(out, "scripts/task_runner.py") {
		t.Errorf("cat 失败未按 WARN 记录:\n%s", out)
	}
}

// TestPythonCmd WB2A_PYTHON 覆盖解释器名：缺省/空白回落 "python3"（保持
// 容器与既有测试的行为不变），显式设置时取其值（Windows 等仅有 python 的环境）。
func TestPythonCmd(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("default pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "   ")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("blank pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "python")
	if got := pythonCmd(); got != "python" {
		t.Errorf("override pythonCmd()=%q want python", got)
	}

	t.Setenv("WB2A_PYTHON", "  /usr/bin/python3.10  ")
	if got := pythonCmd(); got != "/usr/bin/python3.10" {
		t.Errorf("trim pythonCmd()=%q want /usr/bin/python3.10", got)
	}
}

// TestScriptLogDir WB2A_SCRIPT_LOG_DIR 覆盖脚本日志目录；缺省/空白回落
// <repoRoot>/data/logs（容器内 = 宿主 data 卷，容器重建不丢）。
func TestScriptLogDir(t *testing.T) {
	root := repoRoot()
	want := filepath.Join(root, "data", "logs")

	t.Setenv("WB2A_SCRIPT_LOG_DIR", "")
	if got := scriptLogDir(root); got != want {
		t.Errorf("default scriptLogDir=%q want %q", got, want)
	}

	t.Setenv("WB2A_SCRIPT_LOG_DIR", "   ")
	if got := scriptLogDir(root); got != want {
		t.Errorf("blank scriptLogDir=%q want %q", got, want)
	}

	dir := t.TempDir()
	t.Setenv("WB2A_SCRIPT_LOG_DIR", "  "+dir+"  ")
	if got := scriptLogDir(root); got != dir {
		t.Errorf("override scriptLogDir=%q want %q（应 trim）", got, dir)
	}
}

// TestRunCatNowWritesScriptLog 夜猫子任务把脚本输出落盘到
// <WB2A_SCRIPT_LOG_DIR>/cat-YYYYMMDD.log：命令头 + 结果行 + 子进程接到了 writer。
// 脚本 print 是每号结果的唯一来源，进程日志那行 ok/WARN 不足以对账。
func TestRunCatNowWritesScriptLog(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t) // 已把日志目录隔离到 t.TempDir()
	s := New(Config{})
	s.RunCatNow()

	dir := os.Getenv("WB2A_SCRIPT_LOG_DIR")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log dir %q: %v", dir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("log dir %q 应有 1 个日志文件，实际 %d 个", dir, len(entries))
	}
	if want := "cat-" + time.Now().Format("20060102") + ".log"; entries[0].Name() != want {
		t.Errorf("日志文件名=%q want %q", entries[0].Name(), want)
	}
	if f.output == nil {
		t.Errorf("脚本子进程未接日志 writer（SetOutput 未被调用）")
	}

	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"dir=" + repoRoot(),
		"python3 scripts/task_runner.py ALL --yes --only black_cat",
		"cat: ok",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("脚本日志缺少 %q:\n%s", want, got)
		}
	}
}

// TestRunScriptFailureWritesScriptLog 脚本失败时日志同样落盘（FAILED 结果行），
// 且进程日志的 WARN 行带上日志路径——否则失败原因只剩一个退出码。
func TestRunScriptFailureWritesScriptLog(t *testing.T) {
	f := installFakeExec(t)
	f.err = errors.New("boom boom")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	runScript("cat", repoRoot(), [][]string{
		{"python3", "scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"},
	})

	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "(log: ") {
		t.Errorf("失败 WARN 行应带日志路径:\n%s", out)
	}
	dir := os.Getenv("WB2A_SCRIPT_LOG_DIR")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("log dir %q entries=%v err=%v want 1 file", dir, entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "FAILED: boom boom") {
		t.Errorf("日志缺少失败结果行:\n%s", string(b))
	}
}

// TestRunOneScriptLogUnwritable 日志目录不可建时不影响脚本任务本身：
// 只留一行 WARN，命令照常执行（可观测性增强不得阻断业务）。
func TestRunOneScriptLogUnwritable(t *testing.T) {
	f := installFakeExec(t)
	// 指向一个"父路径是普通文件"的位置：MkdirAll 必然失败。
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_SCRIPT_LOG_DIR", filepath.Join(blocker, "logs"))

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	runScript("cat", repoRoot(), [][]string{{"python3", "scripts/task_runner.py"}})

	if f.runN != 1 {
		t.Errorf("日志不可写时脚本仍应执行: runN=%d want 1", f.runN)
	}
	if f.output != nil {
		t.Errorf("日志打开失败时不应设置 writer")
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "cat: ok") {
		t.Errorf("应同时有目录 WARN 与结果行:\n%s", out)
	}
}
