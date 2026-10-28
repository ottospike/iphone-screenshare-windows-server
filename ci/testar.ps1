# Teste de fumaca no Windows do CI, fora do MSYS2 (PATH limpo, como no PC de quem usa):
# 1) o bundle acha todos os elementos do GStreamer que o UxPlay usa;
# 2) o uxplay.exe sobe e continua no ar;
# 3) o PhoneScreenshareServer.exe extrai os bundles sozinho e mantem um uxplay por slot (2 por padrao);
# 4) o scrcpy extraido roda e o servidor do adb sobe (sem Android no CI nao da para espelhar).
$ErrorActionPreference = 'Stop'
$b = Resolve-Path 'build/uxplay'
$env:PATH = "$b\bin;C:\Windows\System32;C:\Windows"
$env:GST_PLUGIN_SYSTEM_PATH_1_0 = "$b\lib\gstreamer-1.0"
$env:GST_PLUGIN_PATH_1_0 = "$b\lib\gstreamer-1.0"
$env:GST_PLUGIN_SCANNER_1_0 = "$b\libexec\gstreamer-1.0\gst-plugin-scanner.exe"
$env:GST_REGISTRY_1_0 = "$env:RUNNER_TEMP\registry.bin"
$env:FONTCONFIG_FILE = "$b\etc\fonts\fonts.conf"

$elementos = 'queue appsrc playbin decodebin h264parse h265parse avdec_h264 avdec_h265 avdec_aac avdec_alac ' +
  'videoconvert videoscale videoflip imagefreeze textoverlay jpegdec audioconvert audioresample volume level ' +
  'autovideosink autoaudiosink d3d11videosink'
$faltando = @()
foreach ($e in $elementos.Split(' ')) {
  & "$b\bin\gst-inspect-1.0.exe" --exists $e
  if ($LASTEXITCODE -ne 0) { $faltando += $e }
}
if ($faltando) { throw "Elementos do GStreamer faltando no bundle: $($faltando -join ', ')" }
Write-Host 'GStreamer: todos os elementos presentes'

function Espera-UxPlay([int]$segundos, [int]$quantos = 1) {
  Start-Sleep -Seconds $segundos
  $p = @(Get-Process uxplay -ErrorAction SilentlyContinue)
  if ($p.Count -lt $quantos) { throw "esperava $quantos uxplay rodando depois de $segundos s, achei $($p.Count)" }
  Write-Host "uxplay no ar (pid $($p.Id -join ','))"
}

$log = "$env:RUNNER_TEMP\uxplay.log"
$ux = Start-Process "$b\bin\uxplay.exe" -ArgumentList '-n','CI','-nh','-s','1920x1080','-fps','60' `
  -RedirectStandardOutput $log -RedirectStandardError "$log.err" -PassThru
try { Espera-UxPlay 15 } finally {
  Stop-Process -Id $ux.Id -Force -ErrorAction SilentlyContinue
  Get-Content $log, "$log.err" -ErrorAction SilentlyContinue | Select-Object -First 60
}

Remove-Item Env:GST_*, Env:FONTCONFIG_FILE
$env:PATH = 'C:\Windows\System32;C:\Windows'
$app = Start-Process 'dist/PhoneScreenshareServer.exe' -PassThru
try {
  Espera-UxPlay 40 2
  $scrcpy = Get-ChildItem "$env:LOCALAPPDATA\PhoneScreenshareServer\scrcpy-*\scrcpy.exe" | Select-Object -First 1
  if (-not $scrcpy) { throw 'scrcpy.exe nao foi extraido' }
  & $scrcpy.FullName --version
  if ($LASTEXITCODE -ne 0) { throw "scrcpy --version saiu com $LASTEXITCODE" }
  if (-not (Get-Process adb -ErrorAction SilentlyContinue)) { throw 'servidor do adb nao subiu' }
  Write-Host 'scrcpy e adb OK'
} finally {
  Get-Content "$env:LOCALAPPDATA\PhoneScreenshareServer\espelho.log" -ErrorAction SilentlyContinue | Select-Object -Last 40
  Stop-Process -Id $app.Id -Force -ErrorAction SilentlyContinue
  Get-Process uxplay, adb -ErrorAction SilentlyContinue | Stop-Process -Force
}
Write-Host 'OK'
