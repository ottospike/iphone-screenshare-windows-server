# Smoke test on the CI Windows runner, outside MSYS2 (clean PATH, like a user's PC):
# 1) the bundle has every GStreamer element UxPlay uses;
# 2) uxplay.exe starts and stays up with the sink, MAC and resolution flags the app passes;
# 3) iPhoneScreenshareServer.exe extracts the bundle by itself and keeps 1 free slot (LIVE 1) up,
#    without crashes, when there is no binding;
# 4) with a fake uxplay (ci/fake-uxplay, built to $env:RUNNER_TEMP\fake) that simulates a device:
#    visible video window, slot not restarted right after the connection, binding saved, new free
#    slot with -block for the bound device, the source geometry/rotation read from the log, and the
#    video window fitted to the video (client area with the portrait, then the landscape, ratio).
$ErrorActionPreference = 'Stop'
$b = Resolve-Path 'build/uxplay'
$env:PATH = "$b\bin;C:\Windows\System32;C:\Windows"
$env:GST_PLUGIN_SYSTEM_PATH_1_0 = "$b\lib\gstreamer-1.0"
$env:GST_PLUGIN_PATH_1_0 = "$b\lib\gstreamer-1.0"
$env:GST_PLUGIN_SCANNER_1_0 = "$b\libexec\gstreamer-1.0\gst-plugin-scanner.exe"
$env:GST_REGISTRY_1_0 = "$env:RUNNER_TEMP\registry.bin"
$env:FONTCONFIG_FILE = "$b\etc\fonts\fonts.conf"

$elements = 'queue appsrc playbin decodebin h264parse h265parse avdec_h264 avdec_h265 avdec_aac avdec_alac ' +
  'videoconvert videoscale videoflip imagefreeze textoverlay jpegdec audioconvert audioresample volume level ' +
  'autovideosink autoaudiosink d3d11videosink'
$missing = @()
foreach ($e in $elements.Split(' ')) {
  & "$b\bin\gst-inspect-1.0.exe" --exists $e
  if ($LASTEXITCODE -ne 0) { $missing += $e }
}
if ($missing) { throw "GStreamer elements missing from the bundle: $($missing -join ', ')" }
Write-Host 'GStreamer: all elements present'

function Wait-UxPlay([int]$seconds, [int]$count = 1) {
  Start-Sleep -Seconds $seconds
  $p = @(Get-Process uxplay -ErrorAction SilentlyContinue)
  if ($p.Count -ne $count) { throw "expected $count uxplay running after $seconds s, found $($p.Count)" }
  Write-Host "uxplay up (pid $($p.Id -join ','))"
  return ($p.Id | Sort-Object) -join ','
}

# Each resolution preset the tray offers (Native, FHD, HD, SD) must be accepted at startup.
$presets = @(
  @('-h265', '-s', '3840x3840', '-fps', '60'),
  @('-s', '1920x1920', '-fps', '60'),
  @('-s', '1280x1280', '-fps', '30'),
  @('-s', '854x854', '-fps', '30')
)
foreach ($preset in $presets) {
  $log = "$env:RUNNER_TEMP\uxplay.log"
  $ux = Start-Process "$b\bin\uxplay.exe" -ArgumentList (@('-n', 'CI', '-nh', '-m', '06:00:00:00:00:01', '-vs', 'd3d11videosink') + $preset) `
    -RedirectStandardOutput $log -RedirectStandardError "$log.err" -PassThru
  try { Wait-UxPlay 10 | Out-Null } finally {
    Stop-Process -Id $ux.Id -Force -ErrorAction SilentlyContinue
    Get-Content $log, "$log.err" -ErrorAction SilentlyContinue | Select-Object -First 30
  }
  if (-not (Select-String -Path $log -Pattern 'user-set MAC address 06:00:00:00:00:01' -Quiet)) { throw "uxplay did not accept -m with $preset" }
  Write-Host "preset OK: $preset"
  Start-Sleep -Seconds 1
}

Remove-Item Env:GST_*, Env:FONTCONFIG_FILE
$env:PATH = 'C:\Windows\System32;C:\Windows'
$appLog = "$env:LOCALAPPDATA\iPhoneScreenshareServer\app.log"
$app = Start-Process 'dist/iPhoneScreenshareServer.exe' -PassThru
try {
  $pid20 = Wait-UxPlay 20 1
  $pid40 = Wait-UxPlay 20 1
  if ($pid20 -ne $pid40) { throw "the free slot's uxplay was restarted ($pid20 -> $pid40)" }
  $l = Get-Content $appLog -Encoding UTF8
  if ($l -match 'UxPlay crashed|ERROR starting') { throw 'uxplay crashed during the test' }
  if (-not ($l -match 'slot 1: starting .* -m .* -h265 -s 3840x3840 -fps 60')) { throw 'slot 1 started without -m or the default resolution' }
} finally {
  Get-Content $appLog -Encoding UTF8 -ErrorAction SilentlyContinue | Select-Object -Last 40
  Stop-Process -Id $app.Id -Force -ErrorAction SilentlyContinue
  Get-Process uxplay -ErrorAction SilentlyContinue | Stop-Process -Force
}
Start-Sleep -Seconds 2

# 4) fake uxplay, with the app isolated in its own folder
$fake = "$env:RUNNER_TEMP\fake\uxplay.exe"
$env:LOCALAPPDATA = "$env:RUNNER_TEMP\appfake"
$dir = "$env:LOCALAPPDATA\iPhoneScreenshareServer"
New-Item -ItemType Directory -Force $dir | Out-Null
[IO.File]::WriteAllText("$dir\config.json", (@{ maxSlots = 8; uxplay = $fake } | ConvertTo-Json))
$app = Start-Process 'dist/iPhoneScreenshareServer.exe' -PassThru
try {
  Start-Sleep -Seconds 25
  $windows = @(Get-Process uxplay -ErrorAction SilentlyContinue | ForEach-Object MainWindowTitle)
  $l = Get-Content "$dir\app.log" -Encoding UTF8
  $c = Get-Content "$dir\config.json" -Raw -Encoding UTF8 | ConvertFrom-Json
  $errors = @()
  if ($l -match 'FAKE: inherited SW_HIDE') { $errors += 'uxplay started with SW_HIDE' }
  if (-not ($l -match 'FAKE: visible=1')) { $errors += 'video window not visible' }
  if ($c.bindings.'1'.deviceID -ne 'AA:BB:CC:DD:EE:01') { $errors += 'slot 1 binding not saved' }
  $s1 = @($l -match 'slot 1: starting')
  $s2 = @($l -match 'slot 2: starting')
  if ($s1.Count -ne 1 -or ($l -match 'slot 1: applying')) { $errors += 'slot 1 restarted with the device connected' }
  if ($s2.Count -lt 1) { $errors += 'new free slot (slot 2) did not start' }
  elseif (-not ($s2[0] -match '-block AA:BB:CC:DD:EE:01')) { $errors += 'slot 2 without -block for the bound device' }
  if ($s1.Count -ge 1 -and $s2.Count -ge 1) {
    $m1 = [regex]::Match($s1[0], ' -m (\S+)').Groups[1].Value
    $m2 = [regex]::Match($s2[0], ' -m (\S+)').Groups[1].Value
    if (-not $m1 -or $m1 -eq $m2) { $errors += "slots with the same MAC ($m1 / $m2)" }
  }
  if (-not ($l -match 'slot 1: video portrait 1320.2868')) { $errors += 'portrait geometry not read' }
  if (-not ($l -match 'slot 1: video landscape 2868.1320')) { $errors += 'rotation not read' }
  if ($windows -notcontains 'Slot 1') { $errors += "window not renamed to 'Slot 1' (found: $($windows -join ', '))" }
  # The fake prints its client size on every resize: after the portrait geometry the app fits the
  # window to 1320x2868's ratio, after the rotation to 2868x1320's, never larger than the video.
  $sizes = @($l | ForEach-Object { if ($_ -match 'FAKE: client (\d+)x(\d+)') { ,@([int]$Matches[1], [int]$Matches[2]) } })
  $fit = { param($w, $h, $vw, $vh) $h -gt 0 -and [math]::Abs($w / $h - $vw / $vh) -lt 0.02 -and $w -le $vw -and $h -le $vh }
  $p = -1
  for ($i = 0; $i -lt $sizes.Count; $i++) { if (& $fit $sizes[$i][0] $sizes[$i][1] 1320 2868) { $p = $i; break } }
  $q = -1
  for ($i = [math]::Max($p, 0); $i -lt $sizes.Count; $i++) { if (& $fit $sizes[$i][0] $sizes[$i][1] 2868 1320) { $q = $i } }
  if ($p -lt 0) { $errors += "window not fitted to the portrait video (client sizes: $(($sizes | ForEach-Object { $_ -join 'x' }) -join ', '))" }
  elseif ($q -le $p) { $errors += "window not fitted to the landscape video after the rotation (client sizes: $(($sizes | ForEach-Object { $_ -join 'x' }) -join ', '))" }
  if (-not ($l -match 'slot 1: window fitted: client')) { $errors += 'window fit not logged' }
  if ($errors) { throw ($errors -join '; ') }
  Write-Host 'fake uxplay: binding, new slot, MACs, geometry, window Slot 1 fitted to the video OK'
} finally {
  Get-Content "$dir\app.log" -Encoding UTF8 -ErrorAction SilentlyContinue | Select-Object -Last 40
  Stop-Process -Id $app.Id -Force -ErrorAction SilentlyContinue
  Get-Process uxplay -ErrorAction SilentlyContinue | Stop-Process -Force
}
Write-Host 'OK'
