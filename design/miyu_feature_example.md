# 蜜语功能开关示例

## 概述

蜜语功能通过云端配置 `miyu_enabled` 控制启用/禁用。设备端通过 `bb_device_config_get()` 读取配置并应用到功能逻辑中。

## 使用示例

### 1. 在代码中检查蜜语开关

```c
#include "bb_device_config.h"

void some_feature_function(void) {
  const bb_device_config_t* config = bb_device_config_get();
  
  if (config->miyu_enabled) {
    // 蜜语功能启用：执行蜜语相关逻辑
    ESP_LOGI(TAG, "miyu feature enabled");
    // TODO: 实现蜜语功能的具体逻辑
  } else {
    // 蜜语功能禁用：跳过或使用默认行为
    ESP_LOGI(TAG, "miyu feature disabled");
  }
}
```

### 2. 在 TTS 播放时应用蜜语效果

```c
// 示例：在 bb_audio.c 或相关音频处理模块中
#include "bb_device_config.h"

esp_err_t bb_audio_play_tts(const char* text) {
  const bb_device_config_t* config = bb_device_config_get();
  
  // 根据配置调整音频参数
  int volume = config->volume_pct;
  int speed = config->speed_ratio_x10;
  
  if (config->miyu_enabled) {
    // 应用蜜语音效：可能包括音调调整、音色变化等
    // TODO: 实现蜜语音效处理
    ESP_LOGI(TAG, "applying miyu audio effects");
  }
  
  // 播放 TTS 音频
  return play_audio_with_params(text, volume, speed);
}
```

### 3. 在消息处理时应用蜜语文本转换

```c
// 示例：在 bb_ui_agent_chat.c 或消息处理模块中
#include "bb_device_config.h"

static void process_assistant_message(const char* text) {
  const bb_device_config_t* config = bb_device_config_get();
  
  const char* display_text = text;
  char* miyu_text = NULL;
  
  if (config->miyu_enabled) {
    // 应用蜜语文本转换：可能包括语气词添加、表情符号等
    miyu_text = apply_miyu_text_transform(text);
    if (miyu_text != NULL) {
      display_text = miyu_text;
    }
  }
  
  // 显示消息
  display_message(display_text);
  
  if (miyu_text != NULL) {
    free(miyu_text);
  }
}
```

## 云端配置更新

### Web Portal 更新配置

```bash
# 启用蜜语功能
curl -X POST https://bbclaw.daboluo.cc/v1/devices/BBClaw-0.4.1-C7EB89/config \
  -H "Authorization: Bearer $SESSION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"miyu_enabled": true}'

# 禁用蜜语功能
curl -X POST https://bbclaw.daboluo.cc/v1/devices/BBClaw-0.4.1-C7EB89/config \
  -H "Authorization: Bearer $SESSION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"miyu_enabled": false}'
```

### 配置生效流程

1. Web Portal 调用 API 更新配置
2. Cloud 保存配置并推送 `config.update` 消息给在线设备
3. 设备收到消息，解析并应用配置
4. 配置持久化到 NVS
5. 下次调用 `bb_device_config_get()` 时返回新配置

## 配置持久化

配置自动持久化到 NVS：
- Namespace: `bbclaw`
- Key: `device/config`
- 设备重启后自动加载

## 注意事项

1. **实时生效**：配置更新通过 WebSocket 实时推送，无需重启设备
2. **离线支持**：配置持久化到 NVS，设备离线时使用本地缓存
3. **版本控制**：配置带版本号，防止旧消息覆盖新配置
4. **线程安全**：`bb_device_config_get()` 返回只读指针，可在任意任务中安全调用

## 蜜语功能的具体实现

蜜语功能的具体实现取决于产品需求，可能包括：

- **音频效果**：音调调整、音色变化、背景音效
- **文本转换**：语气词添加、表情符号、语言风格转换
- **交互行为**：动画效果、LED 灯光、震动反馈

根据实际需求在相应模块中实现，并通过 `config->miyu_enabled` 控制启用/禁用。
