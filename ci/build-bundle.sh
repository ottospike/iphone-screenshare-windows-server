#!/usr/bin/env bash
# Runs in CI's MSYS2 (UCRT64). Builds UxPlay with its embedded mDNS (no Bonjour) and assembles
# bundle/uxplay.zip: bin/ (uxplay.exe + DLLs), lib/gstreamer-1.0/ (only the plugins UxPlay uses),
# libexec/ (gst-plugin-scanner) and etc/fonts/ (textoverlay).
set -euo pipefail

UXPLAY_REF="${UXPLAY_REF:?set UXPLAY_REF}"
ROOT="$(pwd)"
OUT="$ROOT/build/uxplay"
PREFIX=/ucrt64

# GStreamer plugins used by UxPlay's pipelines (renderers/*.c) on Windows.
PLUGINS=(
  coreelements app typefindfunctions playback          # queue, appsrc, playbin/decodebin
  videoconvertscale videofilter imagefreeze pango      # videoconvert/videoscale, videoflip, textoverlay
  jpeg videoparsersbad libav                           # jpegdec (cover art), h264parse/h265parse, avdec_*
  audioconvert audioresample volume level              # audio chain
  autodetect d3d11 d3d12 wasapi wasapi2 directsound    # Windows video and audio sinks
)

rm -rf build && mkdir -p build && cd build
git clone --quiet https://github.com/FDH2/UxPlay src
git -C src checkout --quiet "$UXPLAY_REF"
# Unbuffered log: the app reads "connection request ... deviceID" right away to bind the device to its slot.
sed -i 's/^int main (int argc, char \*argv\[\]) {$/&\n    setvbuf(stdout, NULL, _IONBF, 0);/' src/uxplay.cpp
grep -c '_IONBF' src/uxplay.cpp
cmake -S src -B obj -G Ninja -DCMAKE_BUILD_TYPE=Release   # no -DUSE_DNS_SD: uses lib/mdnsd
ninja -C obj

mkdir -p "$OUT/bin" "$OUT/lib/gstreamer-1.0" "$OUT/libexec/gstreamer-1.0" "$OUT/etc"
cp obj/uxplay.exe "$OUT/bin/"
cp "$PREFIX/bin/gst-inspect-1.0.exe" "$OUT/bin/"
cp "$PREFIX/libexec/gstreamer-1.0/gst-plugin-scanner.exe" "$OUT/libexec/gstreamer-1.0/"
cp -r "$PREFIX/etc/fonts" "$OUT/etc/"
for p in "${PLUGINS[@]}"; do
  f="$PREFIX/lib/gstreamer-1.0/libgst$p.dll"
  if [ -f "$f" ]; then cp "$f" "$OUT/lib/gstreamer-1.0/"; else echo "warning: plugin $p not found in MSYS2"; fi
done

# Every MSYS2 DLL the .exe files and plugins depend on (ldd is already transitive).
find "$OUT" -name '*.exe' -o -name '*.dll' | while read -r bin; do
  ldd "$bin" | awk '{print $3}' | grep -i "^$PREFIX/bin/" || true
done | sort -u | while read -r dll; do cp -n "$dll" "$OUT/bin/"; done

cd "$OUT" && rm -f "$ROOT/bundle/uxplay.zip" && mkdir -p "$ROOT/bundle"
zip -qr9 "$ROOT/bundle/uxplay.zip" .
du -sh "$OUT" "$ROOT/bundle/uxplay.zip"
