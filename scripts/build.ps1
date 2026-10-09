# 离线构建脚本 —— 产出带版本号的 taoapi.exe
#
# 用法：
#   .\scripts\build.ps1                 # 版本号默认 dev
#   .\scripts\build.ps1 -Version 0.1.0  # 发布用
#   .\scripts\build.ps1 -SkipTests      # 只构建（不推荐，发布前别用）
#
# 为什么需要它：
#   1. 版本号通过 -ldflags -X 注入 app.Version，否则 `wbapi version` 只打印 "dev"；
#   2. 本项目【零第三方依赖】（go.mod 无 require），任何环境都能离线构建，
#      不需要 vendor 目录、不需要 GOPROXY。
#
# ⚠️ 本文件必须保存为【带 BOM 的 UTF-8】：
#   Windows PowerShell 5.1 读取无 BOM 的 .ps1 时按 ANSI(GBK) 解码，
#   中文注释会变成乱码并破坏字符串终结符，导致语法错误。
#   （注意与 Go 源码相反：.go/.json 绝不能带 BOM，见 docs/维护备忘.md §五）

param(
    [string]$Version = "dev",
    [string]$Output  = "taoapi.exe",
    [switch]$SkipTests
)

$ErrorActionPreference = "Stop"

# 本机 Go 不在 PATH（见 docs/维护备忘.md §五），能自动找到就补上
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $candidates = @("C:\Program Files\Go\bin", "C:\Go\bin")
    foreach ($c in $candidates) {
        if (Test-Path (Join-Path $c "go.exe")) {
            $env:Path = "$c;" + $env:Path
            break
        }
    }
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Cannot find 'go'. Install Go 1.22+ or add its bin directory to PATH."
}

$pkg  = "workbuddy.local/workbuddy-api/internal/app"
$root = Split-Path -Parent $PSScriptRoot

# -Output 允许给绝对路径。Join-Path 遇到绝对路径会拼成 "root\C:\..." 这种错路径，
# 所以先判根、再归一为绝对路径（曾真实踩到）。
if (-not [System.IO.Path]::IsPathRooted($Output)) {
    $Output = Join-Path $root $Output
}
$Output = [System.IO.Path]::GetFullPath($Output)
$OutputDir = Split-Path -Parent $Output
if ($OutputDir -and -not (Test-Path $OutputDir)) {
    New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null
}

Write-Host "[build] version = $Version"
Write-Host "[build] output  = $Output"

Push-Location $root
try {
    Write-Host "[build] gofmt..."
    $unformatted = gofmt -l .
    if ($unformatted) {
        throw "gofmt failed for:`n$unformatted"
    }

    Write-Host "[build] go vet..."
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet failed" }

    if (-not $SkipTests) {
        # 🔴 必须**分包**跑，不能 `go test ./...`（2026-10-09 修）。
        #
        #	本机装了 Kaspersky，全量单测会被它拖住/挂死（已多次实测）。
        #	分包跑的结果完全等价（同样的包、同样的用例），只是逐个调用
        #	go test，避免一次性并发起太多进程触发杀软扫描。
        #
        #	⚠️ 逐包检查退出码：go test 对失败的包返回非 0，
        #	  但循环里必须**显式**判断，否则最后一个包成功会掩盖前面的失败。
        Write-Host "[build] go test（分包）..."
        $pkgs = go list ./... 2>$null
        if ($LASTEXITCODE -ne 0) { throw "go list ./... failed" }
        $failed = @()
        foreach ($p in $pkgs) {
            if (-not $p) { continue }
            go test $p
            if ($LASTEXITCODE -ne 0) { $failed += $p }
        }
        if ($failed.Count -gt 0) {
            throw "go test failed for:`n$($failed -join "`n")"
        }
    }

    Write-Host "[build] go build..."
    # 🔴 必须带 -H=windowsgui —— 这是**产品需求**，不是可选优化。
    #
    # ═══════════════════════════════════════════════════════════════
    # 2026-10-07 委托人实测反馈（原话："我是用户要使用又不是调试
    # 测试员，搞个终端干嘛"）
    # ═══════════════════════════════════════════════════════════════
    #
    #	漏掉这个标志编出来的是 **WINDOWS_CONSOLE 子系统**（PE 头
    #	OptionalHeader.Subsystem = 3），双击就弹一个黑色终端窗口 ——
    #	而本项目的形态是"双击即用的桌面程序 + 托盘图标"，
    #	黑窗会跟着托盘程序一起常驻，用户看得莫名其妙。
    #
    #	带 -H=windowsgui 则是 Subsystem=2（GUI），双击无黑窗。
    #	⚠️ 代价：**没有控制台**，所以日志必须落文件
    #	   （internal/app/gui_windows.go 的 newGUIlogger 就是为此）。
    #	   命令行用法不受影响：GUI 子系统下 stdout 仍可被重定向/管道读。
    #
    #	下面是**构建后自检**：直接读 PE 头的 Subsystem 字段断言为 2。
    #	护栏的另一半在 `TestBuildScriptProducesGUISubsystem`（查本文件文本）。
        #
    #	🔴 同时必须带 -trimpath（2026-10-09 补）。
    #	   不加的话，exe 里会嵌进**本机绝对路径**（如
    #	   D:\download\DSH\构建反代项目\workbuddy-api\...）—— 那是开发者
    #	   目录结构泄露，且让构建不可复现。v0.1.9 起就要求带，但本脚本
    #	   一直漏着（第 60 轮交接文档 §九 已把它列为"发布前需改"）。
    go build -trimpath -ldflags "-X $pkg.Version=$Version -H=windowsgui" -o $Output ./cmd/wbapi
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
}
finally {
    Pop-Location
}

$exe  = $Output
$size = (Get-Item $exe).Length

# ── 自检①：PE 子系统必须是 GUI(2) ──
# 双击弹黑窗就是因为编成了 CONSOLE(3)。这里直接读 PE 头断言，
# 让"漏了 -H=windowsgui"在构建阶段就炸掉，而不是等用户看到终端窗口。
$bytes = [System.IO.File]::ReadAllBytes($exe)
$peOff = [BitConverter]::ToInt32($bytes, 0x3C)
$optOff = $peOff + 24
$subsystem = [BitConverter]::ToUInt16($bytes, $optOff + 68)
if ($subsystem -ne 2) {
    $what = if ($subsystem -eq 3) { "WINDOWS_CONSOLE(3) —— 双击会弹黑色终端窗口" } else { "未知($subsystem)" }
    throw "[build] 🔴 PE 子系统 = $what；期望 WINDOWS_GUI(2)。构建命令必须带 -H=windowsgui。"
}
Write-Host "[build] 自检: PE 子系统 = WINDOWS_GUI(2) ✓（双击无黑窗）"

Write-Host ("[build] OK -> {0}  ({1:N2} MB)" -f $exe, ($size / 1MB))
Write-Host "[build] self-check:"
& $exe version
