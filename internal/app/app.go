// Package app 组装并运行 wbapi 的各个命令模式。
//
// 设计要点：
//   - 只有一个长期运行的进程（serve）；其余命令跑完即退。
//   - 服务只监听回环地址，绝不对外暴露。
//   - 所有超时/并发上限集中在 limits.go，不散落各处。
package app

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// Version 由构建脚本注入；未注入时为开发版本。
var Version = "dev"

// 命令名常量。main 负责分派，app 负责执行。
const (
	CmdServe   = "serve"
	CmdAuth    = "auth"
	CmdCheckin = "checkin"
	CmdStatus  = "status"
	CmdReport  = "report"
	CmdDoctor  = "doctor"
	CmdVersion = "version"
)

// usage 打印命令帮助。
func usage(w io.Writer) {
	fmt.Fprintf(w, `wbapi —— WorkBuddy 额度反代（自用）

用法:
  wbapi <command> [flags]

命令:
  serve        启动本地反代服务（常驻，仅监听 127.0.0.1）
  auth         账号凭证管理（import/list/remove）
  checkin      执行签到（--due 表示由计划任务触发，只处理到期的）
  status       查看账号状态、额度
  report       生成静态 HTML 用量报告
  doctor       诊断（连通性、凭据、配置）
  version      打印版本

示例:
  wbapi serve
  wbapi serve --addr 127.0.0.1:8787
  wbapi checkin --due
  wbapi doctor
`)
}

// Run 是唯一的入口。args 不含程序名。
func Run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case CmdServe:
		return runServe(rest)
	case CmdVersion, "-v", "--version":
		fmt.Printf("wbapi %s (%s)\n", Version, "workbuddy-only")
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	case CmdAuth, CmdCheckin, CmdStatus, CmdReport, CmdDoctor:
		// 这些命令在 cli.go 实现（见 cli.go）。
		return runCLICommand(cmd, rest)
	default:
		fmt.Fprintf(os.Stderr, "wbapi: 未知命令 %q\n\n", cmd)
		usage(os.Stderr)
		return 2
	}
}

// requireLoopback 校验监听地址必须是回环，避免误暴露到局域网/公网。
//
// 允许的形式：
//
//	127.0.0.1:8787 / localhost:8787 / [::1]:8787 / :8787（空主机视为回环）
//
// 拒绝：0.0.0.0:8787、192.168.x.x:8787、主机名为外部地址等。
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("地址 %q 格式无效（应形如 127.0.0.1:8787）：%w", addr, err)
	}
	if host == "" {
		// ":8787" 会监听全部网卡，必须显式改写为回环
		return fmt.Errorf("地址 %q 未指定主机，会监听全部网卡；请显式写 127.0.0.1:8787", addr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("地址 %q 的主机名 %q 无法确认是回环；请用 127.0.0.1", addr, host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("地址 %q 不是回环地址；本服务只允许监听 127.0.0.1（安全边界，不可配置）", addr)
	}
	return nil
}

// nowFunc 便于测试替换时间源。
var nowFunc = time.Now
