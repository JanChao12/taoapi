# 离线构建脚本 —— 产出带版本号的 wbapi.exe
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
    [string]$Output  = "wbapi.exe",
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
        Write-Host "[build] go test..."
        go test ./...
        if ($LASTEXITCODE -ne 0) { throw "go test failed" }
    }

    Write-Host "[build] go build..."
    go build -ldflags "-X $pkg.Version=$Version" -o $Output ./cmd/wbapi
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
}
finally {
    Pop-Location
}

$exe  = $Output
$size = (Get-Item $exe).Length
Write-Host ("[build] OK -> {0}  ({1:N2} MB)" -f $exe, ($size / 1MB))
Write-Host "[build] self-check:"
& $exe version
