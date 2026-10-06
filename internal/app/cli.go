// cli.go：一次性命令（auth / checkin / status / report / doctor）。
//
// serve 之外的命令都走这里：跑完即退，不常驻。
// 每个命令独占一个函数，互不共享状态 —— 失败互不连累。
package app

import (
	"fmt"
	"os"
)

// runCLICommand 分派一次性命令。
func runCLICommand(cmd string, args []string) int {
	switch cmd {
	case CmdAuth:
		return runAuth(args)
	case CmdCheckin:
		return runCheckinCmd(args)
	case CmdStatus:
		return runStatusCmd(args)
	case CmdReport:
		return runReportCmd(args)
	case CmdDoctor:
		return runDoctorCmd(args)
	}
	fmt.Fprintf(os.Stderr, "wbapi: 未知命令 %q\n", cmd)
	return 2
}
