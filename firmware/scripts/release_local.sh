#!/usr/bin/env bash
# 本地发版：本机构建固件 + 生成 OTA bundle + 直接推到 OTA 服务器，跳过 GitHub Actions
# 慢 round-trip（且本机/CI toolchain 差异不再卡发版）。复刻 .github/workflows/release.yml
# 的 firmware 段：build → otadata → stage → POST /v1/ota/flash-bundle。
#
# 用法：
#   get_idf                          # 先把 ESP-IDF 环境加载好（提供 cmake/ninja/python）
#   export OTA_ADMIN_KEY=...          # 见下方「获取 admin key」
#   firmware/scripts/release_local.sh v0.4.19        # 构建并推 OTA
#   firmware/scripts/release_local.sh v0.4.19 --skip-build   # 复用已构建产物，只推 OTA
#   firmware/scripts/release_local.sh v0.4.19 --gh-release   # 顺带把 4 个 bin 传到 GitHub Release
#   firmware/scripts/release_local.sh v0.4.19 --no-ota       # 只构建+stage，不推 OTA
#
# 获取 admin key：从私有运维渠道取（部署主机 / 密钥路径见 firmware/.release.local，
# 不写入仓库），或直接 `export OTA_ADMIN_KEY=<key>` 后运行。
set -euo pipefail

FW_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$FW_DIR"

OTA_URL="${OTA_URL:-https://bbclaw.daboluo.cc}"
SKIP_BUILD=0
DO_OTA=1
DO_GH=0
VERSION=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-build) SKIP_BUILD=1; shift ;;
    --no-ota)     DO_OTA=0; shift ;;
    --gh-release) DO_GH=1; shift ;;
    -h|--help)    sed -n '2,22p' "$0"; exit 0 ;;
    -*)           echo "unknown option: $1" >&2; exit 1 ;;
    *)            VERSION="$1"; shift ;;
  esac
done

if [[ -z "$VERSION" ]]; then
  # 默认本地发版版本（约定 2026-06-25）：查 OTA 当前 active bundle 版本 → patch+1 →
  # 追加 -g<短哈希>[-dirty]。OTA 的 VersionGreater 只比较 M.m.p、忽略后缀，所以 patch 必须
  # 真的 +1 才能让设备升级；后缀只作本地构建的可追溯标记（来自哪个 commit / 是否 dirty）。
  # 不依赖 git tag（git tag 可能落后于已推 OTA 的版本）。查询失败则从 v0.0.0 起。
  ACTIVE="$(curl -fSs "$OTA_URL/v1/ota/flash-bundles?platform=esp32s3" 2>/dev/null \
    | jq -r '[.data[]|select(.isActive)|.version][0] // empty' 2>/dev/null || true)"
  BASE="${ACTIVE#v}"; [[ -z "$BASE" ]] && BASE="0.0.0"
  IFS=. read -r VMAJ VMIN VPAT <<<"$BASE"
  VPAT="${VPAT%%[!0-9]*}"               # patch 段的前导数字（丢掉任何 -后缀）
  : "${VMAJ:=0}" "${VMIN:=0}" "${VPAT:=0}"
  SHORT="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"
  DIRTY=""; git diff --quiet HEAD 2>/dev/null || DIRTY="-dirty"
  VERSION="v${VMAJ}.${VMIN}.$((VPAT+1))-g${SHORT}${DIRTY}"
  echo "[release-local] 未传版本 → 按 OTA active(${ACTIVE:-none}) patch+1 自动定: $VERSION"
fi

STAGE="$FW_DIR/release-stage/firmware"

if [[ $SKIP_BUILD -eq 0 ]]; then
  # 与 release.yml 一致：bbclaw 板配置（OCT PSRAM / cloud_saas / 生产 URL），把版本号
  # 写进 esp_app_desc.version（否则所有设备上报同一版本 → OTA 循环）。
  echo "[release-local] 应用 sdkconfig.bbclaw.latest + version.txt=$VERSION"
  cp sdkconfig.bbclaw.latest sdkconfig
  printf '%s' "$VERSION" > version.txt
  echo "[release-local] make build …（需先 get_idf 加载 ESP-IDF 环境）"
  # 显式钉死发布版本，覆盖 Makefile stamp-version 的 git describe 默认，避免被改写。
  make build FW_VERSION="$VERSION"
fi

# 生成指向 ota_0(seq=1) 的 otadata —— 默认全 0xFF 会指 factory，放不下 2.36MB 固件。
# 瞬时且确定性，--skip-build 也要做（stage 依赖它）。
echo "[release-local] 生成 otadata_ota0.bin"
python3 -c "
import binascii, struct, os
seq = 1
entry = struct.pack('<I20sII', seq, b'\xff'*20, 0xFFFFFFFF,
                    binascii.crc32(struct.pack('I', seq), 0xFFFFFFFF) % (1<<32))
os.makedirs('build', exist_ok=True)
open('build/otadata_ota0.bin','wb').write(entry + b'\xff'*(4096-len(entry)) + b'\xff'*4096)
print('[otadata] ota_0 seq=1')
"

# Stage 四个 bin（版本化命名，与 release 资产一致）
mkdir -p "$STAGE"
cp build/bootloader/bootloader.bin        "$STAGE/bootloader-${VERSION}.bin"
cp build/partition_table/partition-table.bin "$STAGE/partition-table-${VERSION}.bin"
cp build/otadata_ota0.bin                 "$STAGE/ota-data-${VERSION}.bin"
cp build/bbclaw_firmware.bin              "$STAGE/bbclaw-firmware-${VERSION}-esp32s3.bin"
echo "[release-local] staged → $STAGE"
( cd "$STAGE" && shasum -a 256 ./*.bin )

PART_BOOT="bootloader-${VERSION}.bin"
PART_PT="partition-table-${VERSION}.bin"
PART_OTA="ota-data-${VERSION}.bin"
PART_APP="bbclaw-firmware-${VERSION}-esp32s3.bin"

if [[ $DO_OTA -eq 1 ]]; then
  if [[ -z "${OTA_ADMIN_KEY:-}" ]]; then
    echo "::warning:: OTA_ADMIN_KEY 未设置 —— 跳过 OTA 推送（产物已 stage）。" >&2
    echo "  设置后重跑：export OTA_ADMIN_KEY=<从私有运维渠道获取>（见 firmware/.release.local.example）" >&2
  else
    MANIFEST=$(jq -nc --arg v "$VERSION" --arg b "$PART_BOOT" --arg p "$PART_PT" --arg o "$PART_OTA" --arg a "$PART_APP" \
      '{version:$v, platform:"esp32s3", chipFamily:"ESP32-S3",
        parts:[{offset:"0x0",filename:$b},{offset:"0x8000",filename:$p},
               {offset:"0x10000",filename:$o},{offset:"0x120000",filename:$a}]}')
    echo "[release-local] 推 flash bundle → $OTA_URL/v1/ota/flash-bundle"
    curl -fSs -X POST "$OTA_URL/v1/ota/flash-bundle" \
      -H "X-OTA-Admin-Key: $OTA_ADMIN_KEY" \
      -F "manifest=$MANIFEST" \
      -F "${PART_BOOT}=@${STAGE}/${PART_BOOT}" \
      -F "${PART_PT}=@${STAGE}/${PART_PT}" \
      -F "${PART_OTA}=@${STAGE}/${PART_OTA}" \
      -F "${PART_APP}=@${STAGE}/${PART_APP}"
    echo ""
    echo "[release-local] OTA bundle 已推送：$VERSION —— 设备下次 /v1/ota/check 即可拉到。"
  fi
fi

if [[ $DO_GH -eq 1 ]]; then
  echo "[release-local] gh release：上传 4 个 bin 到 $VERSION"
  command -v gh >/dev/null || { echo "gh 未安装，跳过" >&2; exit 0; }
  gh release view "$VERSION" --repo daboluocc/bbclaw >/dev/null 2>&1 \
    || gh release create "$VERSION" --repo daboluocc/bbclaw --title "$VERSION" --notes "local release"
  gh release upload "$VERSION" --repo daboluocc/bbclaw --clobber \
    "$STAGE/$PART_BOOT" "$STAGE/$PART_PT" "$STAGE/$PART_OTA" "$STAGE/$PART_APP"
fi

echo "[release-local] 完成：$VERSION"
