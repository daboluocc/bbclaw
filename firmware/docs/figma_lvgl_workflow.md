# Figma -> LVGL Workflow (ST7789 320x172)

目标：

- `Figma` 作为唯一 UI 设计源
- 从 Figma 直接导出 PNG / SVG 图层元素
- 由仓库脚本把导出物转成 LVGL 可用资源

## 设计源

当前工作文件：

- [BBClaw ST7789 Landscape UI](https://www.figma.com/design/YB7P6zFrpKncMwhvz9dvXf)

设计约束见：

- `ui/figma/README.md`

## 目录约定

1. Figma 导出的整页 PNG：
- `ui/lvgl/screens/`

2. screen PNG 与 LVGL 符号映射：
- `ui/lvgl/screens_manifest.json`

3. Figma 导出的可复用 PNG 元素：
- `ui/lvgl/elements/`（含第三方切片如 LimeZu 待机吉祥物，见 [lvgl_limezu_mascots.md](lvgl_limezu_mascots.md)）

4. element PNG 与 LVGL 符号映射：
- `ui/lvgl/elements_manifest.json`

5. Figma 导出的可复用 SVG：
- `assets/svg/`

6. 生成的 LVGL 资源：
- `generated/bb_lvgl_screen_assets.c`
- `generated/bb_lvgl_screen_assets.h`
- `generated/bb_lvgl_element_assets.c`
- `generated/bb_lvgl_element_assets.h`
- `src/bb_lvgl_assets.c`
- `include/bb_lvgl_assets.h`

## Figma 里怎么拆素材

1. 整页视觉：
- 只用于启动页、关于页、复杂背景、整页插画
- 导出 `PNG`

2. 可复用图层元素：
- 如状态图标、logo、波形装饰、小按钮图标
- 优先导出 `SVG`

3. 独立 PNG 图层元素：
- 只在矢量效果不稳定、滤镜无法保留时使用
- 例如带复杂发光的局部装饰图

## 导出规范

### 整页 PNG

- Frame size 固定为 `320 x 172`
- Export scale 固定为 `1x`
- 文件名必须与 manifest 对齐

当前约定：

- `01-standby.png` -> `bb_screen_standby`
- `02-notification.png` -> `bb_screen_notification`
- `03-history.png` -> `bb_screen_history`
- `04-speaking.png` -> `bb_screen_speaking`
- `05-settings.png` -> `bb_screen_settings`
- `06-about.png` -> `bb_screen_about`

但当前 `BBClaw` 的本地联调主线是：

- 优先使用 LVGL 原生组件跑通 screen 结构
- 只接入少量共享元素图片
- 暂不把整页 PNG 作为默认依赖

设计规格见：

- `docs/lvgl_landscape_ui_spec.md`
- `ui/lvgl/asset_catalog.md`

### SVG 图层

- 文件名使用语义化命名
- 统一放到 `assets/svg/`
- 继续走已有的 `make gen-lvgl-assets`

## 转成 LVGL 资源

### SVG -> LVGL

```bash
cd firmware
make gen-lvgl-assets
```

这个流程适合：

- logo
- 状态 icon
- 可复用小图形

### PNG -> LVGL

```bash
cd firmware
make gen-lvgl-screens
```

这个流程适合：

- 整页背景
- 启动画面
- 大插画
- 从 Figma 导出的整页静态页面

如果后面要导入“非整页尺寸”的 PNG 图层元素，也可以复用同一脚本，执行：

```bash
cd firmware
make gen-lvgl-elements
```

## LVGL 使用方式

整页 PNG：

```c
#include "bb_lvgl_screen_assets.h"

lv_obj_t* img = lv_image_create(lv_screen_active());
lv_image_set_src(img, &bb_screen_standby);
lv_obj_align(img, LV_ALIGN_CENTER, 0, 0);
```

公共 SVG 资源转换后的图标：

```c
#include "bb_lvgl_assets.h"

lv_obj_t* icon = lv_image_create(lv_screen_active());
lv_image_set_src(icon, &bb_img_ready);
```

## 设计边界

- 不要把消息正文、时间、电量、网络状态烤进整页 PNG
- 不要让 Figma 导出图承担动态内容
- 动态内容应保留给 LVGL 文本和布局代码

这样素材更新和界面逻辑可以分开维护，不会每改一个状态文案就重出整张图。
