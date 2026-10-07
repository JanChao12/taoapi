# 图标生成工具

生成 TAOAPI 的应用图标（托盘 / 任务栏）。

## 设计说明

| 元素 | 含义 | 依据 |
|---|---|---|
| **圆环**（开口） | "反代网关"——数据在本机中转 | 项目定位 |
| **闪电** | 额度 / 速度 | 面板 logo 原本就是 `⚡` |
| **深色圆角底** | 现代 app 图标惯例；浅色/深色任务栏上都清晰 | — |
| **绿色 `#16a34a`** | 面板主色 | `panel/style.css` |

**小尺寸特殊处理**：16×16 / 24×24 时**去掉圆环、只画大闪电** ——
Windows 会按尺寸自动挑选资源，小尺寸优先"一眼可辨"而不是细节完整。
（32×32 及以上才用完整构图。）

## 用法

需要 Python + Pillow（本机 DSH 运行时自带）：

```powershell
$py = "C:\Users\Administrator\.dsh\dsh-runtimes\dsh-primary-runtime\dependencies\python\python.exe"

# ① 生成各尺寸 PNG（输出到 %TEMP%\taoapi-icon-final）
& $py tools\icon\make_icon.py

# ② 打包成 .ico
#    · 完整版（含 256，供人类查看 / 将来做安装包）
& $py tools\icon\pack_ico.py
#    · 内嵌版（只要 16/24/32/48/64，避免 exe 多出 ~330KB）
& $py tools\icon\pack_ico.py "16,24,32,48,64" internal\app\assets\taoapi.ico
```

## 🔴 两个必须知道的约束（都是实测踩出来的）

### 1. ICO 里必须是 DIB，**不能是内嵌 PNG**

现代图标工具默认把每张图存成内嵌 PNG（Vista+ 扩展）。实测在 Windows 上：

- `LoadImage` / `CreateIconFromResourceEx` 可能**返回成功却渲染成坏图**
  （只有顶部一小条有内容）
- 或直接报 `ERROR_RESOURCE_NAME_NOT_FOUND(1815)`

⇒ `pack_ico.py` 手工写 **DIB**（BITMAPINFOHEADER + 自下而上 BGRA + AND 掩码）。
`TestEmbeddedIconParses` 会断言"每张图都以 `0x28` 开头"，防退化。

### 2. `biHeight` 必须是 `2 × 实际高度`

ICO 里的 DIB 约定：DIB 的高度**包含 AND 掩码**那一半。
写错会导致图像被裁掉上半/下半（症状是"图标只有半截"）。

## 为什么托盘用"运行时创建 HICON"而不是编译期资源（.syso）

托盘图标走 `CreateIconFromResourceEx`（见 `internal/app/trayicon_windows.go`），
**没有**用 `.syso` + `RT_GROUP_ICON` 编译期嵌入。原因：

`.syso` 方案实测**未走通** —— `FindResource` 能命中 `RT_GROUP_ICON`、
重定位也正确回填了 RVA，但 Windows 加载时报 `ERROR_BAD_EXE_FORMAT(193)`，
怀疑与 Go linker 对 `.rsrc` 节的处理有关，已花多轮未收敛。

运行时创建**完全绕开 PE 资源段**，行为确定可控。
代价：**exe 文件本身在资源管理器里仍显示默认图标**（那需要 `.syso`）。
委托方最关心的是托盘图标，该需求已满足。

`tools/genico/` 保留了当时写的 `.syso` 生成器与调研结论，供将来继续。
