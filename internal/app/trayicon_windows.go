//go:build windows

// trayicon_windows.go：从**内嵌的 ICO 字节**创建托盘图标。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么走"运行时创建"而不是"嵌进 exe 资源段（.syso）"
// ═══════════════════════════════════════════════════════════════════
//
//	委托方要求托盘显示自定义图标。有两条路：
//
//	  A. **编译期嵌资源**（`.syso` + RT_GROUP_ICON）
//	     优点：exe 文件本身在资源管理器里也显示该图标
//	     缺点：实测**没走通**（见 tools/genico 的说明）——
//	           虽然 `FindResource` 能找到、重定位也回填正确，
//	           但 Windows 加载时报 ERROR_BAD_EXE_FORMAT(193)，
//	           怀疑与 Go linker 对 `.rsrc` 节的处理有关，
//	           已花多轮未收敛。
//
//	  B. **运行时从内嵌字节创建 HICON**（本文件）
//	     优点：**完全绕开 PE 资源段**，只依赖 Win32 的
//	           `CreateIconFromResourceEx`，行为确定、可控
//	     缺点：exe 文件在资源管理器里仍是默认图标
//	           （任务栏/窗口图标可另行设置，见 SetClassLongPtr）
//
//	⇒ 先用 B 把**托盘图标**（委托方最关心的）做出来。
//	  exe 文件图标（A）作为后续增强，不阻塞当前需求。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 ICO 解析要点
// ═══════════════════════════════════════════════════════════════════
//
//	`CreateIconFromResourceEx` 要的是**单个图像**的 DIB 数据
//	（就是 ICO 里某一张的字节，以 BITMAPINFOHEADER 开头），
//	**不是整个 .ico 文件**。
//
//	所以我必须自己解析 ICONDIR 找出"最合适的尺寸"：
//	  · 托盘要 16x16（高 DPI 下 20/24 更清晰）
//	  · 优先精确匹配，没有就挑不小于目标的**最小**那张
//	    （放大比缩小好看；缩小时细节会糊）
package app

import (
	_ "embed" // go:embed 需要它（即使不直接调用 embed 包）
	"encoding/binary"
	"unsafe"
)

// trayIconData 是内嵌的 ICO 文件内容（由 assets/taoapi.ico 生成）。
//
// ⚠️ 用 go:embed 而不是运行时读文件：单 exe 分发，不能依赖外部文件。
//
//go:embed assets/taoapi.ico
var trayIconData []byte

const (
	// CreateIconFromResourceEx 的标志
	lrDefaultColor = 0x00000000
	lrMonochrome   = 0x00000001

	// 期望的托盘图标尺寸（高 DPI 下系统会取更大的）
	trayIconSize = 16
)

var procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")

// icoImage 是 ICO 里的一张图像。
type icoImage struct {
	width, height int
	offset, size  int
}

// parseICOImages 解析 ICONDIR，返回各图像的位置。
//
// 格式：reserved(2) type(2)=1 count(2) 然后每项 16 字节。
// 只做**最小必要**校验：任何异常都返回 nil，由调用方退化到系统图标。
func parseICOImages(b []byte) []icoImage {
	if len(b) < 6 {
		return nil
	}
	if binary.LittleEndian.Uint16(b[0:2]) != 0 {
		return nil
	}
	if binary.LittleEndian.Uint16(b[2:4]) != 1 { // 1 = ICO（2 是 CUR）
		return nil
	}
	n := int(binary.LittleEndian.Uint16(b[4:6]))
	if n <= 0 || len(b) < 6+n*16 {
		return nil
	}

	out := make([]icoImage, 0, n)
	for i := 0; i < n; i++ {
		e := b[6+i*16:]
		w, h := int(e[0]), int(e[1])
		if w == 0 { // 0 表示 256（字段只有 1 字节）
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size := int(binary.LittleEndian.Uint32(e[8:12]))
		off := int(binary.LittleEndian.Uint32(e[12:16]))
		if off < 0 || size <= 0 || off+size > len(b) {
			return nil // 越界 ⇒ 整个文件不可信
		}
		out = append(out, icoImage{width: w, height: h, offset: off, size: size})
	}
	return out
}

// pickIcon 选出最适合目标尺寸的那张图像。
//
// 策略：精确匹配 → 不小于目标里最小的 → 最大的（总比没有好）。
func pickIcon(imgs []icoImage, want int) (icoImage, bool) {
	if len(imgs) == 0 {
		return icoImage{}, false
	}
	var bestGE *icoImage // 不小于 want 里最小的
	var largest = imgs[0]

	for i := range imgs {
		im := imgs[i]
		if im.width == want {
			return im, true
		}
		if im.width > largest.width {
			largest = im
		}
		if im.width >= want {
			if bestGE == nil || im.width < bestGE.width {
				bestGE = &imgs[i]
			}
		}
	}
	if bestGE != nil {
		return *bestGE, true
	}
	return largest, true
}

// iconFromICOData 从 ICO 字节创建 HICON。
//
// 返回 0 表示失败（调用方应退化为系统默认图标，而不是让托盘挂不上）。
func iconFromICOData(data []byte, want int) uintptr {
	imgs := parseICOImages(data)
	if imgs == nil {
		return 0
	}
	im, ok := pickIcon(imgs, want)
	if !ok {
		return 0
	}

	dib := data[im.offset : im.offset+im.size]
	h, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&dib[0])),
		uintptr(len(dib)),
		1,          // fIcon = TRUE（图标；FALSE 是光标）
		0x00030000, // dwVersion 必须是 0x00030000
		uintptr(im.width), uintptr(im.height),
		lrDefaultColor,
	)
	return h
}
