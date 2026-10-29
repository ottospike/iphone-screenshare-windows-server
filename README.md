# iPhone Screenshare Windows Server

Receives **AirPlay screen mirroring** from iPhone, iPad or Mac on Windows over Wi-Fi, in a window
that **OBS** captures. One `.exe`: no installer, no Bonjour, no account, no trial.

Under the hood it runs [UxPlay](https://github.com/FDH2/UxPlay), an open source AirPlay receiver,
with its own embedded mDNS, so devices find the PC without Apple's Bonjour. The app lives in the
Windows tray, keeps UxPlay running and restarts it if it crashes.

Several devices at once, all through **one entry**: everyone taps **LIVE** in Screen Mirroring.
Each device gets its own video window, **named after the device**, so each OBS source follows its
person, across reconnects and app restarts.

## Download

Get `iPhoneScreenshareServer.exe` from the [latest release](../../releases/latest).
Requirements: Windows 10 or 11, 64-bit; device and PC **on the same network**.

If Windows warns about the file the first time:

- Chrome/Edge blocked the download: **⋯ → Keep**.
- "Windows protected your PC" (SmartScreen): **More info → Run anyway**.
- To stop the warning: right-click the `.exe` → **Properties** → tick **Unblock** → OK.

## Usage

1. Run `iPhoneScreenshareServer.exe`. The first time:
   - it extracts UxPlay to `%LOCALAPPDATA%\iPhoneScreenshareServer\` (a few seconds);
   - Windows asks for administrator rights to open the firewall: once per version, on the first
     run after an update.
2. On each device: **Control Center → Screen Mirroring → LIVE**. Every device taps the same LIVE.
3. In OBS, one source per device, by the device's name (see [OBS setup](#obs-setup)).

## One LIVE for everyone

The app runs a pool of UxPlay instances, up to `maxSlots` (default 8), all with the same name.
Exactly one of them is free and announced as **LIVE**:

- When a device connects to LIVE, that instance is the device's for the session, and the app
  brings up the next free one right away for the next device.
- While a device mirrors, its instance takes itself out of Screen Mirroring (UxPlay is patched
  for this, see [How it works](#how-it-works)), so other devices only see the free LIVE and can't
  take over someone's screen. When it stops mirroring, the instance puts itself back in Screen
  Mirroring as LIVE. All instances share the name, so devices now reach LIVE on this instance:
  it becomes the free one, and the previous free one goes down about 5 s later. It announces
  itself a few more times over the next 8 s, so a device that missed one announcement still finds
  it. If Resolution or FPS changed while it was live, it restarts right then with the new settings,
  before the next device takes it. If UxPlay can't announce itself again, the instance stays out of
  Screen Mirroring and restarts when it is needed.
- Each instance has its own ports and MAC address; the window, the tray line and the log say
  which device is on it.
- When `maxSlots` devices are live there is no free LIVE; the tray says so. Devices may still list
  LIVE for a while then, but tapping it fails until a session ends.
- Only Screen Mirroring is supported: AirPlay video (e.g. from YouTube) or audio sent to LIVE does
  not take the instance out of Screen Mirroring.
- A device that reconnects (Wi-Fi blip) while its old session still counts as live is taken by the
  free LIVE, and its old instance restarts at once, so there is never a frozen copy of its window.

## OBS setup

One **Window Capture** source per device: **Sources → + → Window Capture**.

- **Window**: `[uxplay.exe]: <device name>`, e.g. `[uxplay.exe]: Otavio Augusto’s iPhone`: the
  label you gave the device in the config if any, otherwise the name the device sends. The window
  only exists while the device mirrors: connect it once so it shows in the list.
- **Capture Method**: **Windows 10 (1903 and up)**, with **Client area** checked: only the video
  is captured, without the title bar, buttons or borders.
- **Window Match Priority**: **Window title must match**. The title is the same every time the
  device connects, whatever instance it lands on, so the source always finds its person.
- Right-click the source → **Transform → Edit Transform** → **Bounding Box Type**:
  **Scale to inner bounds**, and set the bounding box size to the area you want. The source then
  fits portrait and landscape automatically when the device rotates.

Two devices with the same name (two `iPhone`s) get `iPhone` and `iPhone 2`, and each keeps its
title: the app remembers it per device. Give them a `label` in the config to tell them apart.

**Coming from v0.2.0** (sources on `Slot 1`, `Slot 2`…): connect each device once, then open each
source and pick the device's window instead of `Slot N`. Only once: from then on the title stays.

The video window:

- **Don't minimize it**: a minimized window sends OBS nothing. It can sit behind other windows or
  on another monitor.
- **Closing it with X ends that device's session**: the device stops mirroring and mirrors again
  from Screen Mirroring.
- The app fits the window to the video, so there are no black bars: its client area takes the
  video's aspect ratio, as large as fits in 90% of its monitor's work area (the screen minus the
  taskbar) but never larger than the video. The window is first seen already in that shape: UxPlay
  sizes its new window to the full video right before showing it, so the app keeps the window
  transparent from the moment it is created until it has been fitted, a few milliseconds after it
  is shown. You can resize it by hand; the app fits it again on every rotation and when a device
  reconnects.
- Only the video window itself (GStreamer's `GSTD3D11` window, from the d3d11videosink the app runs UxPlay with) is fitted and renamed:
  the close button's tooltip and other small windows of UxPlay are left alone.
- Quality comes from what the device sends (**Native**: its own resolution). The window only
  needs to show the whole video, and it may be smaller than the video: OBS captures the whole
  picture, at the window's size.

### Audio

Audio is always on. Each device's sound plays on the PC's default output and is also a separate
audio app that OBS can capture:

- Add **Sources → + → Application Audio Capture**, pick `[uxplay.exe]: <device name>` and set
  **Window Match Priority** to **Window title must match**, the same as for Window Capture.
- Mute or unmute each device in the OBS **Audio Mixer**.
- Since the sound also plays on the PC's default output, mute **Desktop Audio** in OBS, or set
  it to another device, to avoid capturing every device twice.
- AirPlay has 100–200 ms of latency; adjust **Sync Offset** in Advanced Audio Properties if needed.

## Tray menu

| Item | What it does |
|---|---|
| First line | How many devices are live and the limit, or that it is full |
| `Otavio Augusto’s iPhone · live · H.265 · portrait 1325×2880 · 60 fps` | One per device: its name (the window title), state, and while live the codec, the orientation and the real source resolution, updated on every rotation, and the frame rate the device reports it sends. After a Resolution/FPS change, e.g. `· SD on reconnect` until its instance restarts. Submenu: **Apply now**, shown while a change waits for this device, ends its session and restarts its instance with the new settings, then tap LIVE again on the device; **Forget device** removes it from the remembered devices |
| `LIVE · waiting` | The free instance, for the next device |
| **Resolution** | Native · FHD · HD · SD, for all instances. Saved; each instance restarts with it once nobody is mirroring on it, so live devices are not dropped |
| **FPS** | 60 · 30, for all instances. Saved; applied the same way |
| **Black screen** | Hides every video window from the screen and OBS at once, until you turn it off. See [Privacy](#privacy-lock-screen-and-frozen-frames) |
| **Restart** | Rereads the config and restarts every instance. If the config has an error or can't be read, nothing restarts and live devices stay connected |
| **Open log** | App and UxPlay log in Notepad |
| **Open config** | `config.json` in Notepad |
| **Quit** | Closes the app and UxPlay |

States: `starting` gray · `waiting` blue · `connecting` yellow · `live` green ·
`stopped: mirror again on the device` yellow, the window is hidden (it would keep the last frame) ·
`receiving video but no window` yellow · `reconnecting` yellow, UxPlay crashed and is coming back ·
`start error` / `stopped: crashed 3 times` red.

The tray icon is **red** if any instance has an error, else **green** if any device is live, else
**yellow** if one needs attention, else **blue**. Hover it to see how many are live.

## Resolution and FPS

AirPlay requires the receiver to declare a display size; the sender scales its screen to fit
inside it. A 16:9 size would shrink a portrait phone to a fraction of its pixels, so the app asks
for a **square**: portrait and landscape devices both fit at full size.

The sender also scales **up** to fill the declared size, so Native asks for 2880×2880 and not more:
with 3840×3840 an iPhone 17 Pro Max, natively 1320×2868, sent 1768×3840, upscaled by the phone and
heavier on the network with no extra detail. 2880 is about the native height of a recent iPhone,
so it sends roughly its own resolution, and it still fits an iPad Pro's 2752 at full size.

| Option | UxPlay flags | When |
|---|---|---|
| **Native** (default) | `-h265 -s 2880x2880` | The device sends about its native resolution, e.g. about 1325×2880 on an iPhone 17 Pro Max, in H.265. Best quality; needs a good 5 GHz network |
| **FHD** | `-s 1920x1920` | H.264, longest side up to 1920. Less bandwidth and CPU |
| **HD** | `-s 1280x1280` | H.264, longest side up to 1280. Weak Wi-Fi or many devices at once |
| **SD** | `-s 854x854` | H.264, longest side up to 854. Very weak Wi-Fi, last resort |

**FPS** sets the maximum frame rate (`-fps 60` or `-fps 30`). 30 lowers bandwidth and CPU. It is a
maximum: the device may send fewer frames, and the tray shows what it actually sends, from the
reports it makes once a second (UxPlay's `-FPSdata`). The first report of each session is written
to the log, the ones after it are not.

A change applies to each instance as soon as nobody is mirroring on it: a live device keeps its
current settings, and its tray line says what it gets on reconnect, e.g. `SD on reconnect`, until
it stops mirroring. **Apply now** in its submenu ends that device's session and restarts its
instance with the new settings; tap LIVE again on the device. **Restart** applies it everywhere.

## When the connection drops

| What happens | What the app does |
|---|---|
| Device Wi-Fi blips | Mirror again to LIVE on the device. The free instance takes it, with the same window title, and the old instance restarts at once |
| Device disappears without notice | UxPlay drops the session after 15 s without response and the instance is LIVE again; its window, hidden (it would keep the last frame), may stay open under the device's name, until another device connects to it, or the instance restarts when the same device reconnects on another one |
| UxPlay exits or crashes | Restarts in 2 s. If it crashes 3 times in a row right after starting, it stops and warns once, to avoid a loop: check the log, then change **Resolution** or **FPS**, which starts the stopped instances again, or use **Restart** |
| PC network changes: cable/Wi-Fi back, new IP | Restarts the instances nobody is mirroring on, so they announce on the new network. Live ones continue |
| PC resumes from sleep | Same |
| App started twice | Asks whether to close the running copy, whose icon may be hidden under the tray ^ arrow. **Yes** closes it and starts the new one |

No receiver can make the device resume mirroring by itself: when mirroring drops, the device ends
it on its side. Tap **Screen Mirroring → LIVE** again; its window comes back with the same title,
on the same OBS source.

## Privacy: lock screen and frozen frames

When the device's screen goes off (Auto-Lock, side button), AirPlay stops sending video but keeps the
session, and the window would freeze on the last frame, often the lock screen. When it wakes, the
lock screen and the passcode keypad would show live, with each key flashing as it is tapped. So the
app hides the video window (fully transparent: the screen and OBS get nothing, the OBS source shows
what is behind it, black on an empty scene):

- while the device's screen is off: the line in the tray says `screen off: black`;
- for `wakeBlackSeconds` (10 s by default) after it comes back on, to cover the unlock: `just woke: black`;
- whenever nobody is mirroring on that window (the session ended or dropped: its last frame is frozen);
- while **Black screen** is on in the tray, for every device, to hide everything by hand at any time.

The screen-off and screen-on signals come from AirPlay itself (UxPlay is patched to log them), so
this works without guessing from the picture. For no gap at all, keep the device from locking during
the show: **Settings → Display & Brightness → Auto-Lock → Never**, and, to keep the side button from
locking it, **Guided Access** with the side button disabled.

## Config

`%LOCALAPPDATA%\iPhoneScreenshareServer\config.json` (tray → Open config). After editing, use
**Restart**. If the app needs to save, for example a new device, before you press **Restart**, it
reads your edit first and keeps it. If the file has an error, such as an extra comma, the app warns
and applies nothing, without touching the file. If the error is there at startup, the app runs with
the defaults in memory, saves a copy to `config.json.broken` and doesn't overwrite your file; once
you fix it, the app reads it again by itself, or use **Restart**. If the file can't be read, such as
when another program has it locked, the app doesn't overwrite it either and tries to read it again
every 5 s. Changes made in the app meanwhile, new and forgotten devices, **Resolution** and **FPS**,
are merged into the file when it reads fine, unless the file changed that same device or setting.
If a save fails, for example because the file is read-only, the app keeps the changes in memory,
retries with a growing delay and warns once. The file is saved atomically, so a crash or power loss
never leaves it cut off, and a copy of the last save is kept in `config.json.bak`.

```json
{
  "name": "LIVE",
  "maxSlots": 8,
  "resolution": "native",
  "fps": 60,
  "uxplay": "",
  "extraArgs": [],
  "wakeBlackSeconds": 10,
  "devices": {
    "22:C5:83:7E:F0:3F": { "name": "Otavio Augusto’s iPhone", "label": "", "window": "Otavio Augusto’s iPhone" },
    "AA:BB:CC:DD:EE:FF": { "name": "iPhone", "label": "Player 2", "window": "Player 2" }
  }
}
```

| Key | Purpose |
|---|---|
| `name` | The one name shown in Screen Mirroring |
| `maxSlots` | Max devices at once, 1 to 8 |
| `resolution` | `native`, `fhd`, `hd` or `sd`, see [Resolution and FPS](#resolution-and-fps) |
| `fps` | `60` or `30` |
| `uxplay` | Path to another `uxplay.exe`. Empty = the embedded one. It needs the pool patch to hide busy instances |
| `extraArgs` | Extra UxPlay options for every instance, added last so they override the app's. E.g. `["-reset", "30"]` to tolerate 30 s without response before dropping the session |
| `wakeBlackSeconds` | How long the window stays black after the device's screen comes back on (lock screen, passcode), 0 to 60. Default 10. See [Privacy](#privacy-lock-screen-and-frozen-frames) |
| `devices` | Remembered devices, by the `deviceID` they send over AirPlay. Filled by the app: `name` is what the device sends, `window` the window title it got (kept so it never changes). Set `label` to name the device yourself, e.g. `"Player 2"`: it replaces the name in the window title and the tray from its next connection, so re-point its OBS source once. Deleting an entry, or **Forget device**, frees its title |

Configs of earlier versions are converted automatically. v0.2.0's slot `bindings` become
`devices`, so bound devices are remembered (the original file is kept as `config.json.v2`). The
Portuguese keys of the first versions (`nome`, `slots`, `qualidade`, `argsExtras`, `vinculos`) are
converted too: `FHD` becomes `native`, `HD` becomes `hd`, and the original file is kept as
`config.json.v1`. To go back to an older version, restore that file as `config.json`, since older
versions don't read the new keys. If a Portuguese-era version is started by mistake, it rewrites
`config.json` without the new settings; the next start of this version restores them from
`config.json.bak` and keeps any device the older version bound.

Separately, the `resolution` values `1920` and `1280` of earlier English versions become `fhd`
and `hd`, and an unknown value becomes `native`.

## Tips for a live show

- PC on **Ethernet**; devices on the **5 GHz** Wi-Fi of the same router.
- If another phone is the camera (Iriun etc.), connect it over **USB** so it doesn't share the Wi-Fi.
- On the mirrored device: **Do Not Disturb** on and Auto-Lock **Never** (the window goes black while
  the screen is off and right after it wakes; see [Privacy](#privacy-lock-screen-and-frozen-frames)).

## Troubleshooting

| Symptom | What to do |
|---|---|
| SmartScreen "Windows protected your PC" | **More info → Run anyway**, or **Unblock** in the file's Properties. Signed releases build SmartScreen reputation over time |
| "Already running" at startup | Answer **Yes** to replace it. If it says the running copy is an older version or runs as administrator, quit it from its tray menu and start again |
| LIVE doesn't show on the device | Same network? Guest networks and router "client isolation" block it. Check the rule "iPhone Screenshare Windows Server v2" in *Windows Firewall → Inbound Rules*; it covers Private and Public, local subnet only. If the tray says it is full, no LIVE is free |
| Tapping LIVE doesn't connect | Wait a second and tap again: right after a device connects, the next free instance is still starting |
| Device connects but OBS is black / no window | Open log: if `Begin streaming` shows and no window with the device's name, use **Restart**; check that OBS uses `[uxplay.exe]: <device name>` |
| OBS source shows nothing after changing a label | The window title changed to the label: pick the new title in the source once |
| Black in OBS with the right window | Is the window minimized? Restore it. Otherwise change the Window Capture method, or use Display Capture with a crop |
| Stuttering video | Lower **Resolution** to FHD, HD or SD, or **FPS** to 30, then **Apply now** on that device, or stop and start Screen Mirroring on it |
| Red icon | Open log; the last line says why |
| Antivirus complains | Allow `%LOCALAPPDATA%\iPhoneScreenshareServer\` |

## How it works

```
iPhoneScreenshareServer.exe (Go, tray)
 ├─ embedded bundle/uxplay.zip ── extracted once ──> %LOCALAPPDATA%\iPhoneScreenshareServer\uxplay-<hash>\
 │                                                    bin\uxplay.exe + DLLs, GStreamer plugins
 ├─ per instance: uxplay.exe -n LIVE -nh -p <its ports> -m <its MAC> -vs d3d11videosink
 │                -h265 -s 2880x2880 -fps 60 -FPSdata   (Resolution/FPS from the tray)
 ├─ pool: one instance per live device plus exactly one free, up to maxSlots
 ├─ reads UxPlay output: "connection request from <device> ... deviceID = <id>" and the accept → the
 │                       device's window title, remembered in the config;
 │                       "mDNS: advertised at startup" / "mDNS: hidden while mirroring" /
 │                       "mDNS: mirroring ended, advertised again" (the patch);
 │                       "Begin streaming" / session end → live / stopped;
 │                       "codec is h264|h265: geometry is (w,h)=(…) (source)" → codec, orientation and size;
 │                       the device's FPS report (a plist) → its frame rate
 ├─ renames each video window to its device's name (OBS finds the source by title)
 ├─ window events: each new video window stays transparent until it is fitted to the video
 │                 (no black bars) at show, so it is first seen in shape; fitted again on rotation
 ├─ watches the process: crashed → restart; 3 quick crashes → stop and warn
 ├─ watches the network: IP changed or PC resumed → restarts idle instances
 └─ Windows job object: however the app exits, uxplay exits with it
```

UxPlay and GStreamer are built in GitHub Actions (Windows + MSYS2 UCRT64) from a pinned commit of
[FDH2/UxPlay](https://github.com/FDH2/UxPlay) (≥ 1.74, which has its own mDNS) with two changes:
unbuffered output, so the app sees at once which device connected, and the pool patch,
[`ci/uxplay-pool.patch`](ci/uxplay-pool.patch). With the patch an instance removes its mDNS
advertisement when a device starts mirroring and adds it again when mirroring stops, logging
`mDNS: hidden while mirroring` and `mDNS: mirroring ended, advertised again` (and
`mDNS: advertised at startup`, plus `mDNS: announced again` for each repeat at +1, +3 and +8 s);
the change runs on UxPlay's main loop, since its mDNS code is not thread-safe. Every instance has
the same name and announces with the cache-flush bit, so once an instance announces again after a
session, the devices resolve LIVE to it: the app takes the end line as the end of the session,
makes that instance the free one as it is (no restart, unless new settings wait for it) and takes
the other idle instance down about 5 s later. If announcing again fails (`mDNS: mirroring ended, advertising again
failed: still hidden`), the instance stays hidden and the app restarts it when it is needed. Its
goodbye only withdraws its own records (SRV, TXT and its own `_raop` PTR), not the ones it shares
with the free instance, and the app keeps as the free one the instance that is not hidden and
announced itself last, timed from those log lines. The build fails if the patch doesn't apply
to the pinned commit.

CI runs `go vet` and the tests (`main_test.go`) and tests the result on a clean Windows: all
GStreamer elements present, the patch compiled in, `uxplay.exe` starting with each resolution
preset and `-FPSdata`, `iPhoneScreenshareServer.exe` extracting and keeping exactly one free
instance up, and, with a fake uxplay (`ci/fake-uxplay`) that simulates a device: the device
remembered, exactly one new free instance with its own MAC, the patch's mDNS lines (the end of
the session read from the patch's line alone, then the ended instance, advertised again, kept as
the one free instance without a restart and the other idle one down), the reported
fps with only the first report in the log, the codec, geometry and rotation, a visible window named
after the device that, sized to the full video and then shown like the real video sink does, is
first seen already in the portrait video's shape and fitted to the landscape one after the
rotation, and the close button's tooltip window left alone.

The app sends no data anywhere. It only talks to devices on the local network over AirPlay.

## Build

The `.exe` comes from the [`build`](.github/workflows/build.yml) workflow:

- every push to `main` produces the `iPhoneScreenshareServer` artifact, unsigned;
- a `v*` tag publishes a release;
- on a `v*` tag, when the `SIGNPATH_API_TOKEN` secret is set, the exe is signed through SignPath
  before it is uploaded and released. With the secret, the repository variable
  `SIGNPATH_ORGANIZATION_ID` is required. Optional variables: `SIGNPATH_PROJECT_SLUG` (default
  `iphone-screenshare-windows-server`) and `SIGNPATH_SIGNING_POLICY_SLUG` (default
  `release-signing`). A test-signing policy also works for trying the pipeline; its signature
  is not trusted by Windows, so the run only warns about it. The run waits up to one hour for the manual approval in SignPath; if it is
  not approved by then, the job fails and the tag's workflow has to be re-run. Without the secret
  the build is unsigned and otherwise identical. The token is only passed to the job on `v*` tags.

To update UxPlay, change `UXPLAY_REF` in the workflow to the new commit and check that
`ci/uxplay-pool.patch` still applies (`git apply --check`); the build stops if it doesn't.

Locally (needs `bundle/uxplay.zip` from CI; to just compile and test, an empty zip works). The
app is Windows-only: the tests run on Windows, and on other systems use `GOOS=windows` to vet and
build.

```
GOOS=windows go vet ./...
go test ./...            # on Windows
GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -s -w" -o dist/iPhoneScreenshareServer.exe .
```

| File | Role |
|---|---|
| `main.go` | Tray app |
| `main_test.go` | Tests for the pool, args, window titles, the window filter, log and FPS report parsing and config migration |
| `ci/build-bundle.sh` | Builds UxPlay with `ci/uxplay-pool.patch` and `bundle/uxplay.zip` (in MSYS2) |
| `ci/uxplay-pool.patch` | UxPlay patch: hidden from Screen Mirroring while mirroring |
| `ci/smoke-test.ps1` | Smoke test on the CI Windows runner |
| `ci/fake-uxplay/` | Fake uxplay that simulates a device, for the smoke test |

## Code signing policy

Free code signing provided by [SignPath.io](https://signpath.io), certificate by
[SignPath Foundation](https://signpath.org).

- Committers and reviewers: [ottospike](https://github.com/ottospike)
- Approvers: [ottospike](https://github.com/ottospike)

Only release binaries, built by this repository's GitHub Actions workflow from a `v*` tag, are
signed. Every release is approved manually before signing.

**Privacy:** this program does not send any data to any server. It only receives AirPlay
connections from devices on your local network; logs and config stay on your PC.

## License

GPL-3.0, the same as the embedded UxPlay. Third-party components inside the `.exe`:
[UxPlay](https://github.com/FDH2/UxPlay) (GPL-3.0),
[GStreamer](https://gstreamer.freedesktop.org/) (LGPL-2.1+),
[FFmpeg](https://ffmpeg.org/) via gst-libav (LGPL/GPL) and the
[MSYS2](https://www.msys2.org/) libraries they depend on.
