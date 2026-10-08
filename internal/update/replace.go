package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 替换过程用到的后缀。
const (
	// bakSuffix 是旧 exe 的备份后缀（替换后保留，供回滚）。
	bakSuffix = ".old"

	// newSuffix 是新 exe 在下载目录里的临时名。
	newSuffix = ".new"
)

// ReplaceSelf 把当前运行的 exe 替换成 newExe。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 Windows 的关键约束：**正在运行的 exe 不能被覆盖，但可以改名**
// ═══════════════════════════════════════════════════════════════════
//
//	这一点在本项目里已经实测踩过：早前 `Copy-Item` 覆盖正在运行的
//	taoapi.exe 时报 "文件正由另一进程使用，因此该进程无法访问此文件"。
//
//	Windows 的规则是：
//	  · 运行中的映像被**独占锁定** ⇒ 不能写、不能删；
//	  · 但**可以重命名**（Rename 只改目录项，不动文件数据）。
//
//	⇒ 所以"自替换"必须走**改名交换**，不能直接覆盖：
//
//	  1. 把正在运行的 exe 改名为 `<exe>.old`（合法，映像句柄跟着走）
//	  2. 把下载好的新 exe 改名为 `<exe>`（此时原位置已空出）
//	  3. 由调用方重启 —— 新进程从 `<exe>` 启动，跑的是新版本
//
//	⚠️ 第 2 步失败时必须**把第 1 步改回来**，否则用户就
//	   "既没有新 exe、也没有原 exe"——程序直接消失。这是本函数最该守住的点。
//
// 返回的 oldPath 供回滚/清理使用。
func ReplaceSelf(newExe string) (oldPath string, err error) {
	if strings.TrimSpace(newExe) == "" {
		return "", fmt.Errorf("update：新 exe 路径为空")
	}
	if _, statErr := os.Stat(newExe); statErr != nil {
		return "", fmt.Errorf("update：新 exe 不存在: %w", statErr)
	}

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("update：无法定位当前 exe: %w", err)
	}
	// 解析符号链接，确保比较的是同一路径（Windows 上一般不需要，
	// 但保持与 os.Executable 的语义一致）。
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}

	// 防御：新旧必须是同一个目录下的**同名文件**。
	//
	// 若不同名/不同目录，说明调用方给错了路径（例如把下载到别处的
	// 文件当成"新版"），改名交换会得到一个意外的文件名。
	if !strings.EqualFold(filepath.Base(self), AssetName) {
		return "", fmt.Errorf("update：当前 exe 名为 %q，与预期资产名 %q 不符，拒绝自动替换",
			filepath.Base(self), AssetName)
	}

	oldPath = self + bakSuffix

	// 上一轮可能留下了 .old（例如上次更新后没清理、或用户手工放回）。
	// 先移除，避免 Rename 因目标存在而失败。
	if _, statErr := os.Stat(oldPath); statErr == nil {
		if rmErr := os.Remove(oldPath); rmErr != nil {
			// 旧 .old 可能正被某个残留进程占用 —— 那就换一个带时间戳的名字，
			// 而不是让整个更新失败。
			oldPath = fmt.Sprintf("%s.%d%s", self, os.Getpid(), bakSuffix)
		}
	}

	// ── 第 1 步：运行中的 exe 改名为 .old（Windows 允许改名）──
	if err := os.Rename(self, oldPath); err != nil {
		return "", fmt.Errorf("update：无法重命名当前 exe（若被占用请关闭托盘后重试）: %w", err)
	}

	// ── 第 2 步：新 exe 就位；失败必须回滚第 1 步 ──
	if err := os.Rename(newExe, self); err != nil {
		// 🔴 回滚：把原名改回来，否则程序"消失"了。
		if rbErr := os.Rename(oldPath, self); rbErr != nil {
			return "", fmt.Errorf("update：新 exe 就位失败(%v)，且回滚也失败(%v)；"+
				"原程序在 %s，请手工改回 %s", err, rbErr, oldPath, filepath.Base(self))
		}
		return "", fmt.Errorf("update：新 exe 就位失败（已回滚，程序未受影响）: %w", err)
	}

	return oldPath, nil
}

// CleanupOld 删除替换后留下的旧 exe。
//
// 为什么单独一步而不是在 ReplaceSelf 里顺手删：
//
//	刚改名时旧映像**仍在运行**（本进程就是它），文件被锁定 ⇒ 删不掉。
//	必须等新进程起来、旧进程退出后才可能删除。
//	所以由**新进程启动时**尝试清理（best-effort，失败不影响启动）。
func CleanupOld() (removed string, err error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	// 清两种：无后缀的 .old 与带 pid 的 .old（见 ReplaceSelf 的兜底分支）。
	candidates := []string{self + bakSuffix}
	if m, gerr := filepath.Glob(self + ".*" + bakSuffix); gerr == nil {
		candidates = append(candidates, m...)
	}
	for _, p := range candidates {
		if p == self {
			continue
		}
		if _, statErr := os.Stat(p); statErr != nil {
			continue
		}
		// 删不掉（仍被占用）不算错误 —— 下次启动再试。
		if rmErr := os.Remove(p); rmErr == nil {
			removed = p
		}
	}
	return removed, nil
}
