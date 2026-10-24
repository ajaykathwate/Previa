#!/bin/sh
# Official Tailwind CSS standalone CLI, pinned with GitHub release asset digests.
set -eu
version=4.1.18
destination=${1:?Usage: sh deploy/install-tailwind.sh destination}
case "$(uname -s):$(uname -m)" in
  Linux:x86_64)
    asset=tailwindcss-linux-x64
    digest=737becf8d4ad1115ea98df69fa94026d402ca8feb91306a035b5b004167da8dd ;;
  Linux:aarch64|Linux:arm64)
    asset=tailwindcss-linux-arm64
    digest=7a7702db6c93718a9b6655d455304edda18600f5a4f195242342ed3b5b70ebe8 ;;
  Darwin:x86_64)
    asset=tailwindcss-macos-x64
    digest=1e8a77fd796a3a4aa3d8727887de926ef9d38477aba113fd7c32c0d31a32a3ab ;;
  Darwin:arm64)
    asset=tailwindcss-macos-arm64
    digest=7f27711dceac1a580b6ad58ddac46e59202c85a6c16f2f08f6fdcdee261008e1 ;;
  *) echo 'Unsupported platform: use Linux/WSL2 or macOS on x64/arm64.' >&2; exit 1 ;;
esac
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sum=$(sha256sum "$1")
  else
    sum=$(shasum -a 256 "$1")
  fi
  echo "${sum%% *}"
}
# A verified copy (for example in a build cache) needs no download.
if [ -f "$destination" ] && [ "$(sha256 "$destination")" = "$digest" ]; then
  chmod 755 "$destination"
  echo "Official Tailwind CSS v${version} is already installed."
  exit 0
fi
mkdir -p "$(dirname "$destination")"
temporary=$(mktemp "${destination}.download.XXXXXX")
trap 'rm -f "$temporary"' EXIT HUP INT TERM
# GitHub's release downloads fail now and then (a 502, a dropped stream), so
# retry a few times before failing a build.
curl --fail --location --silent --show-error \
  --retry 5 --retry-delay 3 --retry-all-errors --connect-timeout 20 \
  "https://github.com/tailwindlabs/tailwindcss/releases/download/v${version}/${asset}" \
  --output "$temporary"
actual=$(sha256 "$temporary")
if [ "$actual" != "$digest" ]; then
  echo 'Tailwind download checksum mismatch; refusing to install.' >&2
  exit 1
fi
chmod 755 "$temporary"
mv "$temporary" "$destination"
echo "Installed official Tailwind CSS v${version}."
