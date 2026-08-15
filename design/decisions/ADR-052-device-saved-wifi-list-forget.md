# ADR-052: 设置菜单新增「已保存 WiFi」列表——设备端逐条查看/忘记

- **日期**: 2026-08-15
- **状态**: 已接受（随本 ADR 一并落地实现）
- **组件**: firmware only（纯固件；cloud / adapter 零改动）
- **关联**:
  - `ADR-050`（WiFi 断网恢复与重新配网提案）——§4.5 已经设想过同一形态的「网络」二级页
    （当前状态 + 重新配网 + 已保存网络列表），本 ADR 只落地其中「已保存网络列表 + 忘记」
    这一片，不实现四级阶梯降级 / 免重启热切网 / 顶栏状态那部分，两者是正交的子集关系
  - `ADR-051`（设备端自助解绑与重置）——同一 `bb_ui_settings.c` 惯例（双击确认、行状态字段）
  - 既有存储层：`bb_wifi.c` 的 `load_saved_wifi_slot` / `delete_wifi_slot` /
    `BBCLAW_WIFI_MAX_SAVED`（`bb_config.h`，当前 8）——本 ADR 不改存储层语义，只加两个
    公开包装函数给 UI 用
  - 已有的同形态实现：HTTP 配网门户（`handle_saved_get` + `handle_delete_post`）——本 ADR
    是把同一能力搬到设备屏幕，两条入口并存，互不影响

## 1. 背景

`bb_wifi.c` 已经支持保存最多 `BBCLAW_WIFI_MAX_SAVED`（8）组 WiFi 凭据，并且门户网页
（设备进 AP 配网模式时手机连热点打开的页面）已经能列出全部已保存 SSID 并逐条删除。
但**设备本机屏幕的 Settings 菜单没有对应入口**——用户想清掉某个不再用的 WiFi（换路由器、
搬家、密码改了想清掉旧记录）只能整机 Reset Device（ADR-051，连账号绑定一起清）或者重新
进入配网门户手动删，都不是「设置里翻一下就能删一条」的直觉操作。

## 2. 决策

Settings 主菜单新增一行 `MAIN_ROW_WIFI_SAVED`（排在 Sleep 之后、SD 卡录音行之前，
所有模式常显——local_home 和 cloud_saas 都要联网）：

```
Saved WiFi (3/8)
```

点击进入新二级页 `LEVEL_WIFI_SAVED`：

```
* HomeWiFi          ← 当前正在用的连接前面标 "* "
  OfficeWiFi
  Hotspot-iPhone
```

- 点选中行 → 双击确认「忘记」（沿用 `MAIN_ROW_RECORDER`/`MAIN_ROW_MIYU` 已有的 5 秒双击
  确认惯用法：首击行文案变 `<ssid> · tap again to FORGET`，窗口内再点才真正调
  `bb_wifi_forget_saved()`）。误触代价是要重新输一遍密码，不算不可逆的破坏性操作，但和
  Recording/Miyu 保持一致的交互习惯，用户不用记两套规则。
- 光标移动到其它行会自动解除武装状态（同 recfiles 列表习惯，防止「武装了一行→划走→划回来
  →以为没武装其实点一下就删了」的误操作）。
- 列表为空时显示 `No saved networks`。
- 只有查看 + 忘记，**不支持在设备屏幕上新增/编辑 WiFi**（那仍然是配网门户的事——设备端没有
  文本输入能力，加密码输入框不现实）。

### 存储层：只加只读+删除的公开包装，不改语义

`bb_wifi.h` 新增两个函数，直接转调 `bb_wifi.c` 里已有的 static 存储层函数：

```c
esp_err_t bb_wifi_saved_get(int slot, char* ssid, size_t ssid_size);
esp_err_t bb_wifi_forget_saved(int slot);
```

- `bb_wifi_saved_get` = `load_saved_wifi_slot` 的公开版，丢弃密码只回 SSID（UI 不需要也不该
  拿到密码明文）。
- `bb_wifi_forget_saved` = `delete_wifi_slot` 的公开版，行为不变（含既有的槎位压缩逻辑）。
- 不修改 `BBCLAW_WIFI_MAX_SAVED`、不改 NVS key 结构、不改压缩/查找逻辑——存储层是成熟代码，
  本 ADR 只是给它加一个设备 UI 消费入口。

### 为什么不現在做 ADR-050 §4.5 的完整「网络」页

ADR-050 提议的「网络」页还包含「当前连接状态（哪一级阶梯）」和「重新配网」两个入口，
依赖 ADR-050 本身的四级阶梯降级机制（还处于「提议待评审」状态，未实现）。用户这次的需求
只是「列表 + 删除」，与阶梯降级正交，不必等 ADR-050 落地才能做。若 ADR-050 之后落地，
`MAIN_ROW_WIFI_SAVED` 这一行和 `LEVEL_WIFI_SAVED` 这个页可以直接作为其「已保存网络」子项
复用，不冲突。

## 3. 影响范围

- `firmware/include/bb_wifi.h`：+2 个函数声明
- `firmware/src/bb_wifi.c`：+2 个函数实现（薄包装，无新逻辑）
- `firmware/src/bb_ui_settings.c`：+1 个 `main_row_t` 值、+1 个 `settings_level_t` 值、
  +状态字段（列表缓存 + 双击确认时间戳）、+render/enter 函数、+click/back/rotate 分支
- Cloud / Adapter：零改动（纯设备本地菜单 + 本地 NVS 操作，不涉及任何协议）

## 4. 验收

- `make build` 通过（已验证，无新增警告）。
- 真机验证 checklist（未做，待用户或下一次真机会话跑）：
  - [ ] 有 ≥2 个已保存网络时，列表正确显示，当前连接项带 `*` 前缀
  - [ ] 双击确认忘记一条 → 该条从列表消失、槎位号压缩、其余条目不受影响
  - [ ] 忘记正在使用的那条 → 不影响当前连接（只是下次断线重连/开机不会再选它）
  - [ ] 光标移动到其它行 → 武装状态解除，不会误删旁边一条
  - [ ] 列表为空（全部清空后）→ 显示 `No saved networks`，BACK 正常返回主菜单
