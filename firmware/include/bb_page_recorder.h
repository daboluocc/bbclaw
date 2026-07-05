#pragma once

/**
 * 录音模式页（ADR-044 §3.6）：常显录音指示（合规刚性——红点+时长不可关闭）。
 * 极简暗屏构图：AMOLED 黑底只亮少量像素，功耗友好。
 * show/hide 需在 LVGL 锁内调用（radio_app 的 RECORDER 态驱动）。
 */
void bb_page_recorder_show(void);
void bb_page_recorder_hide(void);
/** BACK 第一按提示「再滑一次停止」；arm=1 显示，0 隐藏。锁内调用。 */
void bb_page_recorder_exit_hint(int arm);
