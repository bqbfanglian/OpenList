#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
target_os="${1:-windows}"
target_arch="${2:-amd64}"
case "$target_os/$target_arch" in windows/amd64|linux/amd64|linux/arm64) ;; *) echo 'Supported: windows amd64, linux amd64, linux arm64' >&2; exit 1;; esac
frontend_sha='88aba5260646ed784847a11e473880d749b17b80af53caf895fe1f31f36aa3e3'
archive="$(mktemp)"
trap 'rm -f "$archive"' EXIT
curl -fL --retry 3 'https://github.com/OpenListTeam/OpenList-Frontend/releases/download/v4.2.7/openlist-frontend-dist-v4.2.7.tar.gz' -o "$archive"
echo "$frontend_sha  $archive" | sha256sum -c -
mkdir -p public/dist
tar -xzf "$archive" -C public/dist
destination="build/feemoo/$target_os-$target_arch"
mkdir -p "$destination"
binary='openlist'
if [[ "$target_os" == windows ]]; then binary='openlist.exe'; fi
commit="$(git rev-parse --short HEAD)"
built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -tags=jsoniter -trimpath \
  -ldflags="-s -w -X github.com/OpenListTeam/OpenList/v4/internal/conf.Version=v4.2.7-feemoo -X github.com/OpenListTeam/OpenList/v4/internal/conf.WebVersion=v4.2.7 -X github.com/OpenListTeam/OpenList/v4/internal/conf.GitCommit=$commit -X github.com/OpenListTeam/OpenList/v4/internal/conf.BuiltAt=$built_at" \
  -o "$destination/$binary" .
cp drivers/feemoo/README.md "$destination/使用说明.md"
cp drivers/feemoo/VALIDATION.md "$destination/VALIDATION.md"
cp drivers/feemoo/session-helper.html "$destination/session-helper.html"
mkdir -p "$destination/images"
cp drivers/feemoo/images/openlist-copy-result.png "$destination/images/"
cp drivers/feemoo/session-helper.html "$destination/会话信息提取.html"
if [[ "$target_os" == windows ]]; then
  printf '@echo off\r\ncd /d "%%~dp0"\r\nopenlist.exe server\r\npause\r\n' > "$destination/启动OpenList.cmd"
fi
(cd "$destination" && sha256sum "$binary" > SHA256SUMS.txt)
echo "Build ready: $destination"
