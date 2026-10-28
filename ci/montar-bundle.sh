#!/usr/bin/env bash
# Roda no MSYS2 (UCRT64) do CI. Compila o UxPlay com o mDNS embutido (sem Bonjour) e monta
# bundle/uxplay.zip: bin/ (uxplay.exe + DLLs), lib/gstreamer-1.0/ (so os plugins que o UxPlay usa),
# libexec/ (gst-plugin-scanner) e etc/fonts/ (textoverlay).
set -euo pipefail

UXPLAY_REF="${UXPLAY_REF:?defina UXPLAY_REF}"
RAIZ="$(pwd)"
OUT="$RAIZ/build/uxplay"
PREFIX=/ucrt64

# Plugins do GStreamer usados pelos pipelines do UxPlay (renderers/*.c) no Windows.
PLUGINS=(
  coreelements app typefindfunctions playback          # queue, appsrc, playbin/decodebin
  videoconvertscale videofilter imagefreeze pango      # videoconvert/videoscale, videoflip, textoverlay
  jpeg videoparsersbad libav                           # jpegdec (capa), h264parse/h265parse, avdec_*
  audioconvert audioresample volume level              # cadeia de audio
  autodetect d3d11 d3d12 wasapi wasapi2 directsound    # sinks de video e audio do Windows
)

rm -rf build && mkdir -p build && cd build
git clone --quiet https://github.com/FDH2/UxPlay src
git -C src checkout --quiet "$UXPLAY_REF"
cmake -S src -B obj -G Ninja -DCMAKE_BUILD_TYPE=Release   # sem -DUSE_DNS_SD: usa lib/mdnsd
ninja -C obj

mkdir -p "$OUT/bin" "$OUT/lib/gstreamer-1.0" "$OUT/libexec/gstreamer-1.0" "$OUT/etc"
cp obj/uxplay.exe "$OUT/bin/"
cp "$PREFIX/bin/gst-inspect-1.0.exe" "$OUT/bin/"
cp "$PREFIX/libexec/gstreamer-1.0/gst-plugin-scanner.exe" "$OUT/libexec/gstreamer-1.0/"
cp -r "$PREFIX/etc/fonts" "$OUT/etc/"
for p in "${PLUGINS[@]}"; do
  f="$PREFIX/lib/gstreamer-1.0/libgst$p.dll"
  if [ -f "$f" ]; then cp "$f" "$OUT/lib/gstreamer-1.0/"; else echo "aviso: plugin $p nao existe no MSYS2"; fi
done

# Todas as DLLs do MSYS2 de que os .exe e plugins dependem (o ldd ja e transitivo).
find "$OUT" -name '*.exe' -o -name '*.dll' | while read -r bin; do
  ldd "$bin" | awk '{print $3}' | grep -i "^$PREFIX/bin/" || true
done | sort -u | while read -r dll; do cp -n "$dll" "$OUT/bin/"; done

cd "$OUT" && rm -f "$RAIZ/bundle/uxplay.zip" && mkdir -p "$RAIZ/bundle"
zip -qr9 "$RAIZ/bundle/uxplay.zip" .
du -sh "$OUT" "$RAIZ/bundle/uxplay.zip"
