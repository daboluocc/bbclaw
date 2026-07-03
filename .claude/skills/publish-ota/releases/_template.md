# Release vX.Y.Z (YYYY-MM-DD)

## 发布方式
- [ ] 灰度发布（本地脚本 release_local.sh）
- [ ] 正式发布（Tag + GitHub Actions）

## 执行命令

### 前置环境
```bash
export OTA_ADMIN_KEY=<从私有运维渠道获取，勿写入仓库>
get_idf  # 或 source ~/esp/esp-idf/export.sh
```

### 发布命令
```bash
# 复制此块，粘贴到终端执行
cd firmware
./scripts/release_local.sh vX.Y.Z [--skip-build] [--gh-release] [--no-ota]
```

## 前置状态

| 项目 | 值 |
|------|-----|
| Git branch | main |
| Git commit | `<git rev-parse --short HEAD>` |
| Git log | 见下方关键改动 |
| IDF 版本 | v5.5.2 |
| 板子配置 | sdkconfig.bbclaw.latest ✓ |
| 固件大小 | ~2.3-2.4 MB |
| PSRAM | OCTAL |

## 关键改动

**来自 CHANGELOG.md 或 git log:**
```
feat(firmware): xxxxx
fix(firmware): xxxxx
```

或链接到 GitHub:
https://github.com/daboluocc/bbclaw/compare/vX.Y.Z-1..main

## OTA 服务端状态

| 项 | 值 |
|----|-----|
| 推送到 | https://bbclaw.daboluo.cc/v1/ota/flash-bundle |
| Bundle 版本 | vX.Y.Z |
| 平台 | esp32s3 |
| Active 切换 | ✓ 已切换 / ⋯ 灰度中 / ✗ 未推送 |
| 灰度用户 | @user1, @user2 (可选) |
| 预期升级时间 | 设备下次心跳自动拉取 (~15 min) |

## 验证方式

```bash
# 服务端验证（查看 active bundle）
curl https://bbclaw.daboluo.cc/v1/ota/flash-bundles?platform=esp32s3 | jq '.data[] | select(.isActive)'

# 设备端验证
# 进入 Settings → Check Update → 出现 vX.Y.Z 且可升级
```

## 回滚计划

**若问题严重需要立即回滚：**

```bash
# 1. 在能登云端的机器上，停用本版本 (设置 inactive)
ssh <部署主机> <<'CMD'
  curl -X PUT https://localhost:8443/v1/ota/flash-bundles/esp32s3/vX.Y.Z/deactivate \
    -H "X-OTA-Admin-Key: <KEY>"
CMD

# 2. 设备侧救回
#    - 若卡 boot loop: `make boot-recover` 回 factory (UART0 烧录)
#    - 等 factory 起来后，下次心跳会拉旧版本（或最新好版本）
```

**下一版本(vX.Y.Z+1) 会自动停用本版本**

## 备注

- 灰度期间如有反馈: _______________
- 已知限制: _______________
- 后续跟进: _______________
