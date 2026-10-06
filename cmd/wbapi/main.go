// Command wbapi 是 WorkBuddy 额度反代工具的入口。
//
// 本程序只做命令分派，具体逻辑在 internal/app。
package main

import (
	"os"

	"workbuddy.local/workbuddy-api/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
