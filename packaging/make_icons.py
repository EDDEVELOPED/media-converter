"""Render the app icon and write .ico (Windows) and .icns (Mac)."""
import sys
from playwright.sync_api import sync_playwright
from PIL import Image

out = sys.argv[1]
# Mac icons sit inside a 1024 canvas with ~10% margin; the glyph matches the in-app mark.
svg = """<svg xmlns='http://www.w3.org/2000/svg' width='1024' height='1024' viewBox='0 0 1024 1024'>
<defs><linearGradient id='g' x1='0' y1='0' x2='0' y2='1'><stop offset='0' stop-color='#FFD43B'/><stop offset='1' stop-color='#F0BC0F'/></linearGradient></defs>
<rect x='100' y='100' width='824' height='824' rx='190' fill='url(#g)'/>
<path d='M300 330v364l252-182z' fill='#18212B'/>
<rect x='608' y='420' width='72' height='184' rx='36' fill='#18212B'/>
<rect x='724' y='338' width='72' height='348' rx='36' fill='#18212B'/>
</svg>"""
with sync_playwright() as pw:
    b = pw.chromium.launch()
    pg = b.new_page(viewport={"width": 1024, "height": 1024})
    pg.set_content(f"<html><body style='margin:0;background:transparent'>{svg}</body></html>")
    pg.screenshot(path=f"{out}/icon-1024.png", omit_background=True, clip={"x": 0, "y": 0, "width": 1024, "height": 1024})
    b.close()

img = Image.open(f"{out}/icon-1024.png").convert("RGBA")
img.save(f"{out}/AppIcon.icns")
# Windows: crop the margin so the icon fills its tile.
win = img.crop((96, 96, 928, 928)).resize((256, 256), Image.LANCZOS)
win.save(f"{out}/app.ico", sizes=[(16, 16), (20, 20), (24, 24), (32, 32), (40, 40), (48, 48), (64, 64), (128, 128), (256, 256)])
print("icons written")
