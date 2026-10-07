"""TAOAPI 图标 —— 精修版。

针对第一轮发现的三个问题改进：
  1. 16x16 下"圆环+闪电"太挤 ⇒ 小尺寸**单独用更简的构图**
     （Windows 会按尺寸选资源，所以 16/24 与 32+ 可以是不同设计）
  2. 闪电形状偏"通用电源符号" ⇒ 调整斜切角度，更像"电击/额度"
  3. 深色底在深色任务栏上会糊 ⇒ 加一圈极细的高光边

设计基准（来自项目现状）：
  · 面板 logo 是 ⚡（额度/速度）—— 保留
  · 项目是"反代网关"（本地中转）—— 用开口圆环表达
  · 主色 #16a34a / 深 #15803d / 亮 #86efac
"""

from PIL import Image, ImageDraw, ImageFilter
import math, os

GREEN = (22, 163, 74)
GREEN_D = (21, 128, 61)
GREEN_L = (134, 239, 172)
DARK = (28, 32, 38)
DARK2 = (22, 26, 31)
WHITE = (255, 255, 255)
EDGE = (60, 70, 80)


def bg(size, radius_ratio=0.22):
    """圆角底 + 极细高光边（深色任务栏上不糊）。"""
    img = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    r = int(size * radius_ratio)
    # 竖直渐变底
    grad = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    gd = ImageDraw.Draw(grad)
    for y in range(size):
        t = y / max(1, size - 1)
        c = tuple(int(DARK[i] * (1 - t) + DARK2[i] * t) for i in range(3))
        gd.line([(0, y), (size, y)], fill=c + (255,))
    mask = Image.new("L", (size, size), 0)
    ImageDraw.Draw(mask).rounded_rectangle([0, 0, size - 1, size - 1],
                                           radius=r, fill=255)
    img.paste(grad, (0, 0), mask)
    # 高光边
    d.rounded_rectangle([0, 0, size - 1, size - 1], radius=r,
                        outline=EDGE + (150,), width=max(1, size // 128))
    return img


def glow(size, cx_ratio=0.5, cy_ratio=0.44, radius_ratio=0.75,
         color=GREEN, max_alpha=95):
    layer = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    px = layer.load()
    cx, cy = size * cx_ratio, size * cy_ratio
    R = size * radius_ratio
    for y in range(size):
        for x in range(size):
            dist = math.hypot(x - cx, y - cy)
            if dist < R:
                t = 1.0 - dist / R
                px[x, y] = color + (int(max_alpha * t ** 2),)
    return layer


def bolt(size, cx_r, cy_r, h_r, w_r=None, color=WHITE, flip=False):
    """闪电形状。

    🔴 形状经过实测调整（第一版太"竖直"，32x32 下像一棵树）：
      关键是**上下两段要有明显水平错位**（经典闪电的折角），
      以及**下尖要更细更长**，才有"电击"的力量感。
    """
    layer = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    d = ImageDraw.Draw(layer)
    h = size * h_r
    w = size * (w_r if w_r else h_r * 0.62)
    cx, cy = size * cx_r, size * cy_r
    if flip:
        w = -w
    # 归一化坐标：上段向右压、下段向左甩，中间折角错位明显
    pts = [
        (0.72, 0.00),   # 右上角
        (0.06, 0.52),   # 中左（左甩）
        (0.44, 0.52),   # 折角内
        (0.20, 1.00),   # 下尖（细长）
        (0.94, 0.42),   # 中右
        (0.52, 0.42),   # 折角内
    ]
    poly = [(cx + (nx - 0.5) * w, cy + (ny - 0.5) * h) for nx, ny in pts]
    d.polygon(poly, fill=color)
    return layer


def ring(size, r_ratio=0.295, w_ratio=0.072, start=115, end=65,
         color=GREEN):
    layer = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    d = ImageDraw.Draw(layer)
    cx = cy = size / 2
    r = size * r_ratio
    d.arc([cx - r, cy - r, cx + r, cy + r], start=start, end=end,
          fill=color, width=max(2, int(size * w_ratio)))
    return layer


def make(size):
    """按尺寸选择构图（Windows 会按尺寸挑资源）。"""
    img = bg(size)
    img = Image.alpha_composite(img, glow(size))

    if size <= 24:
        # ── 小尺寸：只要"闪电"最清晰，不加环 ──
        # 托盘/任务栏小图标优先"一眼可辨"，细节全删
        b = bolt(size, 0.50, 0.51, 0.68, color=WHITE)
        img = Image.alpha_composite(img, b)
    else:
        # ── 中/大尺寸：开口圆环 + 闪电（环=网关中转，闪电=额度）──
        rg = ring(size)
        img = Image.alpha_composite(img, rg)
        # 闪电略偏左，避免与环开口打架
        b = bolt(size, 0.485, 0.50, 0.50, color=WHITE)
        img = Image.alpha_composite(img, b)

    return img


def make_preview(out_path):
    """生成对比预览：各尺寸实际渲染 + 浅/深两种背景。"""
    sizes = [16, 24, 32, 48, 64, 128, 256]
    pad = 20
    W = 256 + pad * 2 + 240
    H = 256 + 130
    canvas = Image.new("RGB", (W, H), (245, 246, 248))
    d = ImageDraw.Draw(canvas)

    # 左：256 大图
    big = make(256)
    canvas.paste(big, (pad, pad), big)

    # 右上：各尺寸并排（浅底）
    x = 256 + pad * 2
    y = pad
    for s in sizes:
        img = make(s)
        canvas.paste(img, (x, y), img)
        x += s + 14

    # 右下：深色背景下的小尺寸（模拟深色任务栏）
    dark_strip = Image.new("RGB", (240, 100), (32, 36, 42))
    canvas.paste(dark_strip, (256 + pad * 2, 256 - 40))
    x = 256 + pad * 2 + 10
    for s in (16, 24, 32, 48):
        img = make(s)
    # 单独画到深色条上
    for s in (16, 24, 32, 48):
        img = make(s)
        canvas.paste(img, (x, 256 - 30), img)
        x += s + 14

    canvas.save(out_path)


if __name__ == "__main__":
    outdir = os.path.join(os.environ.get("TEMP", "."), "taoapi-icon-final")
    os.makedirs(outdir, exist_ok=True)
    for s in (16, 24, 32, 48, 64, 128, 256):
        make(s).save(os.path.join(outdir, f"icon-{s}.png"))
    make_preview(os.path.join(outdir, "preview.png"))
    print("OK:", outdir)
