# DLSS 5 Patcher

DLSS 5 Neural Rendering patcher for Windows games, built with Wails v2 (Go + Vanilla JS).

<p align="center">
  <img src="src/frontend/src/assets/images/preview_1.png" alt="DLSS 5 Patcher - Game List" width="500">
</p>

## Features

Simple DLSS 5 patcher does the smart work automatically:

- **Native-DLSS detection**: games that ship their own `nvngx_dlss.dll` (Unreal, Cyberpunk, etc.) get the Direct RenoDX path *without* the DLSS5-Feeder.
- **Upscaling disabled by default**: writes `NREnableUpscaling=0` into every config section the add-on reads. This app boots with it OFF: no overlay toggling.
- **Pre-configured ReShade preset**: iMMERSE: Launchpad and DLSS 5 Feed are enabled and ordered automatically, so there's no manual in-game setup.
- **Multi-drive, multi-platform detection**: scans every active drive, Steam `libraryfolders.vdf`, Epic manifests, Ubisoft Connect, GOG Galaxy, EA/Origin, and Xbox Games. Games appear in the UI live as they're found.
- **Deep clean uninstall**: scans every subdirectory (`Binaries/Win64`, `_retail_`, etc.), removes all ReShade/DLSS files, and restores originals from `.dlss5_backup`.
- **Patch → Launch flow**: after patching, the game stays selected and is one click away from launching.

## Easy to Use

Just select a game and click **Patch**. The app handles everything else automatically, including ReShade installation, DLSS 5 add-ons, and config tweaks.

<p align="center">
  <img src="src/frontend/src/assets/images/preview_2.png" alt="DLSS 5 Patcher - Game List" width="500">
</p>

---

### Download App Here: [DLSS 5 Patcher](https://github.com/MRHRTZ/DLSS5-Patcher/releases/download/v1.0.0/DLSS.5.Patcher.v1.0.0.zip)

---

# Development
```bash
wails dev   # from src/
```

### Build & Deploy
```bash
wails build   # from src/
```
The binary is generated at `src/build/bin/DLSS 5 Patcher.exe`. Copy it to the workspace root.

## Project Structure
```
DLSS 5 Setup/
├── DLSS 5 Patcher.exe         # Production launcher
├── ReShadeConfig.json         # Optional ReShade installer URL
├── ReShade/                   # ReShade dependencies
│   ├── Effects/               # ReShade shaders & textures
│   ├── Extracted/             # Pre-extracted ReShade64.dll & manifests
│   └── ReShade_Setup_6.8.0.exe  # Auto-downloaded if missing
├── data/                      # DLSS 5 neural rendering addons & DLLs
│   ├── dlss5-feed.addon64
│   ├── renodx-dlss5.addon64
│   ├── nvngx_dlss.dll
│   ├── nvngx_dlssnr.dll
│   └── reshade-shaders-source/
├── patcher.log                # Operation log
└── src/                       # Wails application source
```

## ReShade Installer Size

The official ReShade installer (`ReShade_Setup_6.8.0.exe`, ~4 MB) is **not** bundled with the repo to keep downloads small. Configure its download URL in `ReShadeConfig.json`:

```json
{
  "url": ""
}
```

If `url` is empty, the app downloads from `https://reshade.me/downloads/ReShade_Setup_6.8.0.exe`. The installer is cached in `ReShade/` for future patches, then `ReShade64.dll` is extracted into `ReShade/Extracted/`.

## License
This project is licensed under the MIT License. See the [LICENSE](https://mit-license.org) file for details.