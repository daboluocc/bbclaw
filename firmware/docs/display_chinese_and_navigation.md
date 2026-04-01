# 屏幕中文、多轮与滚动

## 1. 为什么当前自绘屏显示不了中文？

旧实现 `bb_display_bitmap.c` 使用 **5×7 位图点阵**，按 **ASCII** 逐字节选字形；UTF-8 多字节字符会被画成 `?`，且没有 CJK 字模数据。

要 **正确显示中文**，业界通用做法是：

| 路线 | 说明 |
| --- | --- |
| **LVGL + 子集字库（推荐）** | 用 [LVGL Font Converter](https://docs.lvgl.io/master/tools/font_converter.html) 从思源黑体/文泉驿等生成 **只含常用汉字 + ASCII** 的 `lv_font_t`，体积约 **200KB～1MB**（视字号与字数而定）。通过 `esp_lvgl_port` 把现有 ST7789 接到 LVGL，聊天区用 `lv_label` + `LV_LABEL_LONG_WRAP / SCROLL` 即可换行与滚动。 |
| **字表（本仓库）** | 已生成 **去重 UTF-8 字串**（约 3700+ 字，含标点与对讲/设备用语），见 **[lvgl_font_subset_chars.txt](./lvgl_font_subset_chars.txt)**。可手搓网页粘贴；或见下 **§1.1 一键生成**。若 Flash 紧张，用 `subset=lite` 或截取前 **800～1500 字**。 |
| **整库点阵** | 把 GB2312/常用字做成位图打进 Flash，体积大、维护难，一般不推荐。 |
| **运行时矢量** | FreeType + 缓存，RAM/Flash 开销大，小固件少见。 |

### 1.1 本地一键生成（与官网同款引擎，无需打开网页）

网页 [lvgl.io/tools/fontconverter](https://lvgl.io/tools/fontconverter) 与命令行 **`lv_font_conv`**（npm，[lvgl/lv_font_conv](https://github.com/lvgl/lv_font_conv)）是同一类转换器。脚本 [gen_lvgl_cjk_font.py](../scripts/gen_lvgl_cjk_font.py) 自动下载 **Noto Sans SC**（SIL OFL，来自 `@fontsource/noto-sans-sc` 的 **woff**；当前 `lv_font_conv@1.5.3` **不支持 woff2**，故固定用 woff），并按 `lvgl_font_subset_chars.txt` 生成 C 文件：

```bash
cd firmware
make gen-lv-font
```

生成：`firmware/generated/lv_font_bbclaw_cjk.c`（已加入仓库根 `.gitignore`，需本地生成后再参与编译）。

**依赖**：**Node.js**（`npx`）、**Python 3**。首次会从 unpkg 下载字体到 `firmware/.cache/fonts/`。

**可选环境变量**：`LV_FONT_SIZE`（默认 14）、`LV_FONT_BPP`（默认 4）、`LV_FONT_SUBSET`=`full`|`lite`、`LV_FONT_LITE_LIMIT`（默认 1500，仅 `lite`）。示例：`LV_FONT_SUBSET=lite LV_FONT_LITE_LIMIT=1200 make gen-lv-font`

**结论**：中文属于 **换显示栈（LVGL）+ 字体资源** 的问题，不是改几行 `draw_char` 能根治的。本仓库 `firmware/ui/lvgl/images/` 可作为产品 UI 参考；**运行时**若需中文，应以 **LVGL 为主线** 做一版迁移。

**构建注意**：

- 组件依赖写在 **`firmware/src/idf_component.yml`**（`esp_lvgl_port` + `lvgl`），由 IDF 拉取到 `managed_components/`。
- 全量字库时 app 约 **1.6MB+**：使用 **`partitions_bbclaw.csv`**（factory **3MB**）且 **`CONFIG_ESPTOOLPY_FLASHSIZE_8MB`**（与常见 8MB 芯片一致；勿写 4MB header 配 8MB 片，会告警）。
- `src` 组件定义 **`LV_LVGL_H_INCLUDE_SIMPLE`**，与 `lv_font_conv` 生成的 `#include` 分支一致。
- **`sdkconfig.defaults`** 须开启 **`CONFIG_LV_USE_FONT_COMPRESSED`**（`lv_font_conv` 默认压缩位图；否则 LVGL 无法解压，**英文也会方框**）；全量大字库再开 **`CONFIG_LV_FONT_FMT_TXT_LARGE`**（避免 `bitmap_index` 溢出乱码）。

### 1.2 界面（`bb_lvgl_display.c`）

横屏 320×172：深色底、上下两块「气泡」左色条（我=青绿 / 答=蓝）+ 圆角浅底，**顶栏左侧**为 `assets/svg` 转换的 **状态图标**（READY/TX/RX/ERR 等），**底栏左侧**为小 **logo**；正文为 **`我:` / `答:`** 前缀 + 换行。更新图标：`make gen-lvgl-assets`（依赖 Node `sharp-cli`、Python Pillow）。

## 2. 当前固件已做的铺垫（无 LVGL 时）

- **UTF-8 按码点推进**：换行、滚动行数统计时 **不会在汉字中间截断**（仍显示为 `?`，直到接入 CJK 字体）。
- **多轮历史**：环形保留最近 `BBCLAW_DISPLAY_CHAT_HISTORY` 轮（`bb_config.h`）。
- **左右/上下导航 API**：供 GPIO、旋钮或侧键调用（勿在 ISR 里直接刷屏，应投递到 UI 任务）。

## 3. 导航 API（需在输入侧接线）

| API | 行为 |
| --- | --- |
| `bb_display_chat_prev_turn()` | 查看 **更早一轮**（左/上一则） |
| `bb_display_chat_next_turn()` | 回到 **较新** 一轮 |
| `bb_display_chat_scroll_down()` / `scroll_up()` | 在当前轮 **ME** 或 **AI** 栏内上下滚动长文 |
| `bb_display_chat_focus_me()` / `focus_ai()` | 滚动条作用在 **自己** 或 **回复** 栏（默认 **AI**） |

新对话到达时会 **切回最新一轮** 并 **清零滚动**。

## 4. 与后续 LVGL 的关系

多轮与滚动的 **产品逻辑**（保留几轮、当前查看索引、滚动偏移）可复用到 LVGL：`lv_label` 的 `long_mode` 与 `lv_obj_scroll_*` 承担绘制；本模块的 **状态机** 仍可保留为「业务层」单例，仅把「渲染」从自绘换成 LVGL。
