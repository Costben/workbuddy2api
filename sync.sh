#!/bin/bash
set -euo pipefail

REPO_OWNER="ithtelab"
REPO_NAME="workbuddy-manager"
RELEASE_TAG="upstream-src"
ASSET_NAME="workbuddy2api-src.tar.gz"

echo "=== 1. 获取 ithtelab 最新快照元数据 ==="
DOWNLOAD_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${RELEASE_TAG}/${ASSET_NAME}"

mkdir -p .tmp_sync
curl -fsSL -o .tmp_sync/latest-src.tar.gz "$DOWNLOAD_URL"

NEW_SHA=$(sha256sum .tmp_sync/latest-src.tar.gz | awk '{print $1}')
echo "最新快照 SHA256: $NEW_SHA"

OLD_SHA=""
if [ -f .upstream-snapshot-sha256 ]; then
  OLD_SHA=$(cat .upstream-snapshot-sha256 | tr -d '[:space:]')
fi
echo "当前记录 SHA256: $OLD_SHA"

if [ "$NEW_SHA" = "$OLD_SHA" ] && [ "${FORCE_SYNC:-0}" != "1" ]; then
  echo "✅ 快照与当前完全一致，无需更新。"
  rm -rf .tmp_sync
  exit 0
fi

echo "=== 2. 检测到上游快照变动，开始同步 ==="
# 备份自定义关键路径
mkdir -p .tmp_sync/preserved
cp -r patches .tmp_sync/preserved/
cp -r .github/workflows .tmp_sync/preserved/workflows
cp .gitattributes .tmp_sync/preserved/
cp sync.sh .tmp_sync/preserved/

# 清除现有源码文件（保留 .git 和 .tmp_sync）
find . -mindepth 1 -maxdepth 1 -not -name '.git' -not -name '.tmp_sync' -exec rm -rf {} +

# 解包新源码
tar -xzf .tmp_sync/latest-src.tar.gz --strip-components=1

# 还原自定义关键路径
cp -r .tmp_sync/preserved/patches ./
mkdir -p .github/workflows
cp -r .tmp_sync/preserved/workflows/* .github/workflows/
cp .tmp_sync/preserved/.gitattributes ./
cp .tmp_sync/preserved/sync.sh ./
rm -rf .tmp_sync

# 统一行尾为 LF
find . -type f -not -path "*/.git/*" -not -path "*/patches/*" -name "*.go" -exec perl -pi -e 's/\r\n/\n/g' {} +
find . -type f -not -path "*/.git/*" -not -path "*/patches/*" -name "*.sh" -exec perl -pi -e 's/\r\n/\n/g' {} +
find . -type f -not -path "*/.git/*" -not -path "*/patches/*" -name "*.json" -exec perl -pi -e 's/\r\n/\n/g' {} +
find . -type f -not -path "*/.git/*" -not -path "*/patches/*" -name "*.md" -exec perl -pi -e 's/\r\n/\n/g' {} +
find . -type f -not -path "*/.git/*" -not -path "*/patches/*" -name "*.yml" -exec perl -pi -e 's/\r\n/\n/g' {} +

echo "=== 3. 逐个应用 patches/ 目录下的补丁 ==="
PATCH_FAILED=0
for patch_file in patches/*.patch; do
  [ -e "$patch_file" ] || continue
  echo "正在应用补丁: $patch_file"
  if git apply --check --ignore-whitespace "$patch_file" 2>&1; then
    git apply --ignore-whitespace "$patch_file"
    echo "✅ 成功应用 $patch_file"
  else
    echo "❌ 补丁冲突: $patch_file 无法干净应用！"
    PATCH_FAILED=1
  fi
done

if [ "$PATCH_FAILED" -ne 0 ]; then
  echo "🚨 存在冲突补丁，终止同步并保持退出码非 0！"
  exit 1
fi

echo "$NEW_SHA" > .upstream-snapshot-sha256
echo "=== 4. 同步并补丁完成，准备提交 ==="
