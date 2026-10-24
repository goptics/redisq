"""Paused-frame contact sheets of an animated SVG, light and dark, via headless Chrome. Python 3 stdlib only.

    python3 frames.py diagram.svg [--at 0,2.5,6] [--n 11] [--cols 3] [--width 480] [--out DIR]

Writes <out>/<stem>-light.png and <stem>-dark.png (default <out>: frames/ next to the SVG; keep it out of git)
and prints their paths. Default: n evenly spaced frames over
the loop plus a "seam" frame just before the end (compare it with t=0). Chrome: $CHROME, else found on PATH.
"""
import argparse
import base64
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

DARK = re.compile(r"\(\s*prefers-color-scheme\s*:\s*dark\s*\)")


def chrome():
    names = [os.environ.get("CHROME", ""), "google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome",
             "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
             r"C:\Program Files\Google\Chrome\Application\chrome.exe",
             r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe"]
    for n in names:
        if n and (shutil.which(n) or Path(n).is_file()):
            return shutil.which(n) or n
    sys.exit("frames.py: Chrome not found; install Google Chrome or Chromium, or set CHROME=/path/to/chrome")


def frame(svg, t, dark):
    """SVG paused at t seconds, theme forced (no reliance on the OS or browser setting)."""
    s = DARK.sub("all" if dark else "not all", svg)
    pause = f".a{{animation-play-state:paused!important;animation-delay:-{t:g}s!important}}"
    s = s.replace("</style>", pause + "</style>", 1)
    return "data:image/svg+xml;base64," + base64.b64encode(s.encode()).decode()


def sheet(svg, times, labels, dark, cols, width, png, exe):
    vb = re.search(r'viewBox="\s*[-\d.]+\s+[-\d.]+\s+([\d.]+)\s+([\d.]+)', svg)
    w, h = (float(vb[1]), float(vb[2])) if vb else (960, 420)
    cell_h = round(width * h / w)
    gap, cap = 16, 22
    rows = -(-len(times) // cols)
    W, H = gap + cols * (width + gap), gap + rows * (cell_h + cap + gap)
    bg, fg = ("#0d1117", "#e6edf3") if dark else ("#ffffff", "#1f2328")
    cells = "".join(f'<figure><img src="{frame(svg, t, dark)}"><figcaption>{lb}</figcaption></figure>'
                    for t, lb in zip(times, labels))
    html = f"""<!doctype html><meta charset="utf-8"><style>
html,body{{margin:0;background:{bg};color:{fg};font:13px ui-monospace,Menlo,Consolas,monospace}}
main{{display:grid;grid-template-columns:repeat({cols},{width}px);gap:{gap}px;padding:{gap}px}}
figure{{margin:0}} img{{display:block;width:{width}px;height:{cell_h}px}} figcaption{{height:{cap}px;line-height:{cap}px}}
</style><main>{cells}</main>"""
    page = Path(tempfile.mkdtemp(prefix="frames-")) / "sheet.html"
    page.write_text(html)
    subprocess.run([exe, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--virtual-time-budget=3000",
                    f"--window-size={W},{H}", f"--screenshot={png}", page.as_uri()],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    shutil.rmtree(page.parent, ignore_errors=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("svg")
    ap.add_argument("--at", help="comma-separated seconds, e.g. 0,2.5,6 (default: evenly spaced + seam)")
    ap.add_argument("--n", type=int, default=11, help="evenly spaced frames when --at is not given")
    ap.add_argument("--cols", type=int, default=3)
    ap.add_argument("--width", type=int, default=480, help="px per frame")
    ap.add_argument("--out", help="folder for the PNGs (default: frames/ next to the SVG)")
    a = ap.parse_args()

    svg = Path(a.svg).read_text()
    dur = re.search(r"--dur:\s*([\d.]+)s", svg)
    if not dur:
        sys.exit(f"frames.py: no '--dur: <seconds>s' in {a.svg}; is it generated from a kfu template?")
    D = float(dur[1])
    if a.at:
        times = [float(t) for t in a.at.split(",")]
        labels = [f"t={t:g}s" for t in times]
    else:
        times = [round(D * i / a.n, 2) for i in range(a.n)] + [round(D - .05, 2)]
        labels = [f"t={t:g}s" for t in times[:-1]] + [f"t={times[-1]:g}s (seam, ~t=0)"]

    exe = chrome()
    a.out = a.out or str(Path(a.svg).resolve().parent / "frames")
    os.makedirs(a.out, exist_ok=True)
    stem = Path(a.svg).name.removesuffix(".svg")
    for theme in ("light", "dark"):
        png = os.path.join(a.out, f"{stem}-{theme}.png")
        sheet(svg, times, labels, theme == "dark", a.cols, a.width, png, exe)
        print(png)


if __name__ == "__main__":
    main()
