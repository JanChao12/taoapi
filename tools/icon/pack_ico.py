"""把生成的 PNG 打包成 Windows .ico（**DIB 格式，非 PNG 条目**）。

🔴 为什么必须用 DIB 而不是 PNG 条目（2026-10-07 实测）：
    现代图标工具默认把每张图塞成内嵌 PNG（Vista+ 扩展）。
    实测在 Windows 托盘上：`LoadImage` 会**返回句柄却渲染成坏图**
    （只有顶部一小条有内容），或直接报 ERROR_RESOURCE_NAME_NOT_FOUND。
    ⇒ 必须写成传统 DIB：BITMAPINFOHEADER + 自下而上 BGRA + AND 掩码。

ICO 文件格式：
  ICONDIR        : reserved(2)=0 type(2)=1 count(2)
  ICONDIRENTRY[] : w(1) h(1) colors(1) reserved(1) planes(2) bitcount(2)
                   bytesInRes(4) imageOffset(4)
  然后各图像数据（本脚本写 DIB）

⚠️ 只有 16/24/32/48 用 DIB；256 也用 DIB（保证全尺寸一致，
   避免"某些尺寸能显示、某些不能"这种最难查的不一致）。
"""

import os
import struct
from PIL import Image

SIZES = [16, 24, 32, 48, 64, 128, 256]


def to_dib(img: Image.Image) -> bytes:
    """把 RGBA 图像转成 ICO 里的 DIB 数据（BITMAPINFOHEADER + BGRA + 掩码）。"""
    img = img.convert("RGBA")
    w, h = img.size
    px = img.load()

    # BITMAPINFOHEADER（40 字节）
    # ⚠️ biHeight 必须是 2*h —— ICO 惯例：DIB 里含 AND 掩码的高度
    header = struct.pack(
        "<IiiHHIIiiII",
        40,          # biSize
        w,           # biWidth
        h * 2,       # biHeight（ICO 惯例）
        1,           # biPlanes
        32,          # biBitCount
        0,           # biCompression = BI_RGB
        0,           # biSizeImage
        0, 0,        # 分辨率
        0, 0,        # 调色板
    )

    # 像素：自下而上、BGRA
    rows = []
    for y in range(h - 1, -1, -1):
        row = bytearray()
        for x in range(w):
            r, g, b, a = px[x, y]
            row += bytes((b, g, r, a))
        rows.append(bytes(row))
    pixels = b"".join(rows)

    # AND 掩码：1bpp，每行按 4 字节对齐。32bpp 下透明由 alpha 决定，
    # 掩码置 0 即可（但**必须存在**，否则 DIB 尺寸不符）
    mask_stride = ((w + 31) // 32) * 4
    mask = b"\x00" * (mask_stride * h)

    return header + pixels + mask


def build_ico(png_dir: str, out_path: str):
    images = []
    for s in SIZES:
        p = os.path.join(png_dir, f"icon-{s}.png")
        if not os.path.exists(p):
            raise SystemExit(f"缺少 {p}")
        with Image.open(p) as im:
            images.append((s, to_dib(im.convert("RGBA"))))

    entries = []
    offset = 6 + 16 * len(images)
    blob = b""
    for s, data in images:
        # 256 在字段里写 0（1 字节放不下 256）
        wbyte = 0 if s >= 256 else s
        hbyte = 0 if s >= 256 else s
        entries.append(struct.pack(
            "<BBBBHHII",
            wbyte, hbyte,
            0,       # colors（32bpp 时为 0）
            0,       # reserved
            1,       # planes
            32,      # bitcount
            len(data),
            offset,
        ))
        blob += data
        offset += len(data)

    header = struct.pack("<HHH", 0, 1, len(images))
    with open(out_path, "wb") as f:
        f.write(header)
        for e in entries:
            f.write(e)
        f.write(blob)

    return out_path


if __name__ == "__main__":
    import sys
    src = os.path.join(os.environ.get("TEMP", "."), "taoapi-icon-final")
    out = os.path.join(src, "taoapi.ico")

    # 允许只打部分尺寸（内嵌进 exe 时用，避免 exe 多出 ~300KB）
    if len(sys.argv) > 1:
        SIZES = [int(x) for x in sys.argv[1].split(",")]
        out = sys.argv[2] if len(sys.argv) > 2 else out

    build_ico(src, out)
    print("ICO:", out, os.path.getsize(out), "字节")
