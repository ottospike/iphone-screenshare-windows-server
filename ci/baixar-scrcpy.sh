#!/usr/bin/env bash
# Roda no MSYS2 do CI. Baixa o scrcpy oficial para Windows (ja traz o adb), confere o SHA-256 e
# monta bundle/scrcpy.zip com os arquivos na raiz (scrcpy.exe, adb.exe, DLLs, scrcpy-server).
set -euo pipefail

SCRCPY_VERSION="${SCRCPY_VERSION:?defina SCRCPY_VERSION}"
SCRCPY_SHA256="${SCRCPY_SHA256:?defina SCRCPY_SHA256}"
RAIZ="$(pwd)"
NOME="scrcpy-win64-v$SCRCPY_VERSION"

rm -rf build/scrcpy && mkdir -p build/scrcpy && cd build/scrcpy
curl -fsSL -o "$NOME.zip" "https://github.com/Genymobile/scrcpy/releases/download/v$SCRCPY_VERSION/$NOME.zip"
echo "$SCRCPY_SHA256  $NOME.zip" | sha256sum -c -
unzip -q "$NOME.zip"
cd "$NOME" && rm -f "$RAIZ/bundle/scrcpy.zip" && mkdir -p "$RAIZ/bundle"
zip -qr9 "$RAIZ/bundle/scrcpy.zip" .
du -sh "$RAIZ/bundle/scrcpy.zip"
