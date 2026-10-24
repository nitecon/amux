"""Reproduce the native icon from the header's teal terminal mark (Pillow)."""
from pathlib import Path
from PIL import Image, ImageDraw

root = Path(__file__).resolve().parents[1] / "assets" / "icons"
size = 1024
image = Image.new("RGBA", (size, size))
mask = Image.new("L", (size, size))
ImageDraw.Draw(mask).rounded_rectangle((32, 32, 992, 992), radius=205, fill=255)
for y in range(size):
    t = y / (size - 1)
    color = tuple(round(a + (b - a) * t) for a, b in zip((34, 211, 238), (20, 184, 166)))
    ImageDraw.Draw(image).line((0, y, size, y), fill=(*color, 255))
image.putalpha(mask)
draw = ImageDraw.Draw(image)
ink = (11, 15, 23, 255)
points = [(245, 295), (490, 540), (245, 785)]
draw.line(points, fill=ink, width=65, joint="curve")
draw.line([(570, 785), (815, 785)], fill=ink, width=65)
for x, y in points + [(570, 785), (815, 785)]:
    draw.ellipse((x - 32, y - 32, x + 32, y + 32), fill=ink)
image = image.resize((256, 256), Image.Resampling.LANCZOS)
image.save(root / "amux.png")
image.save(root / "amux.ico", sizes=[(n, n) for n in (16, 24, 32, 48, 64, 128, 256)])
(root / "amux.rgba").write_bytes(image.resize((64, 64), Image.Resampling.LANCZOS).tobytes())
