#!/bin/sh
# 打包 Windows + macOS（Intel / Apple 芯片通用）单文件程序，输出到 dist/
#
#   ./scripts/build.sh               版本号默认用打包时间，如 2026.10.14-2130
#   VERSION=1.2.0 ./scripts/build.sh
set -e
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(date +%Y.%m.%d-%H%M)}
LDFLAGS="-s -w -X main.Version=$VERSION"
mkdir -p dist
export CGO_ENABLED=0
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/kefu-windows.exe ./cmd/kefu
GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o dist/kefu-mac-arm64 ./cmd/kefu
GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/kefu-mac-amd64 ./cmd/kefu
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/kefu-linux-amd64 ./cmd/kefu
if command -v lipo >/dev/null 2>&1; then
  lipo -create -output dist/kefu-mac dist/kefu-mac-arm64 dist/kefu-mac-amd64
  rm dist/kefu-mac-arm64 dist/kefu-mac-amd64
fi
echo "版本 $VERSION"
ls -lh dist
