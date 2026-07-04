---
name: publish-ota
description: "发布 BBClaw 固件 OTA 版本。统一走 tag → GitHub Actions：打 v* tag → CI 构建固件 + adapter 5 平台二进制 + GitHub Release + 推 OTA（用 CI 托管的 OTA_ADMIN_KEY secret）。版本号即 tag（无仓库版本文件）。本地不再直推 OTA。Triggers: \"发布\", \"publish\", \"OTA\", \"release\", \"beta\", \"打 tag\", \"发版\", \"推 OTA\", \"出版本\", \"出固件\", \"ota发布\"."
---

# BBClaw OTA 发布

**唯一发布路径：tag → GitHub Actions。** 打 `v*` tag 触发 `.github/workflows/release.yml`，
在云上用生产配置构建固件 + 5 平台 adapter 二进制，建 GitHub Release，并用 CI 托管的
`OTA_ADMIN_KEY` secret 把固件 bundle 推到 OTA server。

> 本地手动直推 OTA 的老路径（`release_local.sh` / `make release-local`）已于 2026-07-04
> 移除——本机不再需要持有生产 OTA admin key，也不会被覆盖 sdkconfig。`make flash` 仍可
> 烧连着的板子（那是本地烧录，不是 OTA 推送）。

---

## 发布流程

```bash
# 1. 确认要发布的所有 commit 都在 origin/main 上（CI 从 tag 指向的 commit 构建）
git log --oneline origin/main..HEAD   # 应为空；有则先 git push

# 2. 打 tag 并推 → 触发 release.yml（版本号即 tag，自己选下一个语义化版本）
git tag v0.6.2            # 上一个是 v0.6.1 → patch+1
git push origin v0.6.2
```

> ⚠️ **打 tag + push 会触发 CI 构建并推送 OTA（全量：所有查更新的设备都会被推该版本，
> 单 active 模型）。操作前需用户确认。**

### 何时发

**发（满足其一即可）：**
- 固件有用户可见的新功能或 bug fix
- adapter 二进制有功能变更或 fix

**不发：**
- 仅 cloud/web 端改动（服务端部署，不需要设备更新）
- 内部重构、文档改动、无用户可见变化

### 版本号从哪来

- **单一真相来源 = git tag**（无仓库版本文件）。发版就是选一个新 tag，`vMAJOR.MINOR.PATCH`。
- CI 发版时 `release.yml` 用 `FW_VERSION=<tag>` 钉死版本写进 `esp_app_desc.version`。
- 本地 `make build` 用 `git describe --tags --dirty --always` 自动推导（如
  `v0.6.1-3-g7544427-dirty`：最近 tag + 落后提交数 + 脏标记），无需手工维护版本。
- 新 tag 必须 **> 当前 OTA active**（单 active 模型 + 只比 M.m.p）。查当前 active：
  `curl "$OTA_SERVER_URL/v1/ota/flash-bundles?platform=esp32s3" | jq '.data[]|select(.isActive)|.version'`

### 监控 CI

```bash
gh run list --repo daboluocc/bbclaw --workflow release.yml --limit 3
gh run watch <run-id> --repo daboluocc/bbclaw
```

---

## CI 产物（release.yml）

- 固件：`bbclaw-firmware-<tag>-esp32s3.bin` + bootloader + partition-table + otadata
- Adapter：`bbclaw-adapter_<tag>_{darwin,linux,windows}_{amd64,arm64}` 5 平台
- `SHA256SUMS`
- GitHub Release 挂载以上全部 artifact
- 固件 bundle 推到 OTA server（设备心跳可拉到）

CI 已配置好 `secrets.OTA_ADMIN_KEY` + `vars.OTA_SERVER_URL`，无需本地提供。

### CI 用的构建约束（勿改回退）

| 约束 | 原因 |
|------|------|
| 用 `sdkconfig.bbclaw.latest` | `sdkconfig.defaults` 是 QUAD PSRAM（breadboard），刷到 bbclaw PCB 会 boot loop |
| 版本从 tag 注入（`FW_VERSION` → `version.txt`） | 否则 esp_app_desc.version 硬编码 → 设备永远报旧版 → 无限 OTA 循环 |
| device_id = `BBClaw-<MAC>`（不含版本号） | 含版本号 → 每次 OTA 设备身份变化 → 云端当新设备要求重新配对 |

---

## 发布记录

发布后记一份，方便下次对比（可选但推荐）：

```bash
cat > .claude/skills/publish-ota/releases/v0.6.1.md <<'EOF'
# Release v0.6.1 (2026-07-04)

## 命令
git tag v0.6.1 && git push origin v0.6.1

## 前置状态
- git: <commit> (main, pushed)

## 关键改动
- fix(firmware): 云健康检查移出输入循环，弱网按键不再卡死
- fix(firmware): WiFi 配网页 4→8 槽位 + 满槽提示 + 删除失败可见

## CI / OTA
- run: gh run list --repo daboluocc/bbclaw --workflow release.yml
- active: curl "$OTA_SERVER_URL/v1/ota/flash-bundles?platform=esp32s3" | jq '.data[]|select(.isActive)'

## 回滚计划
- 严重问题：make boot-recover 回 factory；再发更高版本盖掉（单 active 自动停用旧版）
EOF
```

历史查看：`ls -ltr .claude/skills/publish-ota/releases/`

---

## 回滚

OTA 只换 app slot，factory 分区不变可救急：

```bash
make boot-recover   # 擦 otadata → 下次 boot 回 factory 槽
```

> 若云端 active bundle 是坏固件，factory 起来会再 OTA 变砖 → 先确认云端 active 是好
> 版本，或发一个**更高**版本的修复固件盖掉（单 active 模型会自动停用旧版）。

---

## 常见问题

**CI 里 `OTA_ADMIN_KEY / OTA_SERVER_URL not configured — skipping OTA push`**
→ 仓库 secret/variable 没配好。检查 `gh secret list` 有 `OTA_ADMIN_KEY`、
`gh variable list` 有 `OTA_SERVER_URL`。

**设备升级后无限重启（boot loop）**
→ 先 `make boot-recover` 回 factory，再确认云端 active 版本正常，再发新 tag。

**tag 打错 / 想重发**
→ 删除远端 tag（`git push origin :v0.6.1`）+ 本地 tag（`git tag -d v0.6.1`）后重打；
或直接打下一个版本 tag 重发（更干净，避免 tag 复用）。
