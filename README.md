# iPhone Screenshare Windows Server

Receives **AirPlay screen mirroring** from iPhone, iPad or Mac on Windows over Wi-Fi, in a window
that **OBS** captures. One `.exe`: no installer, no Bonjour, no account, no trial.

Under the hood it runs [UxPlay](https://github.com/FDH2/UxPlay), an open source AirPlay receiver,
with its own embedded mDNS, so devices find the PC without Apple's Bonjour. The app lives in the
Windows tray, keeps UxPlay running and restarts it if it crashes.

Several devices at once, each in its own **slot**: the device that connected to LIVE 1 is bound to
it and, if it drops and reconnects, it comes back to LIVE 1, on the same OBS source. There is
always a free LIVE for the next device.

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
2. On the device: **Control Center → Screen Mirroring → LIVE 1**. The next device sees **LIVE 2**,
   then **LIVE 3**, and so on.
3. In OBS, one source per slot (see [OBS setup](#obs-setup)).

## Dynamic slots

There is always one **free LIVE N** in Screen Mirroring. The device that connects to it is
**bound** to the slot by its AirPlay `deviceID`. The binding is saved in the config and survives
app and PC restarts. The app then announces the next free one, up to `maxSlots` (default 8).

- Every device sees **all** running LIVE N. Slots bound to other devices refuse the connection
  (the log shows `refused <device>`); a device uses its own LIVE or the current free one.
- Each slot is a separate AirPlay receiver with its own name, ports and MAC address, so the
  device lists them as different receivers.
- On the free slot the first device keeps the screen: a second device tapping the same LIVE is
  refused and uses the next LIVE, instead of dropping the first.
- Locks apply when nobody is mirroring on the slot (the app restarts that slot's UxPlay in the
  background), so nobody live is dropped.
- To give a slot to another device: tray menu → `LIVE N` → **Unbind this slot**. A device still
  mirroring stays live until it stops; the slot then becomes free again. The unbound device is not
  rebound to the same slot until the slot restarts.
- A device that gets into a slot that isn't its own, before the locks apply, is dropped at once:
  the slot restarts with the locks.
- When all slots up to `maxSlots` are bound there is no free LIVE; the tray says so. Unbind one.
- `"maxSlots": 1` is single mode: one **LIVE**, no binding, any device connects.

## OBS setup

One **Window Capture** source per slot: **Sources → + → Window Capture**.

- **Window**: `[uxplay.exe]: Slot 1` (and `Slot 2`, `Slot 3`… in the other sources). The window
  only exists while a device mirrors: connect once so it shows in the list.
- **Capture Method**: **Windows 10 (1903 and up)**, with **Client area** checked: only the video
  is captured, without the title bar, buttons or borders.
- **Window Match Priority**: **Window title must match**. The window closes when the slot
  restarts and comes back with the same title, so each source always finds its own slot.
- Right-click the source → **Transform → Edit Transform** → **Bounding Box Type**:
  **Scale to inner bounds**, and set the bounding box size to the area you want. The source then
  fits portrait and landscape automatically when the device rotates.

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
- Quality comes from what the device sends (**Native**: its own resolution). The window only
  needs to show the whole video, and it may be smaller than the video: OBS captures the whole
  picture, at the window's size.

### Audio

Audio is always on. Each slot plays its device's sound on the PC's default output and is also a
separate audio app that OBS can capture:

- Add **Sources → + → Application Audio Capture**, pick `[uxplay.exe]: Slot N` and set
  **Window Match Priority** to **Window title must match**, the same as for Window Capture.
- Mute or unmute each slot in the OBS **Audio Mixer**.
- Since the sound also plays on the PC's default output, mute **Desktop Audio** in OBS, or set
  it to another device, to avoid capturing every slot twice.
- AirPlay has 100–200 ms of latency; adjust **Sync Offset** in Advanced Audio Properties if needed.

## Tray menu

| Item | What it does |
|---|---|
| First line | How many slots are bound, or that all are bound; with `maxSlots` 1, `Single LIVE · any device` |
| `LIVE 2 · <device> · <state> · H.265 · portrait 1320×2868` | One per running slot: bound device or `free`, state, and while live the codec, H.264 or H.265, the orientation and the real source resolution, updated on every rotation. Submenu: **Unbind this slot** |
| **Resolution** | Native · FHD · HD · SD, for all slots. Saved; each slot's UxPlay restarts with it once nobody is mirroring on it, so live devices are not dropped |
| **FPS** | 60 · 30, for all slots. Saved; applied the same way |
| **Restart** | Rereads the config and restarts every slot. If the config has an error or can't be read, nothing restarts and live devices stay connected |
| **Open log** | App and UxPlay log in Notepad |
| **Open config** | `config.json` in Notepad |
| **Quit** | Closes the app and UxPlay |

Slot states: `starting` gray · `waiting` blue · `connecting` yellow · `live` green ·
`stopped: mirror again on the device` yellow, the window keeps the last frame ·
`receiving video but no window` yellow · `reconnecting` yellow, UxPlay crashed and is coming back ·
`start error` / `stopped: crashed 3 times` red.

The tray icon is **red** if any slot has an error, else **green** if any device is live, else
**yellow** if a slot needs attention, else **blue**. Hover it to see how many are live and which
LIVE is free.

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

**FPS** sets the maximum frame rate (`-fps 60` or `-fps 30`). 30 lowers bandwidth and CPU.
A change applies to each slot as soon as nobody is mirroring on it: a live device keeps its current
settings until it stops mirroring, or use **Restart** to apply it everywhere now.
The codec and the actual source size of each slot are shown in the tray menu and in the log.

## When the connection drops

| What happens | What the app does |
|---|---|
| Device Wi-Fi blips | Mirror again on the device. A bound slot accepts it at once (`-nohold`), without waiting for the old session to expire |
| Device disappears without notice | UxPlay drops the session after 15 s without response; the window keeps the last frame |
| UxPlay exits or crashes | Restarts in 2 s. If it crashes 3 times in a row right after starting, it stops and warns once, to avoid a loop: check the log, then change **Resolution** or **FPS**, which starts the stopped slots again, or use **Restart** |
| PC network changes: cable/Wi-Fi back, new IP | Restarts the slots nobody is mirroring on, so they announce on the new network. Live ones continue |
| PC resumes from sleep | Same |
| App started twice | Asks whether to close the running copy, whose icon may be hidden under the tray ^ arrow. **Yes** closes it and starts the new one |

No receiver can make the device resume mirroring by itself: when mirroring drops, the device ends
it on its side. Tap **Screen Mirroring → LIVE N** again; it returns to the same slot and OBS source.

## Config

`%LOCALAPPDATA%\iPhoneScreenshareServer\config.json` (tray → Open config). After editing, use
**Restart**. If the app needs to save, for example a new binding, before you press **Restart**, it
reads your edit first and keeps it. If the file has an error, such as an extra comma, the app warns
and applies nothing, without touching the file. If the error is there at startup, the app runs with
the defaults in memory, saves a copy to `config.json.broken` and doesn't overwrite your file; once
you fix it, the app reads it again by itself, or use **Restart**. If the file can't be read, such as
when another program has it locked, the app doesn't overwrite it either and tries to read it again
every 5 s. Changes made in the tray meanwhile, bindings, unbinds, **Resolution** and **FPS**, are
merged into the file when it reads fine, unless the file changed that same slot, device or setting.
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
  "bindings": {
    "1": { "deviceID": "AA:BB:CC:DD:EE:FF", "device": "Jane's iPhone" }
  }
}
```

| Key | Purpose |
|---|---|
| `name` | Name shown in Screen Mirroring: `LIVE 1`, `LIVE 2`…; with `maxSlots` 1, just `LIVE` |
| `maxSlots` | Max devices at once, 1 to 8. 1 = a single LIVE, no binding |
| `resolution` | `native`, `fhd`, `hd` or `sd`, see [Resolution and FPS](#resolution-and-fps) |
| `fps` | `60` or `30` |
| `uxplay` | Path to another `uxplay.exe`. Empty = the embedded one |
| `extraArgs` | Extra UxPlay options for every slot, added last so they override the app's. E.g. `["-reset", "30"]` to tolerate 30 s without response before dropping the session |
| `bindings` | Which device is bound to each slot. Filled by the app; deleting an entry unbinds it. Bindings above `maxSlots` are ignored |

Configs from earlier versions with Portuguese keys (`nome`, `slots`, `qualidade`, `argsExtras`,
`vinculos`) are converted automatically, bindings included: `FHD` becomes `native`, `HD` becomes
`hd`. The original file is kept as `config.json.v1`; to go back to an older version, restore it
as `config.json`, since older versions don't read the new keys. If an older version is started by
mistake, it rewrites `config.json` without the new settings; the next start of this version
restores them from `config.json.bak` and keeps any device the older version bound.

Separately, the `resolution` values `1920` and `1280` of earlier English versions become `fhd`
and `hd`, and an unknown value becomes `native`.

## Tips for a live show

- PC on **Ethernet**; devices on the **5 GHz** Wi-Fi of the same router.
- If another phone is the camera (Iriun etc.), connect it over **USB** so it doesn't share the Wi-Fi.
- On the mirrored device: **Do Not Disturb** on and Auto-Lock off.

## Troubleshooting

| Symptom | What to do |
|---|---|
| SmartScreen "Windows protected your PC" | **More info → Run anyway**, or **Unblock** in the file's Properties. Signed releases build SmartScreen reputation over time |
| "Already running" at startup | Answer **Yes** to replace it. If it says the running copy is an older version or runs as administrator, quit it from its tray menu and start again |
| LIVE doesn't show on the device | Same network? Guest networks and router "client isolation" block it. Check the rule "iPhone Screenshare Windows Server v2" in *Windows Firewall → Inbound Rules*; it covers Private and Public, local subnet only |
| Only one LIVE shows but the menu lists several | Not normal: every device should see all running LIVE. Check network/firewall and that the app is up to date |
| Tapping LIVE doesn't connect | If the slot belongs to another device this is expected: the log shows `refused <device>`. Use your LIVE or the free one |
| Device connects but OBS is black / no window | Open log: if `Begin streaming` shows and no `Slot N` window, use **Restart**; check that OBS uses `[uxplay.exe]: Slot N` |
| Black in OBS with the right window | Is the window minimized? Restore it. Otherwise change the Window Capture method, or use Display Capture with a crop |
| Stuttering video | Lower **Resolution** to FHD, HD or SD, or **FPS** to 30, then stop and start Screen Mirroring on that device: a live device keeps the old setting until it reconnects |
| Red icon | Open log; the last line says why |
| Antivirus complains | Allow `%LOCALAPPDATA%\iPhoneScreenshareServer\` |

## How it works

```
iPhoneScreenshareServer.exe (Go, tray)
 ├─ embedded bundle/uxplay.zip ── extracted once ──> %LOCALAPPDATA%\iPhoneScreenshareServer\uxplay-<hash>\
 │                                                    bin\uxplay.exe + DLLs, GStreamer plugins
 ├─ per slot: uxplay.exe -n "LIVE N" -nh -p <slot ports> -m <slot MAC> -vs d3d11videosink
 │            -h265 -s 2880x2880 -fps 60   (Resolution/FPS from the tray)
 │            + bound: -restrict -allow <deviceID> -nohold; other slots: -block <deviceID>
 ├─ dynamic slots: one running per bound device plus one free, up to maxSlots
 ├─ reads UxPlay output: "connection request from <device> ... deviceID = <id>" and the accept → binds the slot;
 │                       "Begin streaming" / session end → live / stopped;
 │                       "codec is h264|h265: geometry is (w,h)=(…) (source)" → codec, orientation and size
 ├─ renames each UxPlay video window to "Slot N" (OBS finds the source by title)
 ├─ window events: each new video window stays transparent until it is fitted to the video
 │                 (no black bars) at show, so it is first seen in shape; fitted again on rotation
 ├─ watches the process: crashed → restart; 3 quick crashes → stop and warn
 ├─ watches the network: IP changed or PC resumed → restarts idle slots
 └─ Windows job object: however the app exits, uxplay exits with it
```

UxPlay and GStreamer are built in GitHub Actions (Windows + MSYS2 UCRT64) from a pinned commit of
[FDH2/UxPlay](https://github.com/FDH2/UxPlay) (≥ 1.74, which has its own mDNS) with a one-line
patch: unbuffered output, so the app sees at once which device connected. CI runs `go vet` and the
tests (`main_test.go`) and tests the result on a clean Windows: all GStreamer elements present,
`uxplay.exe` starting with each resolution preset, `iPhoneScreenshareServer.exe` extracting and
keeping the free slot up, and, with a fake uxplay (`ci/fake-uxplay`) that simulates a device, the
binding, the new slot, distinct MACs, the codec, geometry and rotation, a visible `Slot 1` window
that, sized to the full video and then shown like the real video sink does, is first seen already
in the portrait video's shape, and that window fitted to the landscape one after the rotation.

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

To update UxPlay, change `UXPLAY_REF` in the workflow to the new commit.

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
| `main_test.go` | Tests for slot rules, args, log parsing and config migration |
| `ci/build-bundle.sh` | Builds UxPlay and `bundle/uxplay.zip` (in MSYS2) |
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
