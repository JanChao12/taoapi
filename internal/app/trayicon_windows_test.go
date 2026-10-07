//go:build windows

package app

import (
	"encoding/binary"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 托盘图标：ICO 解析与选图逻辑的测试
// ═══════════════════════════════════════════════════════════════════
//
// 这层是**纯逻辑**（不碰 Win32），所以能可靠地单测。
// 真正的 HICON 创建必须实机验证（见 trayicon_windows.go 的说明）。

// TestEmbeddedIconIsPresent 守：内嵌图标真的进了二进制。
//
// 🔴 若 go:embed 路径写错，编译期就会报错；但**文件内容为空**
//
//	是另一种失败（比如打包脚本产出空文件），编译不报错、
//	运行时静默退化到系统图标 —— 用户只会觉得"图标没换"。
func TestEmbeddedIconIsPresent(t *testing.T) {
	if len(trayIconData) == 0 {
		t.Fatal("内嵌图标为空 —— 托盘会退化到系统默认图标")
	}
	if len(trayIconData) < 1000 {
		t.Fatalf("内嵌图标只有 %d 字节，不像一个有效的多尺寸 ICO", len(trayIconData))
	}
	// ICO 魔数
	if trayIconData[0] != 0 || trayIconData[1] != 0 ||
		trayIconData[2] != 1 || trayIconData[3] != 0 {
		t.Fatalf("内嵌数据不是 ICO（头 4 字节 = % x）", trayIconData[:4])
	}
}

// TestEmbeddedIconParses 守：内嵌 ICO 能被解析出多张图像。
func TestEmbeddedIconParses(t *testing.T) {
	imgs := parseICOImages(trayIconData)
	if len(imgs) == 0 {
		t.Fatal("解析不出任何图像")
	}
	if len(imgs) < 3 {
		t.Errorf("只有 %d 张图像，多尺寸图标至少应有 3 张", len(imgs))
	}

	// 必须含 16x16 —— 托盘的标准尺寸，缺了就只能靠缩放（会糊）
	has16 := false
	for _, im := range imgs {
		if im.width == 16 && im.height == 16 {
			has16 = true
		}
		// 每张的偏移/大小必须在文件范围内（parseICOImages 已校验，这里复核）
		if im.offset <= 0 || im.size <= 0 ||
			im.offset+im.size > len(trayIconData) {
			t.Fatalf("图像 %dx%d 越界: off=%d size=%d",
				im.width, im.height, im.offset, im.size)
		}
		// 🔴 必须是 DIB（BITMAPINFOHEADER 起始），不能是内嵌 PNG。
		//
		//	实测：PNG 条目会让 CreateIconFromResourceEx 失败或渲染出坏图
		//	（只有顶部一小条有内容）。所以打包脚本必须写 DIB。
		if trayIconData[im.offset] == 0x89 && trayIconData[im.offset+1] == 'P' {
			t.Fatalf("图像 %dx%d 是内嵌 PNG —— 必须转成 DIB，"+
				"否则 Windows 加载会失败或渲染坏图", im.width, im.height)
		}
		if trayIconData[im.offset] != 0x28 {
			t.Fatalf("图像 %dx%d 不是 BITMAPINFOHEADER DIB（首字节 %#x）",
				im.width, im.height, trayIconData[im.offset])
		}
	}
	if !has16 {
		t.Error("图标里没有 16x16 —— 托盘图标会被缩放而发糊")
	}
}

// TestPickIconPrefersExact 守：选图优先精确匹配。
func TestPickIconPrefersExact(t *testing.T) {
	imgs := []icoImage{
		{width: 16, height: 16, offset: 1, size: 10},
		{width: 32, height: 32, offset: 20, size: 10},
		{width: 48, height: 48, offset: 40, size: 10},
	}
	got, ok := pickIcon(imgs, 32)
	if !ok || got.width != 32 {
		t.Fatalf("要 32 却选到 %d", got.width)
	}
}

// TestPickIconPrefersSmallestLarger 守：没有精确匹配时选**不小于**目标里最小的。
//
// 理由：把大图缩小比把小图放大好看（后者会糊）。
func TestPickIconPrefersSmallestLarger(t *testing.T) {
	imgs := []icoImage{
		{width: 32, height: 32},
		{width: 64, height: 64},
		{width: 128, height: 128},
	}
	got, ok := pickIcon(imgs, 20) // 没有 20，应选 32（不是 128）
	if !ok || got.width != 32 {
		t.Fatalf("要 20 时选了 %d，期望 32（不小于目标里最小的）", got.width)
	}
}

// TestPickIconFallsBackToLargest 守：所有图都小于目标时，选最大的。
func TestPickIconFallsBackToLargest(t *testing.T) {
	imgs := []icoImage{
		{width: 16, height: 16},
		{width: 32, height: 32},
	}
	got, ok := pickIcon(imgs, 256)
	if !ok || got.width != 32 {
		t.Fatalf("要 256 时选了 %d，期望 32（现有最大的）", got.width)
	}
}

// TestParseICORejectsBadData 守：畸形输入返回 nil 而不是 panic 或越界。
//
// 这些数据虽来自内嵌资源（可信），但**解析器不该假定输入可信** ——
// 一旦将来改成"从磁盘读用户提供的 .ico"，这里就是防线。
func TestParseICORejectsBadData(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"空", nil},
		{"太短", []byte{0, 0, 1}},
		{"reserved 非 0", []byte{1, 0, 1, 0, 1, 0}},
		{"type 非 1（是 CUR）", []byte{0, 0, 2, 0, 1, 0}},
		{"count 为 0", []byte{0, 0, 1, 0, 0, 0}},
		{"目录项被截断", []byte{0, 0, 1, 0, 2, 0, 16, 16}},
		{"图像偏移越界", []byte{
			0, 0, 1, 0, 1, 0,
			16, 16, 0, 0, 1, 0, 32, 0,
			100, 0, 0, 0, // size=100
			0xFF, 0xFF, 0, 0, // offset=65535（远超文件长度）
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseICOImages(c.data); got != nil {
				t.Errorf("应当拒绝（返回 nil），实际得到 %d 张", len(got))
			}
		})
	}
}

// TestParseICOAccepts256AsZero 守：宽高字段的 0 要解释成 256。
//
// ICO 的宽高各只占 1 字节，256 无法表示 ⇒ 规范约定写 0。
// 漏了这条会把 256x256 当成 0x0，选图逻辑随之错乱。
func TestParseICOAccepts256AsZero(t *testing.T) {
	// 手工构造一个含 256x256 的 ICO（字段写 0）
	buf := make([]byte, 6+16+16)
	binary.LittleEndian.PutUint16(buf[2:4], 1) // type = ICO
	binary.LittleEndian.PutUint16(buf[4:6], 1) // count = 1
	e := buf[6:22]
	e[0], e[1] = 0, 0 // 0 = 256
	binary.LittleEndian.PutUint16(e[4:6], 1)
	binary.LittleEndian.PutUint16(e[6:8], 32)
	binary.LittleEndian.PutUint32(e[8:12], 16)  // size
	binary.LittleEndian.PutUint32(e[12:16], 22) // offset

	imgs := parseICOImages(buf)
	if len(imgs) != 1 {
		t.Fatalf("应解析出 1 张，实际 %d", len(imgs))
	}
	if imgs[0].width != 256 || imgs[0].height != 256 {
		t.Fatalf("宽高 = %dx%d，期望 256x256（0 应解释为 256）",
			imgs[0].width, imgs[0].height)
	}
}
