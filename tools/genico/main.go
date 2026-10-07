// genico 生成把 .ico 嵌进 Windows exe 的 .syso 资源对象。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么要有这个工具（而不是用现成的 windres / rsrc）
// ═══════════════════════════════════════════════════════════════════
//
//	本项目硬约束是**零第三方依赖 + 离线可构建**，且 CI 里有断言守着。
//	而本机**没有 gcc/mingw**（`windres` 也就没有 —— 维护备忘 §五 已记录
//	"本机没有 gcc"），所以常规做法（windres 把 .rc 编成 .syso）走不通。
//
//	⇒ 直接生成 Go linker 能吃的 **COFF 目标文件**（就是 .syso）。
//	  Go 自己就带这种样本（`$GOROOT/src/cmd/link/testdata/pe-binutils/
//	  rsrc_amd64.syso`），结构极简：一个 `.rsrc` 节 + 一个符号。
//
// ⚠️ 本工具是**一次性生成器**：产物 `cmd/wbapi/rsrc_amd64.syso` 入库，
//
//	日常 `go build` 不需要它（也不该在构建时依赖它）。
//	换图标时才重跑：`go run ./tools/genico -ico <路径> -o <输出>`。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 PNG 条目必须转成 DIB(BMP)（2026-10-07 实测踩到）
// ═══════════════════════════════════════════════════════════════════
//
//	现代图标工具（含委托方给的 taoapi.ico）默认把每张图存成**内嵌 PNG**
//	（Vista+ 的 ICO 扩展）。实测后果：
//
//	  · `LoadImage(hInst, ID, IMAGE_ICON, 16, 16, 0)` **返回了句柄**
//	    （所以"加载失败"这类断言抓不到），但**渲染出来只有顶部一小条**、
//	    其余全白 —— 托盘图标不可用。
//	  · 32x32/48x48 直接 `ERROR_RESOURCE_NAME_NOT_FOUND(1815)`。
//
//	⇒ 用 `image/png` 解码后**重写成传统 DIB**（BITMAPINFOHEADER +
//	  自下而上的 BGRA 像素 + AND 掩码）。这是托盘/任务栏兼容性最好的形式。
//
//	⚠️ 保留 256x256 也用 DIB：虽然文件会变大（约 +250KB），
//	  但换来"所有尺寸都是同一种格式"，避免尺寸相关的不一致行为。
//	  本工具是**离线一次性**跑的，产物体积不影响运行时内存。
//
// ═══════════════════════════════════════════════════════════════════
// 生成的资源结构（Windows PE 资源目录，三层）
// ═══════════════════════════════════════════════════════════════════
//
//	Type(3=RT_ICON)  → Name(ID)     → Lang(1033) → DataEntry → 图标图像字节
//	Type(14=RT_GROUP_ICON) → Name(ID) → Lang(1033) → DataEntry → GRPICONDIR
//
//	RT_GROUP_ICON 是"图标目录"：它引用各个 RT_ICON 里的图像，
//	Windows 靠它把多尺寸图标当成**一个**逻辑图标（托盘用 16x16、
//	任务栏用 32x32，自动挑合适的尺寸）。
//
//	⚠️ 只嵌 RT_ICON 而不嵌 RT_GROUP_ICON 是不行的 —— 那样
//	  「任务栏/资源管理器」看不到图标（只有托盘可能碰巧工作）。
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	_ "image/png" // ICO 里的内嵌 PNG 要用它解码
	"os"
	"path/filepath"
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	langID      = 1033 // en-US（Windows 默认回退语言，实测显示正常）
	iconResID   = 1    // 主图标 ID（exe 用 ID 1）
	groupResID  = 1
)

func main() {
	icoPath := flag.String("ico", "", "输入 .ico 路径")
	outPath := flag.String("o", "", "输出 .syso 路径")
	flag.Parse()

	if *icoPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "用法: genico -ico <input.ico> -o <output.syso>")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*icoPath)
	if err != nil {
		fatal("读取 ico 失败: %v", err)
	}
	images, err := parseICO(raw)
	if err != nil {
		fatal("解析 ico 失败: %v", err)
	}
	fmt.Printf("读到 %d 个图像: ", len(images))
	for i, im := range images {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Printf("%dx%d", im.width, im.height)
	}
	fmt.Println()

	// 🔴 PNG 条目必须先转成 DIB，否则托盘图标会渲染成坏图（见文件头说明）。
	converted := 0
	for i := range images {
		before := len(images[i].data)
		wasPNG := bytes.HasPrefix(images[i].data, pngMagic)
		im, err := toDIB(images[i])
		if err != nil {
			fatal("%v", err)
		}
		images[i] = im
		if wasPNG {
			converted++
			fmt.Printf("  %dx%d：PNG → DIB（%d → %d 字节）\n",
				im.width, im.height, before, len(im.data))
		}
	}
	if converted > 0 {
		fmt.Printf("已转换 %d 张 PNG 条目为 DIB\n", converted)
	}

	res := buildResources(images)
	syso := buildCOFF(res)

	if err := os.WriteFile(*outPath, syso, 0o644); err != nil {
		fatal("写 syso 失败: %v", err)
	}
	fmt.Printf("已生成 %s（%d 字节，%d 条重定位）\n", *outPath, len(syso), len(res.relocs))
}

// 重定位类型（PE/COFF 规范）
//
//	IMAGE_REL_AMD64_ADDR32NB = 0x0003
//	  "32 位 RVA"，即**相对镜像基址**的地址。资源 DataEntry 要的就是它。
const relAmd64Addr32NB = 0x0003

// reloc 是一条重定位：在节内 offset 处，写入该节基址 + 目标偏移。
type reloc struct {
	offset   uint32 // 节内位置（将要被链接器回填的 4 字节字段）
	typeCode uint16
}

// resourceSection 是构造好的资源节：数据 + 需要回填的位置。
type resourceSection struct {
	data   []byte
	relocs []reloc
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "genico: "+f+"\n", a...)
	os.Exit(1)
}

// iconImage 是 .ico 里的一张图像。
type iconImage struct {
	width, height int
	// colorCount / planes / bitCount 原样保留 —— GRPICONDIRENTRY 要回填
	colorCount, planes, bitCount int
	data                         []byte
}

// parseICO 解析 .ico 文件（ICONDIR + ICONDIRENTRY[] + 图像数据）。
//
// 格式（全部小端）：
//
//	ICONDIR        : reserved(2) type(2)=1 count(2)
//	ICONDIRENTRY[] : w(1) h(1) colors(1) reserved(1) planes(2) bitcount(2)
//	                 bytesInRes(4) imageOffset(4)
func parseICO(b []byte) ([]iconImage, error) {
	if len(b) < 6 {
		return nil, fmt.Errorf("文件太小")
	}
	if binary.LittleEndian.Uint16(b[0:2]) != 0 {
		return nil, fmt.Errorf("reserved 字段非 0（不是 ICO）")
	}
	if t := binary.LittleEndian.Uint16(b[2:4]); t != 1 {
		return nil, fmt.Errorf("type=%d，期望 1（ICO）；2 是 CUR，不支持", t)
	}
	n := int(binary.LittleEndian.Uint16(b[4:6]))
	if n == 0 {
		return nil, fmt.Errorf("图标数为 0")
	}
	if len(b) < 6+n*16 {
		return nil, fmt.Errorf("目录项被截断")
	}

	out := make([]iconImage, 0, n)
	for i := 0; i < n; i++ {
		e := b[6+i*16:]
		w, h := int(e[0]), int(e[1])
		// ICO 用 0 表示 256（字段只有 1 字节）
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size := int(binary.LittleEndian.Uint32(e[8:12]))
		off := int(binary.LittleEndian.Uint32(e[12:16]))
		if off < 0 || size < 0 || off+size > len(b) {
			return nil, fmt.Errorf("第 %d 张图像越界（off=%d size=%d）", i, off, size)
		}
		out = append(out, iconImage{
			width: w, height: h,
			colorCount: int(e[2]),
			planes:     int(binary.LittleEndian.Uint16(e[4:6])),
			bitCount:   int(binary.LittleEndian.Uint16(e[6:8])),
			data:       append([]byte(nil), b[off:off+size]...),
		})
	}
	return out, nil
}

// pngMagic 是 PNG 文件头。
var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// toDIB 把"内嵌 PNG"的图标图像改写成传统 DIB(BMP) 形式。
//
// 非 PNG 的（本来就是 DIB）原样返回。
//
// 🔴 为什么要转：见文件头「PNG 条目必须转成 DIB」一节 ——
// 实测 PNG 条目会让 `LoadImage` 返回句柄却**渲染出坏图**，
// 或直接报 `ERROR_RESOURCE_NAME_NOT_FOUND`。
//
// DIB 布局（ICO 里的"BMP"条目）：
//
//	BITMAPINFOHEADER(40)
//	  biSize=40, biWidth=w, biHeight=**2*h**（ICO 惯例：含 AND 掩码的高度）
//	  biPlanes=1, biBitCount=32, biCompression=0(BI_RGB), biSizeImage=0
//	像素数据：**自下而上**、每行 w*4 字节、顺序 B,G,R,A
//	AND 掩码：自下而上、每行补齐到 4 字节，1bpp（32bpp 时全 0 即可）
func toDIB(im iconImage) (iconImage, error) {
	if len(im.data) < 8 || !bytes.HasPrefix(im.data, pngMagic) {
		return im, nil // 已经是 DIB
	}
	src, _, err := image.Decode(bytes.NewReader(im.data))
	if err != nil {
		return im, fmt.Errorf("解码内嵌 PNG 失败（%dx%d）: %w", im.width, im.height, err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	// AND 掩码：每行按 4 字节对齐，1bpp
	maskStride := ((w + 31) / 32) * 4
	pixBytes := w * h * 4
	maskBytes := maskStride * h

	out := make([]byte, 0, 40+pixBytes+maskBytes)

	// BITMAPINFOHEADER
	out = le32(out, 40)          // biSize
	out = le32(out, uint32(w))   // biWidth
	out = le32(out, uint32(h*2)) // biHeight（ICO 惯例）
	out = le16(out, 1)           // biPlanes
	out = le16(out, 32)          // biBitCount
	out = le32(out, 0)           // biCompression = BI_RGB
	out = le32(out, 0)           // biSizeImage
	out = le32(out, 0)           // biXPelsPerMeter
	out = le32(out, 0)           // biYPelsPerMeter
	out = le32(out, 0)           // biClrUsed
	out = le32(out, 0)           // biClrImportant

	// 像素：自下而上、BGRA
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			out = append(out, byte(bl>>8), byte(g>>8), byte(r>>8), byte(a>>8))
		}
	}
	// AND 掩码全 0：32bpp 下透明度由 alpha 通道决定，掩码不参与
	out = append(out, make([]byte, maskBytes)...)

	return iconImage{
		width: w, height: h,
		colorCount: 0,
		planes:     1,
		bitCount:   32,
		data:       out,
	}, nil
}

// buildResources 组装整棵资源目录树，返回节内容。
//
// 布局（这是 Windows 资源节的经典结构，顺序不能任意）：
//
//	[0]                    根目录（按类型分）
//	各类型目录（RT_ICON / RT_GROUP_ICON）
//	各名称目录（按 ID 分）
//	各语言目录（按 lang 分）
//	数据条目（DataEntry，指向实际字节）
//	... 然后是各段实际数据（图标图像、GRPICONDIR），4 字节对齐
func buildResources(images []iconImage) resourceSection {
	// 先构造"要写的数据块"，再统一分配偏移。

	// ── 1. 每张图像一个 RT_ICON ──
	type dataBlob struct {
		bytes []byte
	}
	var iconBlobs []dataBlob
	for _, im := range images {
		iconBlobs = append(iconBlobs, dataBlob{bytes: im.data})
	}

	// ── 2. RT_GROUP_ICON 的内容（GRPICONDIR + entries）──
	// 结构：reserved(2) type(2)=1 count(2) 然后每项 14 字节：
	//   w(1) h(1) colors(1) reserved(1) planes(2) bitcount(2) bytesInRes(4) nID(2)
	grp := make([]byte, 0, 6+len(images)*14)
	grp = append(grp, 0, 0) // reserved
	grp = le16(grp, 1)      // type = icon
	grp = le16(grp, uint16(len(images)))
	for i, im := range images {
		w, h := byte(im.width), byte(im.height)
		if im.width == 256 {
			w = 0
		}
		if im.height == 256 {
			h = 0
		}
		grp = append(grp, w, h, byte(im.colorCount), 0)
		grp = le16(grp, uint16(im.planes))
		grp = le16(grp, uint16(im.bitCount))
		grp = le32(grp, uint32(len(im.data)))
		grp = le16(grp, uint16(iconResID+i)) // 指向第 i 个 RT_ICON
	}

	// ── 3. 计算各段起始偏移 ──
	//
	// 🔴 每一层目录都有自己的 **16 字节头**（Characteristics/TimeDateStamp/
	//    MajorVersion/MinorVersion/NumNamed/NumId 共 16 字节）。
	//    漏算任何一层的头都会整体错位 —— 我第一版就漏了，
	//    自检直接报 `目录长度算错（286 != 288）`，靠它抓出来的。
	//
	// 布局顺序（严格递增，且与实际写出的顺序一致）：
	//
	//	① 根目录                ：16 + 2*8
	//	② RT_ICON 类型目录       ：16 + N*8
	//	③ RT_GROUP_ICON 类型目录 ：16 + 1*8
	//	④ RT_ICON 各语言目录     ：16 + N*8
	//	⑤ RT_GROUP_ICON 语言目录 ：16 + 1*8
	//	⑥ 数据条目              ：(N+1)*16
	//	⑦ 实际数据              ：图标图像们（4 字节对齐）+ GRPICONDIR
	const dirHdr = 16
	nImages := len(images)

	rootOff := 0
	iconTypeOff := rootOff + dirHdr + 2*8            // ②
	groupTypeOff := iconTypeOff + dirHdr + nImages*8 // ③
	iconLangOff := groupTypeOff + dirHdr + 1*8       // ④
	groupLangOff := iconLangOff + dirHdr + nImages*8 // ⑤
	entryOff := groupLangOff + dirHdr + 1*8          // ⑥
	dataOff := entryOff + (nImages+1)*16             // ⑦

	// ── 4. 组装 ──
	buf := make([]byte, 0, dataOff+1024)

	// 根目录
	buf = dirHeader(buf, 2, 0) // 2 个 ID 项（RT_ICON / RT_GROUP_ICON）
	buf = appendDirEntries(buf, []dirEntry{
		dirEntryToDir(rtIcon, uint32(iconTypeOff)),
		dirEntryToDir(rtGroupIcon, uint32(groupTypeOff)),
	})

	// 类型目录 RT_ICON（N 个 ID 名称项）
	buf = dirHeader(buf, nImages, 0)
	var iconNameEntries []dirEntry
	for i := 0; i < len(images); i++ {
		iconNameEntries = append(iconNameEntries, dirEntryToDir(
			iconResID+i, uint32(iconLangOff+i*8)))
	}
	buf = appendDirEntries(buf, iconNameEntries)

	// 类型目录 RT_GROUP_ICON（1 个 ID 名称项）
	buf = dirHeader(buf, 1, 0)
	buf = appendDirEntries(buf, []dirEntry{dirEntryToDir(groupResID, uint32(groupLangOff))})

	// 名称目录 → 语言目录（RT_ICON 的 N 个）
	// ⚠️ 语言层的偏移指向**数据条目**，所以**不置**高位。
	buf = dirHeader(buf, nImages, 0)
	var iconLangEntries []dirEntry
	for i := 0; i < len(images); i++ {
		iconLangEntries = append(iconLangEntries, dirEntry{id: langID, offset: uint32(entryOff + i*16)})
	}
	buf = appendDirEntries(buf, iconLangEntries)

	// 名称目录 → 语言目录（RT_GROUP_ICON 的 1 个）
	buf = dirHeader(buf, 1, 0)
	buf = appendDirEntries(buf, []dirEntry{{id: langID, offset: uint32(entryOff + len(images)*16)}})

	// 数据条目：先算各 blob 的绝对偏移
	blobOffsets := make([]uint32, 0, nImages)
	pos := uint32(dataOff)
	for _, b := range iconBlobs {
		blobOffsets = append(blobOffsets, pos)
		pos += uint32(len(b.bytes))
		pos = align4(pos)
	}
	groupBlobOff := pos
	pos += uint32(len(grp))

	// 🔴 DataEntry 的 OffsetToData 字段**必须登记重定位**。
	//
	//	它的值不是"节内偏移"而是**该数据在被加载后的 RVA**
	//	（section 基址 + 节内偏移）。节基址只有链接器知道，
	//	所以必须留一个 `IMAGE_REL_AMD64_ADDR32NB(0x0003)` 重定位让它回填。
	//
	//	与 Go 自带 `rsrc_amd64.syso` 对照可见：它有一条
	//	`VirtualAddress=0x58, Type=3` 的重定位 —— 0x58 正是第一个
	//	DataEntry 的 OffsetToData 字段位置。
	//	**没有重定位 ⇒ Windows 读到的是节内偏移而非 RVA ⇒
	//	LoadImage 报 ERROR_BAD_EXE_FORMAT(193)、图标显示成默认占位图。**
	var relocs []reloc
	for i := range iconBlobs {
		// 每个 DataEntry 16 字节，OffsetToData 在其起始处
		fixupAt := uint32(entryOff + i*16)
		buf = dataEntry(buf, blobOffsets[i], uint32(len(iconBlobs[i].bytes)))
		relocs = append(relocs, reloc{offset: fixupAt, typeCode: relAmd64Addr32NB})
	}
	buf = dataEntry(buf, groupBlobOff, uint32(len(grp)))
	relocs = append(relocs, reloc{offset: uint32(entryOff + nImages*16), typeCode: relAmd64Addr32NB})

	// ── 5. 实际数据 ──
	if uint32(len(buf)) != uint32(dataOff) {
		fatal("内部错误：目录长度算错（%d != %d）", len(buf), dataOff)
	}
	for i, b := range iconBlobs {
		if uint32(len(buf)) != blobOffsets[i] {
			fatal("内部错误：blob %d 偏移错位", i)
		}
		buf = append(buf, b.bytes...)
		for len(buf)%4 != 0 {
			buf = append(buf, 0)
		}
	}
	if uint32(len(buf)) != groupBlobOff {
		fatal("内部错误：组图标偏移错位")
	}
	buf = append(buf, grp...)

	return resourceSection{data: buf, relocs: relocs}
}

type dirEntry struct {
	id     int
	offset uint32
}

// dirEntryToDir 构造"指向**子目录**"的条目。
//
// 🔴 必须把偏移的**最高位置 1**（IMAGE_RESOURCE_DATA_IS_DIRECTORY
//
//	= 0x80000000）：
//
//	Windows 靠这一位区分「该偏移指向子目录」与「指向数据条目」。
//	漏了它，整棵资源树会被当成数据条目解释 ——
//	表现为 `FindResource` 居然还能找到（名字表在，所以查得到），
//	但 `LoadImage` 报 **ERROR_BAD_EXE_FORMAT(193)**、图标显示成默认占位图。
//
//	⚠️ 这是我实测踩出来的：与 Go 自带的 `rsrc_amd64.syso` 逐字节对照，
//	  官方样本是 `0x80000018`，我的是 `0x00000020`（少了高位）。
//	  Go 自带的测试只断言"字节在二进制里"（`bytes.Contains`），
//	  **不验证 Windows 能否加载** ⇒ 它挡不住这个错。
func dirEntryToDir(id int, offset uint32) dirEntry {
	return dirEntry{id: id, offset: offset | 0x80000000}
}

// dirHeader 写一个 IMAGE_RESOURCE_DIRECTORY 头（固定 16 字节）。
//
//	Characteristics(4) TimeDateStamp(4) MajorVersion(2) MinorVersion(2)
//	NumberOfNamedEntries(2) NumberOfIdEntries(2)
//
// ⚠️ **每一层**目录（根/类型/名称）都要写这个头。
//
//	我第一版只给根目录写了，子目录直接写条目 ⇒ 每层少 16 字节、
//	总共少 48 字节。靠文件里的自检 `目录长度算错（304 != 352）` 抓出来 ——
//	**48 = 3 层 × 16 字节**，正是这个错。
func dirHeader(b []byte, numID, numNamed int) []byte {
	b = le32(b, 0) // Characteristics
	b = le32(b, 0) // TimeDateStamp
	b = le16(b, 0) // MajorVersion
	b = le16(b, 0) // MinorVersion
	b = le16(b, uint16(numNamed))
	b = le16(b, uint16(numID))
	return b
}

func appendDirEntries(b []byte, es []dirEntry) []byte {
	for _, e := range es {
		b = le32(b, uint32(e.id))
		b = le32(b, e.offset)
	}
	return b
}

// dataEntry 写一个 IMAGE_RESOURCE_DATA_ENTRY。
//
//	OffsetToData(4) Size(4) CodePage(4) Reserved(4)
func dataEntry(b []byte, off, size uint32) []byte {
	b = le32(b, off)
	b = le32(b, size)
	b = le32(b, 0) // CodePage
	b = le32(b, 0) // Reserved
	return b
}

func align4(v uint32) uint32 { return (v + 3) &^ 3 }

func le16(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}
func le32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// buildCOFF 把资源节包成 COFF 目标文件（.syso）。
//
// 结构（amd64）：
//
//	COFF 头 20 字节：Machine(2) NumberOfSections(2) TimeDateStamp(4)
//	                 PointerToSymbolTable(4) NumberOfSymbols(4)
//	                 SizeOfOptionalHeader(2) Characteristics(2)
//	节头 40 字节
//	节数据
//	重定位表（每条 10 字节）
//	符号表（本工具写 1 条 `.rsrc` 节符号）
//
// 🔴 必须有**重定位表 + 符号表**（2026-10-07 实测）：
//
//	资源 DataEntry 的 `OffsetToData` 存的是**加载后的 RVA**，
//	而节基址由链接器决定 ⇒ 必须留 `IMAGE_REL_AMD64_ADDR32NB` 重定位
//	让链接器回填。重定位项要引用一个符号（SymbolTableIndex），
//	所以符号表也不能空。
//
//	⚠️ 缺了重定位的后果（我实测踩到）：exe 里有 .rsrc 节、
//	  `FindResource` 甚至能找到条目，但 `LoadImage` 报
//	  **ERROR_BAD_EXE_FORMAT(193)**，图标显示成**默认占位图**。
//	  Go 自带的 rsrc 测试只 `bytes.Contains` 断言"字节在"，
//	  **验不出这类问题** —— 必须用真实 Win32 API 验证。
func buildCOFF(res resourceSection) []byte {
	rsrc := res.data
	const (
		headerSize = 20
		secHdrSize = 40
		relocSize  = 10
		symSize    = 18
	)
	secDataOff := headerSize + secHdrSize
	relocOff := secDataOff + len(rsrc)
	symOff := relocOff + len(res.relocs)*relocSize
	nSyms := 1

	out := make([]byte, 0, symOff+symSize*2)

	// COFF 头
	out = le16(out, 0x8664)         // Machine = amd64
	out = le16(out, 1)              // NumberOfSections
	out = le32(out, 0)              // TimeDateStamp（0 = 可复现构建）
	out = le32(out, uint32(symOff)) // PointerToSymbolTable
	out = le32(out, uint32(nSyms))  // NumberOfSymbols
	out = le16(out, 0)              // SizeOfOptionalHeader
	out = le16(out, 0x0004)         // Characteristics

	// 节头：.rsrc
	name := make([]byte, 8)
	copy(name, ".rsrc")
	out = append(out, name...)
	out = le32(out, 0)                       // VirtualSize
	out = le32(out, 0)                       // VirtualAddress
	out = le32(out, uint32(len(rsrc)))       // SizeOfRawData
	out = le32(out, uint32(secDataOff))      // PointerToRawData
	out = le32(out, uint32(relocOff))        // PointerToRelocations
	out = le32(out, 0)                       // PointerToLinenumbers
	out = le16(out, uint16(len(res.relocs))) // NumberOfRelocations
	out = le16(out, 0)                       // NumberOfLinenumbers
	// Characteristics：与 Go 官方 rsrc 样本一致
	//   0x40000040 已初始化数据|可读
	//   0x02000000 可丢弃（资源在加载后可由系统释放）
	//   0x80000000 / 0x10000000 是 binutils 写资源节的惯例位
	out = le32(out, 0xC0300040)

	// 节数据
	out = append(out, rsrc...)

	// 重定位表：每条 10 字节
	//   VirtualAddress(4) SymbolTableIndex(4) Type(2)
	for _, r := range res.relocs {
		out = le32(out, r.offset)
		out = le32(out, 0) // SymbolTableIndex = 0（指向唯一的 .rsrc 符号）
		out = le16(out, r.typeCode)
	}

	// 符号表：1 条节符号（18 字节）
	//   Name(8) Value(4) SectionNumber(2) Type(2) StorageClass(1) NumberOfAuxSymbols(1)
	symName := make([]byte, 8)
	copy(symName, ".rsrc")
	out = append(out, symName...)
	out = le32(out, 0)   // Value
	out = le16(out, 1)   // SectionNumber（1-based）
	out = le16(out, 0)   // Type
	out = append(out, 3) // StorageClass = IMAGE_SYM_CLASS_STATIC
	out = append(out, 0) // NumberOfAuxSymbols

	// 符号表之后还要 4 字节字符串表长度（空表）
	out = le32(out, 4)

	return out
}

// 保证 filepath 被使用（-o 可能带目录）
var _ = filepath.Base
