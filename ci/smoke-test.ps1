# Smoke test on the CI Windows runner, outside MSYS2 (clean PATH, like a user's PC):
# 1) the bundle has every GStreamer element UxPlay uses, and uxplay.exe has the pool patch;
# 2) uxplay.exe starts and stays up with the sink, MAC, resolution and -FPSdata flags the app passes;
# 3) iPhoneScreenshareServer.exe extracts the bundle by itself and keeps exactly 1 free instance
#    ("LIVE") up, without crashes;
# 4) with a fake uxplay (ci/fake-uxplay, built to $env:RUNNER_TEMP\fake) that simulates a device:
#    visible video window named after the device, instance not restarted right after the
#    connection, device remembered in config.json, exactly one new free instance (same name, its own
#    MAC, no -restrict/-block/-nohold), the patch's mDNS line read, the client's fps read and only
#    the first FPS report in the log, the codec and source geometry/rotation read from the log, the
#    video window already in the video's shape the first time it becomes visible and fitted again on
#    rotation, and the close button's tooltip window left alone (not renamed, not resized), the
#    window hidden while the device's screen is off and shown again after waking; then the
#    device stops mirroring (only the patch's "mirroring ended, advertised again" line says so):
#    the instance is back in Screen Mirroring and becomes the one free instance without a restart,
#    and slot 2 goes down.
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

# The pool patch (ci/uxplay-pool.patch) is compiled in: its log lines are in the exe.
$exeText = [Text.Encoding]::ASCII.GetString([IO.File]::ReadAllBytes("$b\bin\uxplay.exe"))
foreach ($s in 'mDNS: advertised at startup', 'mDNS: hidden while mirroring', 'mDNS: mirroring ended, advertised again') {
  if (-not $exeText.Contains($s)) { throw "uxplay.exe without the pool patch (no '$s')" }
}
foreach ($s in 'privacy: video paused', 'privacy: video resumed') {
  if (-not $exeText.Contains($s)) { throw "uxplay.exe without the privacy lines (no '$s')" }
}
Write-Host 'uxplay.exe: pool patch and privacy lines present'

function Wait-UxPlay([int]$seconds, [int]$count = 1) {
  Start-Sleep -Seconds $seconds
  $p = @(Get-Process uxplay -ErrorAction SilentlyContinue)
  if ($p.Count -ne $count) { throw "expected $count uxplay running after $seconds s, found $($p.Count)" }
  Write-Host "uxplay up (pid $($p.Id -join ','))"
  return ($p.Id | Sort-Object) -join ','
}

# Each resolution preset the tray offers (Native, FHD, HD, SD) must be accepted at startup, with
# the -FPSdata the app always passes.
$presets = @(
  @('-h265', '-s', '2880x2880', '-fps', '60', '-FPSdata'),
  @('-s', '1920x1920', '-fps', '60', '-FPSdata'),
  @('-s', '1280x1280', '-fps', '30', '-FPSdata'),
  @('-s', '854x854', '-fps', '30', '-FPSdata')
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
  if ($pid20 -ne $pid40) { throw "the free instance's uxplay was restarted ($pid20 -> $pid40)" }
  $l = Get-Content $appLog -Encoding UTF8
  if ($l -match 'UxPlay crashed|ERROR starting') { throw 'uxplay crashed during the test' }
  if (-not ($l -match 'slot 1: starting .* -n LIVE .* -m .* -h265 -s 2880x2880 -fps 60 -FPSdata')) {
    throw 'slot 1 started without the name, -m, the default resolution or -FPSdata'
  }
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
Remove-Item "$fake.session" -ErrorAction SilentlyContinue  # the fake simulates one session per run
# wakeBlackSeconds 1: the fake checks its window 3 s after waking, when it must be shown again.
[IO.File]::WriteAllText("$dir\config.json", (@{ maxSlots = 8; uxplay = $fake; wakeBlackSeconds = 1 } | ConvertTo-Json))
$app = Start-Process 'dist/iPhoneScreenshareServer.exe' -PassThru
function Wait-AppLog([string]$pattern, [int]$seconds) {
  $deadline = (Get-Date).AddSeconds($seconds)
  while ((Get-Date) -lt $deadline) {
    if (Select-String -Path "$dir\app.log" -Pattern $pattern -Quiet -ErrorAction SilentlyContinue) { return }
    Start-Sleep -Milliseconds 500
  }
}
try {
  # The fake reports its tooltip about 22 s after it starts, its screen goes off and back on in the
  # next 5 s, and its session ends at 32 s.
  Wait-AppLog 'FAKE: titles' 45
  $procs = @(Get-Process uxplay -ErrorAction SilentlyContinue)
  $windows = @($procs | ForEach-Object MainWindowTitle)
  $l = Get-Content "$dir\app.log" -Encoding UTF8
  $c = Get-Content "$dir\config.json" -Raw -Encoding UTF8 | ConvertFrom-Json
  $errors = @()
  if ($l -match 'FAKE: inherited SW_HIDE') { $errors += 'uxplay started with SW_HIDE' }
  if (-not ($l -match 'FAKE: visible=1')) { $errors += 'video window not visible' }
  $dev = $c.devices.'AA:BB:CC:DD:EE:01'
  if ($dev.name -ne 'CI iPhone' -or $dev.window -ne 'CI iPhone') {
    $errors += "device not remembered in config.json: $($c.devices | ConvertTo-Json -Compress)"
  }
  $s1 = @($l -match 'slot 1: starting')
  $s2 = @($l -match 'slot 2: starting')
  if ($s1.Count -ne 1 -or ($l -match 'slot 1: applying')) { $errors += 'slot 1 restarted with the device connected' }
  if ($s2.Count -lt 1) { $errors += 'no free instance (slot 2) for the next device' }
  elseif ($s2[0] -match ' -restrict| -allow| -block| -nohold' -or -not ($s2[0] -match ' -n LIVE ')) { $errors += "slot 2 must be a plain LIVE: $($s2[0])" }
  if ($l -match 'slot 3: starting') { $errors += 'more than one free instance' }
  if ($procs.Count -ne 2) { $errors += "expected 2 uxplay (the device's and one free), found $($procs.Count)" }
  if ($s1.Count -ge 1 -and $s2.Count -ge 1) {
    $m1 = [regex]::Match($s1[0], ' -m (\S+)').Groups[1].Value
    $m2 = [regex]::Match($s2[0], ' -m (\S+)').Groups[1].Value
    if (-not $m1 -or $m1 -eq $m2) { $errors += "instances with the same MAC ($m1 / $m2)" }
  }
  if (-not ($l -match 'slot 1: CI iPhone connected, deviceID AA:BB:CC:DD:EE:01, window "CI iPhone"')) { $errors += 'connection not logged with its window title' }
  if (-not ($l -match 'slot 1: out of Screen Mirroring while CI iPhone mirrors')) { $errors += "the patch's mDNS line was not read" }
  if (-not ($l -match 'slot 1: client reports 60 fps')) { $errors += 'client fps not read' }
  $reports = @($l -match '^\[slot 1\] <plist')
  if ($reports.Count -ne 1) { $errors += "expected only the first FPS report in the log, found $($reports.Count)" }
  if (-not ($l -match 'slot 1: video H\.264 portrait 1320.2868')) { $errors += 'codec and portrait geometry not read' }
  if (-not ($l -match 'slot 1: video H\.264 landscape 2868.1320')) { $errors += 'rotation not read' }
  if ($windows -notcontains 'CI iPhone') { $errors += "window not named after the device (found: $($windows -join ', '))" }
  # The close button's tooltip: same process, not the video window. It keeps its title and size.
  $tip = @($l -match 'FAKE: titles video=')
  if ($tip.Count -lt 1) { $errors += 'the fake did not report its tooltip' }
  elseif (-not ($tip[0] -match 'video="CI iPhone" tooltip="Close" tooltip size 120x24')) { $errors += "tooltip touched, or video not renamed: $($tip[0])" }
  $fits = @($l -match 'slot 1: window fitted')
  if ($fits.Count -gt 2) { $errors += "more fits than the video's two geometries: $($fits -join ' | ')" }
  # The fake prints its client size on every resize: after the portrait geometry the app fits the
  # window to 1320x2868's ratio, after the rotation to 2868x1320's, never larger than the video.
  $sizes = @($l | ForEach-Object { if ($_ -match 'FAKE: client (\d+)x(\d+)') { ,@([int]$Matches[1], [int]$Matches[2]) } })
  $fit = { param($w, $h, $vw, $vh) $h -gt 0 -and [math]::Abs($w / $h - $vw / $vh) -lt 0.02 -and $w -le $vw -and $h -le $vh }
  $p = -1
  for ($i = 0; $i -lt $sizes.Count; $i++) { if (& $fit $sizes[$i][0] $sizes[$i][1] 1320 2868) { $p = $i; break } }
  $q = -1
  for ($i = [math]::Max($p, 0); $i -lt $sizes.Count; $i++) { if (& $fit $sizes[$i][0] $sizes[$i][1] 2868 1320) { $q = $i } }
  # Like d3d11videosink, the fake sizes its hidden window to the full video and then shows it: the
  # window must already have the video's shape the first time it is seen (visible and not
  # transparent), never be seen at the sink's size and resized afterwards.
  $seen = @($l | ForEach-Object { if ($_ -match 'FAKE: seen client (\d+)x(\d+)') { ,@([int]$Matches[1], [int]$Matches[2]) } })
  if ($seen.Count -lt 1) { $errors += 'the fake window was never seen' }
  elseif (-not (& $fit $seen[0][0] $seen[0][1] 1320 2868)) { $errors += "window first seen at $($seen[0] -join 'x'), not in the portrait video's shape" }
  if ($p -lt 0) { $errors += "window not fitted to the portrait video (client sizes: $(($sizes | ForEach-Object { $_ -join 'x' }) -join ', '))" }
  elseif ($q -le $p) { $errors += "window not fitted to the landscape video after the rotation (client sizes: $(($sizes | ForEach-Object { $_ -join 'x' }) -join ', '))" }
  if (-not ($l -match 'slot 1: window fitted: client')) { $errors += 'window fit not logged' }
  if ($errors) { throw ($errors -join '; ') }
  Write-Host 'fake uxplay: one free LIVE, device-named window, tooltip left alone, fps, mDNS line, MACs, codec, geometry, fit on rotation OK'

  # Privacy: while the device's screen is off the window is transparent (OBS gets nothing, not the
  # frozen lock screen), and it shows again once wakeBlackSeconds after waking is over.
  Wait-AppLog 'FAKE: awake transparent' 15
  $l = Get-Content "$dir\app.log" -Encoding UTF8
  if (-not ($l -match 'slot 1: CI iPhone screen off: window black')) { $errors += 'screen off not read' }
  if (-not ($l -match 'FAKE: asleep transparent=1')) { $errors += "window not hidden while the device's screen is off" }
  if (-not ($l -match 'FAKE: awake transparent=0')) { $errors += 'window not shown again after waking' }
  if ($errors) { throw ($errors -join '; ') }
  Write-Host 'privacy: window hidden while the screen is off, shown again after waking OK'

  # The fake's device stops mirroring 32 s after its start and slot 1 announces itself again: the
  # devices now resolve LIVE to it, so 5 s later it is the free LIVE, without a restart, and slot 2
  # (the other idle one) goes down.
  Wait-AppLog 'slot 2: down, not needed' 30
  Start-Sleep -Seconds 2
  $l = Get-Content "$dir\app.log" -Encoding UTF8
  $procs = @(Get-Process uxplay -ErrorAction SilentlyContinue)
  if (-not ($l -match 'slot 1: CI iPhone stopped mirroring; back in Screen Mirroring')) { $errors += 'end of mirroring or the new announcement not read' }
  if (-not ($l -match 'slot 2: down, not needed')) { $errors += 'slot 2 still up after the session: more than one idle instance' }
  if ($l -match 'slot 1: session over; restarting|slot 1: down') { $errors += 'slot 1 restarted or went down although it is advertised again' }
  if (@($l -match 'slot 1: starting').Count -ne 1) { $errors += 'slot 1 must not restart' }
  if ($l -match 'slot 3: starting') { $errors += 'an extra instance started after the session' }
  if ($procs.Count -ne 1) { $errors += "expected 1 uxplay (the free one) after the session, found $($procs.Count)" }
  if ($errors) { throw ($errors -join '; ') }
  Write-Host 'fake uxplay: after the session, slot 1 is the one free LIVE again and slot 2 is down OK'

} finally {
  Get-Content "$dir\app.log" -Encoding UTF8 -ErrorAction SilentlyContinue | Select-Object -Last 60
  Stop-Process -Id $app.Id -Force -ErrorAction SilentlyContinue
  Get-Process uxplay -ErrorAction SilentlyContinue | Stop-Process -Force
}
Write-Host 'OK'
