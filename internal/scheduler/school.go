// school.go 开学季任务与夜猫子任务的脚本类排程：从系统 crontab 迁入 Go scheduler。
//
// 背景：school（12:00）与 cat（01:00 夜猫窗口）原由系统 crontab 调
// scripts/school_open_day_cron.sh 执行——依赖外部系统 cron、容器重建可能丢失、
// 不在 config 里配置。迁入后成为第五、第六类任务，时点由 schedule.school_hours /
// schedule.cat_hours 配置，school_open_day_cron.sh 保留为手动触发入口。
package scheduler

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// repoRoot 定位仓库根（容器内 /app、宿主 /root/workbuddy2api）。
// 策略：从当前工作目录逐级向上找 scripts/school_open_day_2026.py，
// 找不到回落 os.Getwd()（此时 Run 会因脚本缺失打 WARN，不 panic）。
// 注意：Go scheduler 在 cmd/server 内以工作目录启动（容器 WORKDIR /app），
// 若进程以别的工作目录拉起（如 systemd/裸 binary），上溯穷尽后仍以
// os.Getwd() 兜底，把缺失暴露成 WARN 而非静默。
func repoRoot() string {
	start, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "school_open_day_2026.py")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起 python3。
type scriptRunner interface {
	SetDir(string)
	// SetOutput 指定脚本 stdout/stderr 的落盘目标；nil 表示丢弃（等同改动前行为）。
	SetOutput(io.Writer)
	Run() error
}

// scriptCmd exec.Cmd 适配器：把 exec.Cmd 的 Dir 字段包装成 SetDir 方法，
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。
type scriptCmd struct{ cmd *exec.Cmd }

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }

// SetOutput 把子进程的 stdout/stderr 指向日志文件：exec 直接把 fd 交给子进程，
// 日志文件以 O_APPEND 打开，多路写入的追加语义由内核保证。
func (c *scriptCmd) SetOutput(w io.Writer) {
	c.cmd.Stdout = w
	c.cmd.Stderr = w
}

func (c *scriptCmd) Run() error { return c.cmd.Run() }

// newScriptCmd 构建脚本子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
// 工作目录由调用方 SetDir 显式设置仓库根。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	return &scriptCmd{cmd: exec.Command(program, args...)}
}

// pythonCmd 返回执行 scripts/*.py 的解释器名。
//
// 默认 "python3"，与容器/Linux 现状完全一致，行为零变更；WB2A_PYTHON
// 显式指定时优先，供解释器不叫 python3 的环境使用（命名对齐仓库 Go 侧
// WB2A_* env 约定，如 WB2A_AUTH_DIR / WB2A_LISTEN）。
//
// 需要该开关的原因：Windows 官方安装器只提供 python.exe，且 PATH 上常存在
// Microsoft Store 的 python3.exe App Execution Alias 存根——exec.Command 能找到
// 它却无法真正执行，脚本类任务统一报 `exit status 9009`。
// 设 WB2A_PYTHON=python 即可绕过。
func pythonCmd() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return v
	}
	return "python3"
}

// scriptLogDir 返回脚本输出的落地目录。
//
// 默认 <repoRoot>/data/logs：容器内 /app/data 已 bind-mount 到宿主
// /root/code/workbuddy2api/data（见 docker-compose.yml），容器重建不丢日志；
// 且该路径不在版本库里（*.log 也被 .gitignore 覆盖），不会污染工作树。
// WB2A_SCRIPT_LOG_DIR 显式指定时优先（测试指向 t.TempDir()，不往仓库写）。
func scriptLogDir(root string) string {
	if v := strings.TrimSpace(os.Getenv("WB2A_SCRIPT_LOG_DIR")); v != "" {
		return v
	}
	return filepath.Join(root, "data", "logs")
}

// scriptLogPath 返回当天日志文件路径 <dir>/<name>-YYYYMMDD.log（本地时区，追加写）。
// 按天分文件：cat 每天 3 次、school 每天 1 次，单文件不会无限膨胀。
func scriptLogPath(name, root string) string {
	return filepath.Join(scriptLogDir(root), name+"-"+time.Now().Format("20060102")+".log")
}

// openScriptLog 以追加方式打开当天日志；失败返回 nil 并留一行 WARN——
// 日志落盘属于可观测性增强，绝不能因为目录不可写而阻断脚本任务本身。
func openScriptLog(name, root string) *os.File {
	path := scriptLogPath(name, root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("WARN: script log dir %s: %v", filepath.Dir(path), err)
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("WARN: script log %s: %v", path, err)
		return nil
	}
	return f
}

// logHint 返回进程日志尾部的日志文件路径提示；未落盘时为空串。
func logHint(name, root string, f *os.File) string {
	if f == nil {
		return ""
	}
	return " (log: " + scriptLogPath(name, root) + ")"
}

// runScript 依次执行若干脚本命令：任一命令失败只记一行 WARN，不向上抛、
// 不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
func runScript(name, root string, commands [][]string) {
	for _, cmdArgs := range commands {
		runOneScript(name, root, cmdArgs)
	}
}

// runOneScript 执行单条脚本命令，并把该命令的 stdout/stderr 追加落盘到
// <repoRoot>/data/logs/<name>-YYYYMMDD.log，文件里另写首尾两行（开始时间+命令 /
// 结束时间+结果）。
//
// 这样做的原因：脚本自身的 print 是"每号任务结果"的唯一来源（哪一号领到、
// 哪一号因能量不足没点亮），而进程日志只留一行 ok/WARN——只凭退出码无法对账，
// 出问题也没法回溯。日志写不进去时不改变任务行为（见 openScriptLog）。
func runOneScript(name, root string, cmdArgs []string) {
	c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
	c.SetDir(root)

	f := openScriptLog(name, root)
	if f != nil {
		defer f.Close()
		c.SetOutput(f)
		fmt.Fprintf(f, "\n===== %s | %s | dir=%s =====\n",
			time.Now().Format(time.RFC3339), strings.Join(cmdArgs, " "), root)
	}

	if err := c.Run(); err != nil {
		log.Printf("WARN: %s (%s): %v%s", name, cmdArgs[1], err, logHint(name, root, f))
		if f != nil {
			fmt.Fprintf(f, "===== %s | %s: FAILED: %v =====\n",
				time.Now().Format(time.RFC3339), name, err)
		}
		return
	}
	log.Printf("%s: ok (%s)%s", name, cmdArgs[1], logHint(name, root, f))
	if f != nil {
		fmt.Fprintf(f, "===== %s | %s: ok =====\n", time.Now().Format(time.RFC3339), name)
	}
}

// RunSchoolNow 立即执行开学季任务：school_open_day_2026.py ALL --run --yes。
// 全量跑任务点亮 + 领奖 + 自动抽空抽奖余额。活动下线（in_period=false）时脚本
// 各段全量跳过、正常退出，不视为失败。失败只记 WARN。
func (s *Scheduler) RunSchoolNow() {
	root := repoRoot()
	runScript("school", root, [][]string{
		{pythonCmd(), "scripts/school_open_day_2026.py", "ALL", "--run", "--yes"},
	})
}

// RunCatNow 立即执行夜猫子任务：task_runner.py ALL --yes --only black_cat。
// black_cat 时段敏感：夜猫窗口 23:00–08:00 CST 内最多补 1 次（task_runner 内部
// 判定，非窗口期打印 skip 正常退出）。失败只记 WARN。
func (s *Scheduler) RunCatNow() {
	root := repoRoot()
	runScript("cat", root, [][]string{
		{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"},
	})
}
