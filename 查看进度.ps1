# 进度查看器：任何时候运行都能看到登录功能的真实状态。
#
# 为什么做这个：委托方问「能不能让我看到你的进度」。
# 与其我口头汇报（可能过期、可能美化），不如给他一个**随时可跑的命令**，
# 输出的全是磁盘上的事实（git 日志、代码行数、测试结果）。
#
# 用法（在任意 PowerShell 窗口）：
#   & "D:\download\DSH\构建反代项目\workbuddy-api\查看进度.ps1"
#
# 可选参数：
#   -Fast    跳过跑测试（只显示 git/代码统计，秒出）

param(
    [switch]$Fast
)

$ErrorActionPreference = "Continue"
$root = "D:\download\DSH\构建反代项目\workbuddy-api"
Set-Location $root

function Write-Header($text) {
    Write-Host ""
    Write-Host ("═" * 64) -ForegroundColor DarkGray
    Write-Host "  $text" -ForegroundColor Cyan
    Write-Host ("═" * 64) -ForegroundColor DarkGray
}

function Write-Item($label, $value, $color = "White") {
    Write-Host ("  {0,-34}" -f $label) -NoNewline -ForegroundColor Gray
    Write-Host $value -ForegroundColor $color
}

Clear-Host
Write-Host ""
Write-Host "   WorkBuddy 反代项目 —— 网页登录功能进度" -ForegroundColor White
Write-Host "   $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')" -ForegroundColor DarkGray

# ─────────────────────────────────────────────────────────────
# 1. 总体阶段
# ─────────────────────────────────────────────────────────────
Write-Header "一、阶段总览"

# ⚠️ 关键：每个阶段的 done 状态都由**磁盘上的事实**推导，不是我手写。
# 手写状态等于自己给自己打分 —— 那样这份进度就没有可信度。
function Has-File($rel) { return (Test-Path (Join-Path $root $rel)) }
function Has-Text($rel, $pattern) {
    $p = Join-Path $root $rel
    if (-not (Test-Path $p)) { return $false }
    return [bool](Select-String -Path $p -Pattern $pattern -Quiet -ErrorAction SilentlyContinue)
}

$stages = @(
    @{ name = "调研登录机制";     done = (Has-File "docs\panel-dev\cdp-login-probe.js");   note = "探针脚本在" },
    @{ name = "定位凭据来源";     done = (Has-Text "internal\login\cdp.go" "console/login/enterprise"); note = "端点已写进代码" },
    @{ name = "WebSocket 客户端"; done = (Has-File "internal\login\ws.go");                note = "ws.go 存在" },
    @{ name = "浏览器生命周期";   done = (Has-File "internal\login\browser.go");           note = "browser.go 存在" },
    @{ name = "CDP 编排与提取";   done = (Has-Text "internal\login\login.go" "loadingFinished"); note = "等 loadingFinished" },
    @{ name = "端到端集成测试";   done = (Has-File "internal\login\integration_test.go");  note = "integration_test.go 存在" },
    @{ name = "身份补全 + API";   done = (Has-File "internal\provider\workbuddy\identity.go"); note = "identity.go 存在" },
    @{ name = "面板登录按钮";     done = (Has-Text "internal\app\panel\app.js" "login/start"); note = "app.js 里已有调用" },
    @{ name = "refresh 续期实验"; done = (Has-File "docs\panel-dev\refresh-persistence-result.md"); note = "结论已落盘" },
    @{ name = "内存测量";         done = (Has-File "docs\panel-dev\memory-measurement.md"); note = "含实测值与陷阱" },
    # 判定依据用**文档里实际写的路径**（/v2/plugin/login/account），
    # 不用代码常量名（PathLoginAccount）—— 文档写的是接口路径，不是常量。
    @{ name = "文档与发布说明";   done = (Has-Text "docs\upstream-contract.md" "/v2/plugin/login/account"); note = "登录契约已补" },
    @{ name = "委托方实测验收";   done = (Has-File "docs\panel-dev\acceptance-result.md"); note = "四项均通过" }
)

$doneCount = ($stages | Where-Object { $_.done }).Count
Write-Item "已完成阶段" "$doneCount / $($stages.Count)" "Green"
Write-Host ""
foreach ($s in $stages) {
    if ($s.done) {
        Write-Host "  [✓] " -NoNewline -ForegroundColor Green
        Write-Host ("{0,-22}" -f $s.name) -NoNewline -ForegroundColor White
    } else {
        Write-Host "  [ ] " -NoNewline -ForegroundColor Yellow
        Write-Host ("{0,-22}" -f $s.name) -NoNewline -ForegroundColor Gray
    }
    Write-Host $s.note -ForegroundColor DarkGray
}

# ─────────────────────────────────────────────────────────────
# 2. 代码量
# ─────────────────────────────────────────────────────────────
Write-Header "二、代码量（登录功能）"

$files = @(
    "internal\login\ws.go",
    "internal\login\ws_test.go",
    "internal\login\browser.go",
    "internal\login\browser_test.go",
    "internal\login\cdp.go",
    "internal\login\cdp_test.go",
    "internal\login\login.go",
    "internal\login\login_test.go",
    "internal\login\integration_test.go",
    "internal\app\api_login.go",
    "internal\app\api_login_test.go",
    "internal\provider\workbuddy\identity.go"
)

$totalLines = 0
$totalTest = 0
foreach ($f in $files) {
    $path = Join-Path $root $f
    if (Test-Path $path) {
        $n = (Get-Content $path | Measure-Object -Line).Lines
        $totalLines += $n
        $isTest = $f -match "_test\.go$"
        if ($isTest) { $totalTest += $n }
        $color = if ($isTest) { "DarkGray" } else { "White" }
        $tag = if ($isTest) { "测试" } else { "实现" }
        Write-Host ("  [{0}] {1,-42} {2,5} 行" -f $tag, (Split-Path $f -Leaf), $n) -ForegroundColor $color
    }
}
Write-Host ""
Write-Item "实现代码" "$($totalLines - $totalTest) 行" "White"
Write-Item "测试代码" "$totalTest 行" "DarkGray"
Write-Item "合计" "$totalLines 行" "Cyan"

# ─────────────────────────────────────────────────────────────
# 3. Git 提交
# ─────────────────────────────────────────────────────────────
Write-Header "三、提交记录（最近 8 条）"

# git 默认按 GBK 输出中文，PowerShell 会显示成乱码 —— 显式要求 UTF-8。
$env:LC_ALL = "C.UTF-8"
git config --local i18n.logOutputEncoding utf-8 2>$null
$log = git -c i18n.logOutputEncoding=utf-8 log --oneline -8
# ⚠️ 原先用正则 ^(.....)\s+(.*)$ 解析，实测**匹配不上**（git 的 oneline 格式
# 里 hash 后是单个空格，正则在中文/全角环境下行为不稳定）。
# 改成按第一个空格切分 —— 直白且不会静默失败。
foreach ($line in @($log)) {
    if (-not $line) { continue }
    $idx = $line.IndexOf(' ')
    if ($idx -gt 0) {
        $hash = $line.Substring(0, $idx)
        $msg = $line.Substring($idx + 1).Trim()
        Write-Host "  " -NoNewline
        Write-Host $hash -ForegroundColor Yellow -NoNewline
        Write-Host "  $msg" -ForegroundColor White
    } else {
        Write-Host "  $line" -ForegroundColor White
    }
}

Write-Host ""
# core.quotepath=false 让 git 直接输出中文文件名，而不是 \346\237\245 这样的转义。
$dirty = git -c core.quotepath=false status --short
if ($dirty) {
    Write-Item "工作区状态" "有未提交改动" "Yellow"
    foreach ($d in $dirty) { Write-Host "    $d" -ForegroundColor DarkYellow }
} else {
    Write-Item "工作区状态" "干净（全部已提交）" "Green"
}

# ─────────────────────────────────────────────────────────────
# 4. 测试
# ─────────────────────────────────────────────────────────────
Write-Header "四、测试"

if ($Fast) {
    Write-Host "  （-Fast 模式，跳过跑测试）" -ForegroundColor DarkGray
    Write-Host "  去掉 -Fast 参数可看真实测试结果" -ForegroundColor DarkGray
} else {
    $env:Path = "C:\Program Files\Go\bin;" + $env:Path
    Write-Host "  正在运行测试（约 15 秒）..." -ForegroundColor DarkGray

    $out = go test ./internal/login/ ./internal/app/ -count=1 -v 2>&1
    $pass = ($out | Select-String "^--- PASS").Count
    $fail = ($out | Select-String "^--- FAIL").Count
    $skip = ($out | Select-String "^--- SKIP").Count

    Write-Host ""
    Write-Item "通过" "$pass" "Green"
    Write-Item "失败" "$fail" $(if ($fail -gt 0) { "Red" } else { "DarkGray" })
    Write-Item "跳过（需浏览器，默认跳过）" "$skip" "DarkGray"

    if ($fail -gt 0) {
        Write-Host ""
        Write-Host "  失败的测试：" -ForegroundColor Red
        $out | Select-String "^--- FAIL" | ForEach-Object { Write-Host "    $_" -ForegroundColor Red }
    }

    # 真实浏览器集成测试（单独跑，较慢）
    Write-Host ""
    Write-Host "  提示：真实浏览器集成测试需手动开启：" -ForegroundColor DarkGray
    Write-Host '    $env:WBAPI_RUN_BROWSER_TESTS="1"; go test ./internal/login/ -run TestCDP -v' -ForegroundColor DarkGray
}

# ─────────────────────────────────────────────────────────────
# 5. 环境
# ─────────────────────────────────────────────────────────────
Write-Header "五、环境状态"

# 浏览器
$chrome = "C:\Program Files\Google\Chrome\Application\chrome.exe"
$edge = "${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe"
Write-Item "Chrome" $(if (Test-Path $chrome) { "已安装 ✓" } else { "未找到" }) $(if (Test-Path $chrome) { "Green" } else { "Yellow" })
Write-Item "Edge" $(if (Test-Path $edge) { "已安装 ✓" } else { "未找到" }) $(if (Test-Path $edge) { "Green" } else { "DarkGray" })

# 残留的登录临时 profile（应为 0）
$leftover = Get-ChildItem $env:TEMP -Directory -Filter "wbapi-login-*" -ErrorAction SilentlyContinue
Write-Item "残留临时 profile" "$($leftover.Count) 个" $(if ($leftover.Count -eq 0) { "Green" } else { "Yellow" })

# 残留浏览器进程
$strayChrome = Get-Process chrome -ErrorAction SilentlyContinue | Where-Object { $_.StartTime -gt (Get-Date).AddMinutes(-30) }
Write-Item "残留浏览器进程" "$($strayChrome.Count) 个" $(if ($strayChrome.Count -eq 0) { "Green" } else { "Yellow" })

# 账号数（用安全方式：wbapi.exe status）
$wbapi = Join-Path $root "wbapi.exe"
if (Test-Path $wbapi) {
    $status = & $wbapi status 2>&1 | Out-String
    $acctCount = ([regex]::Matches($status, "正常|限流|禁用")).Count
    Write-Item "账号库" "$acctCount 个账号" "White"
}

Write-Host ""
Write-Host ("═" * 64) -ForegroundColor DarkGray
Write-Host ""
