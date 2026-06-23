# Adapter 设置页面 Driver Panel 改进方案

## 设计目标

1. **提升信息密度**：在有限空间展示更多有用信息（capabilities、model、安装状态）
2. **强化视觉层级**：当前激活 driver 更突出，状态一目了然
3. **改善交互体验**：添加帮助提示、确认流程、快速说明

## 改进点

### 1. Driver 卡片布局改进

**从简单 chip 升级为信息卡片**：

```
┌─────────────────────────────────────────┐
│ claude-code                    ✓ 当前   │
│ Claude Code Official           Model: opus-4-8 │
│ ✓ butler  ✓ resume  ✓ streaming         │
└─────────────────────────────────────────┘

┌─────────────────────────────────────────┐
│ opencode                       已安装    │
│ Open Source Agent              Model: - │
│ ✓ butler  ✗ resume  ✓ streaming         │
└─────────────────────────────────────────┘

┌─────────────────────────────────────────┐
│ aider                          未安装    │
│ Terminal-based AI coding       Model: - │
│ ✗ butler  ✗ resume  ✗ streaming         │
└─────────────────────────────────────────┘
```

### 2. 信息展示增强

**每个 driver 卡片包含**：
- **Driver 名称**：左上，粗体
- **状态徽章**：右上（当前/已安装/未安装）
- **描述文本**：第二行，灰色小字
- **Active Model**：右侧显示当前使用的模型（如果有）
- **Capabilities 图标行**：第三行，用图标+文字展示能力
  - ✓ butler: 支持设备管家模式
  - ✓ resume: 支持会话恢复
  - ✓ streaming: 支持流式响应

### 3. 视觉层级优化

**状态层级**：
1. **当前激活**：accent 边框 + accent 背景色（10%透明度）+ ✓ 徽章
2. **已安装可用**：ghost 边框 + hover 高亮
3. **未安装**：ghost 边框 + 降低透明度 + 禁用点击

**配色方案**（基于 Daboluo 设计系统）：
- 激活：`border-color: var(--accent)`, `background: rgba(46,196,160,.10)`
- 可用：`border-color: var(--ghost)`, hover → `border-color: var(--dim)`
- 禁用：`opacity: .45`

### 4. 交互体验改进

**切换确认流程**：
```javascript
async function switchDriver(target) {
  if (target === activeDriver) return;
  
  // 如果有进行中的会话，给予警告
  const confirmed = confirm(
    `切换到 ${target} 会重置对话连续性，确定继续？\n\n` +
    `下一轮对话将使用新驱动。`
  );
  
  if (!confirmed) return;
  
  await setActiveDriver(target);
  // ... 刷新状态
}
```

**快速帮助**：
- 每个 capability 有 tooltip 说明
- Driver 卡片可以展开查看详细信息（可选）

### 5. 警告和提示改进

**当前问题展示**：
- opencode 版本不兼容 → 卡片右上角红点 + 底部警告面板
- 非 butler-capable 被选中 → 明确提示回落机制
- 未安装的 driver → 提供安装指引链接

**警告面板样式**：
```
⚠ opencode 警告
当前安装的 opencode 版本不支持 serve 模式，
建议升级：pip install --upgrade opencode
```

## 实现清单

### 前端改动

1. **DriversPanel.vue**：
   - 重构卡片布局（从 chip 改为 card）
   - 添加 capabilities 展示
   - 添加 active_model 展示
   - 改进交互逻辑（确认、tooltip）

2. **theme.css**：
   - 新增 `.driver-card` 样式
   - 新增 `.capability-badge` 样式
   - 改进状态标识样式

### 后端改动

3. **api.ts**：
   - 补充 DriverRow 类型的描述字段（如果后端返回）
   - 确保 active_model、capabilities 都能正确传递

### 可选增强

4. **Driver 描述配置**：
   - 可以在前端维护一个 driver 描述字典
   - 或者后端在注册时提供描述

5. **安装状态检测增强**：
   - 提供"重新检测"按钮
   - 显示最后检测时间

## 视觉稿（ASCII）

```
┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
┃ 驱动（设备管家用哪个 CLI）                ┃
┣━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┫
┃ 整套都用激活的驱动：设备管家、派活的      ┃
┃ worker、记忆都跟随它。                    ┃
┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛

┌─────────────────────────────────────────┐◄─ accent 边框
│ claude-code          [✓ 当前] [opus-4-8]│
│ Claude Code Official                     │
│ ✓ 管家  ✓ 恢复  ✓ 流式                  │
└─────────────────────────────────────────┘

┌─────────────────────────────────────────┐◄─ ghost 边框
│ opencode                    [已安装]  ⚠ │
│ Open Source Agent                        │
│ ✓ 管家  ✗ 恢复  ✓ 流式                  │
└─────────────────────────────────────────┘

┌─────────────────────────────────────────┐◄─ ghost 边框 + 降低透明度
│ aider                       [未安装]     │
│ Terminal-based AI coding                 │
│ ✗ 管家  ✗ 恢复  ✗ streaming              │
└─────────────────────────────────────────┘

⚠ opencode：当前版本不支持 serve 模式，建议升级
⚠ 当前驱动不支持管家，设备管家实际使用：claude-code
```

## 兼容性考虑

- 保持 API 契约不变（`/v1/admin/drivers`）
- 后端不返回描述时，前端使用默认描述
- capabilities 缺失时降级为简单显示

## 迁移路径

1. **Phase 1**：改进卡片布局和视觉层级（不破坏现有功能）
2. **Phase 2**：添加 capabilities 展示
3. **Phase 3**：添加交互增强（确认、tooltip、帮助）
