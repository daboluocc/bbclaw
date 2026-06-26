---
name: publish-ota
description: "发布 BBClaw 固件 OTA 版本。支持两条路径：(A) 本地脚本快速灰度发布（无 CI、直推 OTA 服务器）；(B) 打 tag 触发 GitHub Actions 正式发布（固件 + adapter 5 平台二进制 + GitHub Release）。Triggers: \"发布\", \"publish\", \"OTA\", \"release\", \"灰度\", \"beta\", \"打 tag\", \"发版\", \"推 OTA\", \"出版本\", \"出固件\", \"ota发布\", \"本地发版\"."
---

# BBClaw OTA 发布

两条发布路径，按需选择：

| | 路径 A：本地脚本 | 路径 B：Tag + CI |
|---|---|---|
| 适合 | 灰度/快速验证 | 正式对外发布 |
| 触发 | `release_local.sh` | `git tag + push` |
| 产物 | OTA server 直接可见 | GitHub Release + OTA |
| 需要 | OTA_ADMIN_KEY + IDF 环境 | push 权限（用户确认） |
| Adapter 二进制 | 不构建 | ✓ 5 平台 |

---

## 路径 A：本地脚本快速发布

### 前提

```bash
# 1. 加载 ESP-IDF 环境（需 get_idf alias 或手动 source）
get_idf   # 或 source ~/esp/esp-idf/export.sh

# 2. 获取 OTA admin key（在能 ssh 到云端的机器上）
export OTA_ADMIN_KEY=$(ssh root@daboluo.cc \
  "grep '^CLOUD_OTA_ADMIN_KEY=' /opt/bbclaw-cloud/config/cloud.env | cut -d= -f2-")
```

### 发布命令

```bash
cd firmware

# 构建 + 推 OTA（版本号不传则自动: OTA active 版本 patch+1 + git hash）
./scripts/release_local.sh v0.5.2

# 常用 flag 组合
./scripts/release_local.sh v0.5.2 --skip-build   # 复用上次 build 产物，只推 OTA
./scripts/release_local.sh v0.5.2 --gh-release    # 同时上传 4 个 bin 到 GitHub Release
./scripts/release_local.sh v0.5.2 --no-ota        # 只构建 + stage，不推 OTA
./scripts/release_local.sh                        # 不传版本 → 自动定版本号
```

### 脚本内部做什么

1. 应用 `sdkconfig.bbclaw.latest`（bbclaw 板 + OCTAL PSRAM + cloud_saas）
2. 将版本号写入 `version.txt`（注入 `esp_app_desc.version`，防无限 OTA 循环）
3. `make build FW_VERSION=<version>`
4. 生成指向 ota_0 的 `otadata_ota0.bin`（默认 0xFF 会指 factory，装不下 2.5MB 固件）
5. Stage 4 个 bin：`bootloader / partition-table / ota-data / bbclaw-firmware`
6. `POST /v1/ota/flash-bundle` → `https://bbclaw.daboluo.cc`

### 发布后确认

```bash
# 设备 Settings → Check Update，或等下次心跳自动拉取
# 服务端验证（可选）
curl https://bbclaw.daboluo.cc/v1/ota/flash-bundles?platform=esp32s3 | jq '.data[] | select(.isActive)'
```

---

## 路径 B：Tag + GitHub Actions 正式发布

> ⚠️ **打 tag + push 会触发 CI，构建固件 + 5 平台 adapter 二进制，并推送 OTA。操作前需用户确认。**

### 何时打 tag

**打 tag 的条件（满足其一即可）：**
- 固件有用户可见的新功能或 bug fix
- adapter 二进制有功能变更或 fix

**不要打 tag：**
- 仅 cloud/web 端改动（服务端部署，不需要设备更新）
- 内部重构、文档改动、无用户可见变化

### 操作

```bash
# 1. 确认 main 已包含要发布的所有 commit 且 push 了
git log --oneline origin/main..HEAD   # 应该为空，或只有不需要发版的 commit

# 2. 打 tag 并推（推 tag 触发 release.yml）
git tag v0.5.2
git push origin v0.5.2
```

### CI 产物（release.yml）

- 固件：`bbclaw-firmware-v0.5.2-esp32s3.bin` + bootloader + partition-table + otadata
- Adapter：`bbclaw-adapter_v0.5.2_{darwin,linux,windows}_{amd64,arm64}` 5 平台
- `SHA256SUMS` 文件
- GitHub Release 挂载以上全部 artifact
- 固件 bundle 推到 OTA server（设备心跳可拉到）

### CI 用的构建约束（不要在本地手动绕过）

| 约束 | 原因 |
|------|------|
| 用 `sdkconfig.bbclaw.latest` | `sdkconfig.defaults` 是 QUAD PSRAM（breadboard），刷到 bbclaw PCB 会 boot loop |
| 版本从 tag 注入（`version.txt`） | 否则 esp_app_desc.version 硬编码 → 设备永远报旧版 → 无限 OTA 循环 |
| device_id = `BBClaw-<MAC>`（不含版本号） | 含版本号 → 每次 OTA 设备身份变化 → 云端当新设备要求重新配对 |

---

## 回滚

OTA 只换 app slot，factory 分区不变可救急：

```bash
# 擦 otadata → 下次 boot 回 factory 槽
make boot-recover

# 注意：若云端 active bundle 是坏固件，factory 起来会再 OTA 变砖
# → 先确认云端 active 是好版本（发新 tag 会自动停用旧版）
```

---

## 常见问题

**`OTA_ADMIN_KEY 未设置 —— 跳过 OTA 推送`**  
→ 按上面「前提」设置 env var 后重跑（加 `--skip-build` 跳过重新编译）。

**build 失败：`project was configured with ... python_env`**  
→ `make fullclean && make init && make build`（Python 环境路径失效）。

**固件大小超分区限制**  
→ 确认用的是 `partitions_ota.csv`（ota_0 = 2.5MB），`sdkconfig.bbclaw.latest` 已启用。

**设备升级后无限重启（boot loop）**  
→ 先 `make boot-recover` 回 factory，再确认云端 active 版本正常，再发新 tag。
