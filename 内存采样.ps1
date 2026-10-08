# 内存测量脚本（Codex 第 28/33 轮要求）
#
# 为什么用脚本而不是手点：本项目踩过两次测量事故，都必须靠固定口径避免：
#   1. RSS 67MB vs Go 堆 1.27MB —— 我推断错原因并改了代码，实测零变化
#   2. 长时间闲置的进程会被 Windows 换出页面，WorkingSet 塌到几十 KB
#      （实测：闲置 9 小时的 wbapi 报 0.05 MB，刚启动的报 16.32 MB）
#
# 口径（委托方已确认）：
#   60 MB 指标只约束 wbapi **自身**常驻内存，浏览器不计入。
#   但仍须分别测量：wbapi 空闲 / 登录期间峰值 / 结束后稳定值 / 浏览器峰值。
#
# 用法：
#   # 测指定进程（推荐，避免混入残留或闲置进程）
#   powershell -ExecutionPolicy Bypass -File ".\内存采样.ps1" -Label "T0-空闲" -TargetPid 1234
#
#   # 不指定时列出全部候选并给出警告（不会自动求和）
#   powershell -ExecutionPolicy Bypass -File ".\内存采样.ps1" -Label "T0-空闲"
#
# 为什么用 WorkingSet64 而不是 PrivateMemorySize64：
#   前者是物理内存中实际驻留的部分，即通常说的 RSS；
#   后者是提交大小，与常驻内存不是一个口径（会把保留但未触碰的算进来）。

param(
    [Parameter(Mandatory = $true)][string]$Label,
    # 要测量的进程。不给则只列出候选，不报数字。
    # 注意：不能叫 Pid —— 那是 PowerShell 的只读自动变量
    # （实测会报 Cannot overwrite variable Pid because it is read-only）。
    [int]$TargetPid = 0,
    # 重复采样次数，取中位数（防瞬时抖动）
    [int]$Samples = 3
)

$ErrorActionPreference = "Continue"

function Get-Median([double[]]$values) {
    if ($values.Count -eq 0) { return 0 }
    $sorted = $values | Sort-Object
    $mid = [int][math]::Floor($sorted.Count / 2)
    if ($sorted.Count % 2 -eq 1) { return $sorted[$mid] }
    return ($sorted[$mid - 1] + $sorted[$mid]) / 2
}

function Format-MB([double]$bytes) {
    return [math]::Round($bytes / 1MB, 2)
}

$stamp = Get-Date -Format "HH:mm:ss"
Write-Host ""
Write-Host ("=" * 60) -ForegroundColor DarkGray
Write-Host "  [$Label]  $stamp" -ForegroundColor Cyan
Write-Host ("=" * 60) -ForegroundColor DarkGray

# ---- 未指定进程：列出候选并提示（不自动求和）----
if ($TargetPid -eq 0) {
    $cands = @(Get-CimInstance Win32_Process -Filter "Name='taoapi.exe'" -ErrorAction SilentlyContinue)
    if ($cands.Count -eq 0) {
        Write-Host "  没有正在运行的 taoapi 进程。" -ForegroundColor Yellow
        Write-Host "  提示：先启动一个全新实例再测 —— 闲置久的进程会被换出，数字不可信。" -ForegroundColor DarkGray
        return
    }
    if ($cands.Count -eq 1) {
        $TargetPid = $cands[0].ProcessId
        Write-Host "  自动选中唯一进程 PID $TargetPid" -ForegroundColor DarkGray
    } else {
        Write-Host "  发现 $($cands.Count) 个 wbapi 进程，请用 -TargetPid 指定要测哪个：" -ForegroundColor Yellow
        foreach ($c in $cands) {
            $pp = Get-Process -Id $c.ProcessId -ErrorAction SilentlyContinue
            $ws = 0
            if ($pp) { $ws = Format-MB $pp.WorkingSet64 }
            Write-Host ("    PID {0,-7} WorkingSet={1,9} MB   {2}" -f $c.ProcessId, $ws, $c.CommandLine)
        }
        Write-Host ""
        Write-Host "  多个进程时不要求和 —— 它们不是同一个服务。" -ForegroundColor DarkGray
        Write-Host "  WorkingSet 极小（小于 1 MB）时请先核对进程存活与端口监听，再决定能否采信。" -ForegroundColor DarkGray
        return
    }
}

# ---- 测指定进程 ----
$wbapiSamples = @()
for ($i = 0; $i -lt $Samples; $i++) {
    $p = Get-Process -Id $TargetPid -ErrorAction SilentlyContinue
    if (-not $p) {
        Write-Host "  进程 $TargetPid 不存在（可能已退出）" -ForegroundColor Red
        return
    }
    $wbapiSamples += $p.WorkingSet64
    if ($i -lt $Samples - 1) { Start-Sleep -Milliseconds 300 }
}

$wbapiMedian = Get-Median $wbapiSamples
$wbapiMB = Format-MB $wbapiMedian
$cmdline = (Get-CimInstance Win32_Process -Filter "ProcessId=$TargetPid" -ErrorAction SilentlyContinue).CommandLine

# ---- 复核提示：WorkingSet 异常低时要求人工确认 ----
#
# 注意措辞（Codex 第 34 轮纠正）：
#   低 WorkingSet 的**原因**（如"被换出到磁盘"）不是这些数字单独能证明的，
#   工作集裁剪不一定意味着页面写入磁盘。
#   所以这里只作为**复核提示**，不是普遍的"数据不可信"判据。
#
# 正确做法是核对：进程存活 / 监听端口 / 启动时间 / 服务响应。
$suspicious = $wbapiMedian -lt 1MB
$aliveCheck = $true
$portCheck = $false
if ($cmdline -match '--addr\s+127\.0\.0\.1:(\d+)') {
    $port = [int]$Matches[1]
    $portCheck = [bool](Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue)
}

Write-Host "  wbapi PID $TargetPid"
if ($cmdline) { Write-Host "    $cmdline" -ForegroundColor DarkGray }
if ($suspicious) {
    # 措辞注意：只说"观察到异常低"，不断言原因（Codex 第 34 轮纠正）。
    Write-Host ("    常驻内存 : {0} MB   <-- 复核提示：异常低" -f $wbapiMB) -ForegroundColor Yellow
    Write-Host "    先核对下面几项，再决定这个数字能不能用：" -ForegroundColor Yellow
    Write-Host ("      进程存活 : {0}" -f $aliveCheck)
    if ($cmdline -match '--addr') {
        Write-Host ("      端口监听 : {0}" -f $portCheck)
    } else {
        Write-Host "      端口监听 : (命令行里没有 --addr，未检查)"
    }
    Write-Host "    该值不适合作为活跃负载基线。建议改用刚启动的实例重测。" -ForegroundColor Yellow
} else {
    Write-Host ("    常驻内存 : {0,9} MB   (中位数 n={1})" -f $wbapiMB, $Samples) -ForegroundColor Green
}
$peakBytes = ($wbapiSamples | Measure-Object -Maximum).Maximum
$peak = Format-MB $peakBytes
$privMB = 0
$privProc = Get-Process -Id $TargetPid -ErrorAction SilentlyContinue
if ($privProc) { $privMB = Format-MB $privProc.PrivateMemorySize64 }
Write-Host ("    采样峰值 : {0,9} MB   (注意：中位数口径会过滤短暂峰值)" -f $peak) -ForegroundColor DarkGray
Write-Host ("    提交内存 : {0,9} MB   (仅供参考，不是 RSS)" -f $privMB) -ForegroundColor DarkGray

# ---- 受控浏览器进程树 ----
# 必须按命令行含 wbapi-login- 过滤，否则会把用户自己的浏览器算进来。
$browserSamples = @()
for ($i = 0; $i -lt $Samples; $i++) {
    $sum = 0
    $procs = Get-CimInstance Win32_Process -Filter "Name='chrome.exe' OR Name='msedge.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -like "*wbapi-login-*" }
    foreach ($bp in $procs) {
        $gp = Get-Process -Id $bp.ProcessId -ErrorAction SilentlyContinue
        if ($gp) { $sum += $gp.WorkingSet64 }
    }
    $browserSamples += $sum
    if ($i -lt $Samples - 1) { Start-Sleep -Milliseconds 300 }
}
$browserMB = Format-MB (Get-Median $browserSamples)
$browserCount = @(Get-CimInstance Win32_Process -Filter "Name='chrome.exe' OR Name='msedge.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -like "*wbapi-login-*" }).Count

if ($browserCount -gt 0) {
    Write-Host ("  浏览器进程树: {0,9} MB   ({1} 个进程，求和)" -f $browserMB, $browserCount) -ForegroundColor Yellow
    Write-Host "     简单求和可能高估（子进程共享部分内存）" -ForegroundColor DarkGray
} else {
    Write-Host "  浏览器进程树: 未运行" -ForegroundColor DarkGray
}

# ---- 残留临时 profile（应为 0）----
$leftover = @(Get-ChildItem $env:TEMP -Directory -Filter "wbapi-login-*" -ErrorAction SilentlyContinue).Count
$lc = "Green"
if ($leftover -ne 0) { $lc = "Yellow" }
Write-Host ("  残留临时 profile: {0} 个" -f $leftover) -ForegroundColor $lc
Write-Host ""

# 输出一行便于汇总（制表符分隔；不可信时标 BAD）
$flag = "OK"
if ($suspicious) { $flag = "REVIEW" }
$out = "SAMPLE" + "`t" + $Label + "`t" + $wbapiMB + "`t" + $browserMB + "`t" + $TargetPid + "`t" + $browserCount + "`t" + $privMB + "`t" + $flag
Write-Output $out
