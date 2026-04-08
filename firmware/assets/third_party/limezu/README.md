# LimeZu — Animated mini characters (third-party art)

Source: [Animated mini characters \[Platform\] \[FREE\]](https://limezu.itch.io/animated-mini-characters) by **LimeZu**.

BBClaw uses **derivative slices** from the free pack only:

- Colors in repo: **Red**, **Blue**, **Green** (Idle animation, top row — 4 frames × 32×32 px each).
- **Not** included: paid **Violet** character; original `.zip` / full sprite sheets are **not** committed.

## License (itch.io page, summary)

- Usable in **free and commercial** projects; **modifications** allowed.
- **Do not redistribute** the asset pack as-is or **resell** it. This repository ships **cropped frame PNGs** and LVGL-converted binaries for BBClaw firmware only; if your policy requires zero pixel redistribution, regenerate PNGs locally with `scripts/slice_limezu_mini_idle.py` and exclude them from git.

## Regenerating slices

From the firmware directory, with the itch download extracted (e.g. `Free Mini Characters/`):

```bash
python3 scripts/slice_limezu_mini_idle.py --src /path/to/Free\ Mini\ Characters --out ui/lvgl/elements
make gen-lvgl-elements
```

LVGL RGB565 assets are produced by `scripts/gen_lvgl_screen_assets_from_png.py` via `make gen-lvgl-elements` (see `ui/lvgl/elements_manifest.json`).
