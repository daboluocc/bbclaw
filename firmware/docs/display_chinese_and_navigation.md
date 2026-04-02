# 屏幕中文与导航

## 中文字库

生成 CJK 字库的唯一入口：

```bash
make gen-lv-font
```

字符子集维护在 [lvgl_font_subset_chars.txt](./lvgl_font_subset_chars.txt)。

可选环境变量见 `scripts/gen_lvgl_cjk_font.py`（`LV_FONT_SIZE`、`LV_FONT_BPP`、`LV_FONT_SUBSET`、`LV_FONT_LITE_LIMIT`）。

构建要点：

- `sdkconfig.defaults` 须开启 `CONFIG_LV_USE_FONT_COMPRESSED` 和 `CONFIG_LV_FONT_FMT_TXT_LARGE`
- 全量字库 app 约 1.6MB+，需 `partitions_bbclaw.csv`（factory 3MB）+ 8MB Flash

## 导航 API

| API | 行为 |
| --- | --- |
| `bb_display_chat_prev_turn()` | 查看更早一轮 |
| `bb_display_chat_next_turn()` | 回到较新一轮 |
| `bb_display_chat_scroll_down()` / `scroll_up()` | 当前轮内上下滚动长文 |
| `bb_display_chat_focus_me()` / `focus_ai()` | 切换滚动作用栏（默认 AI） |

新对话到达时会切回最新一轮并清零滚动。勿在 ISR 内直接调用，应投递到 UI 任务。
