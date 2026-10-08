package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════════
// 护栏：启动路径**绝不**自动迁移用户主目录里的旧数据（2026-10-09）
// ═══════════════════════════════════════════════════════════════════════
//
// 🔴 委托方原话：
//
//	「程序在空目录首次运行会从 %USERPROFILE%\.wbapi 自动迁移数据，
//	  这个删掉。」
//
// 为什么这是**产品决策**而不是洁癖（不要"好心"加回去）：
//
//	自动迁移 = "程序会去翻用户主目录里的其他位置，并把凭据复制到
//	当前目录"。对一个要发布给别人用的程序，这是**不该有的副作用** ——
//	用户只是想在某个文件夹里跑一下，程序却悄悄动了他主目录下的东西。
//	即便只是复制（不改源），也超出了用户点"运行"时给出的授权范围。
//
// ⚠️ 它还制造过一个反直觉现象（发布前实测踩到）：
//	在"全新的空目录"里首次运行，`auths/accounts.json` 会**立刻出现
//	并含真实账号** —— 与"这是个干净的新环境"的直觉相反。
//	最坏的后果是：把这种目录当成"测试环境"打包/截图/上传时**带走真实凭据**。

// TestStartupNeverAutoMigrates 守：启动路径不再调用 datadir.Migrate。
//
// 🔴 用**源码审查**而不是行为测试，理由：
//
//	行为测试要构造"旧目录有数据 + 新目录为空"的环境，那需要真的去
//	写 %USERPROFILE%\.wbapi —— 而本项目的纪律是**绝不拿真实主目录做测试**
//	（第 56 轮缺陷⑥就是这么把用户真实注册表写坏的，那次教训是
//	"给程序加自动化副作用逻辑时，必须问：这段代码在测试环境里
//	会不会动到用户的真实资源"）。
//
//	⇒ 改为断言"启动文件里不再出现 Migrate 调用"，零副作用且精确。
//
// 反向对照：把 `if res := datadir.Migrate(); res.Performed {` 加回 serve.go，
// 本测试立刻红。
func TestStartupNeverAutoMigrates(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatalf("读 serve.go 失败: %v", err)
	}
	code := string(src)

	// 剥掉注释再检查 —— 注释里会提到 Migrate（说明为什么删掉），
	// 那是资产，不该被判为违规。
	// （同型坑：第 56 轮 TestBuildScriptProducesGUISubsystem。）
	stripped := stripGoComments(code)

	if strings.Contains(stripped, "datadir.Migrate()") {
		t.Error("serve.go 的启动路径仍在调用 datadir.Migrate() —— " +
			"委托方要求删除自动迁移（它会去翻用户主目录）")
	}

	// 另一种写法也要拦住：先赋值再判断。
	if strings.Contains(stripped, "MigrationNeeded()") {
		t.Error("serve.go 仍在检查 MigrationNeeded() —— 启动路径不该碰旧目录")
	}
}

// TestMigrateKeptButNotCalledAtStartup 记录"实现还在、只是不自动跑"。
//
// 🔴 这是**如实记录设计状态**，不是期望契约：
//
//	我们**没有删掉** datadir.Migrate 的实现与它那 7 条安全测试 ——
//	迁移逻辑本身写得很谨慎（复制不移动、绝不覆盖、不解析 JSON），
//	将来若用户明确要求"帮我搬一下数据"，它还是有用的。
//	本次删的只是"启动时自动触发"这一个行为。
func TestMigrateKeptButNotCalledAtStartup(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "datadir", "migrate.go")); err != nil {
		t.Errorf("datadir/migrate.go 不该被删除（实现保留，只是不再自动调用）: %v", err)
	}
}

// stripGoComments 去掉 Go 源码里的行注释与块注释。
//
// ⚠️ 不是完整词法分析器（不处理注释符出现在字符串/字符字面量里的情形）。
// 对本用途足够：要检查的标识符（datadir.Migrate）不会出现在含 `//` 的
// 字符串里。若将来出现，本函数的结论需重新评估。
func stripGoComments(src string) string {
	var b strings.Builder
	inBlock := false
	for i := 0; i < len(src); {
		if inBlock {
			end := strings.Index(src[i:], "*/")
			if end < 0 {
				break
			}
			i += end + 2
			inBlock = false
			continue
		}
		// 行注释
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
			nl := strings.IndexByte(src[i:], '\n')
			if nl < 0 {
				break
			}
			i += nl
			continue
		}
		// 块注释
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			i += 2
			inBlock = true
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}
