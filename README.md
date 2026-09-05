# DLSS 5 Patcher

DLSS 5 Neural Rendering patcher for Windows games **ReShade or OptiScaler, one click**. Built with Wails v2 (Go + Vanilla JS).

<p align="center">
  <img src="src/frontend/src/assets/images/preview_1.png" alt="DLSS 5 Patcher - Game List" width="500">
</p>

### ReShade + DLSS 5 Add-On
Best for DX11 / DX12 / Vulkan with full ReShade control.
- Installs ReShade with Addon Support and DLSS 5 add-ons (RenoDX + optional Feeder)
- Native-DLSS games automatically use Direct path without Feeder to avoid conflicts
- Pre-configured iMMERSE: Launchpad and DLSS 5 Feed preset, no manual in-game setup

### OptiScaler (FSR / XeSS / DLSS)
Best for broad API coverage, including Vulkan and DX11.
- Proxy DLL `dxgi.dll` with automatic backend selection
- Auto-tuned configuration, works where ReShade is limited

### Common Features
- **Multi-drive, multi-platform detection**: scans every active drive, Steam libraries, Epic, Ubisoft Connect, GOG Galaxy, EA/Origin, and Xbox Games. Games appear live as they are found.
- **Deep clean uninstall**: scans every subdirectory and restores originals from backup.
- **Live progress & patch flow**: real-time status during patch and uninstall, and one-click launch after patching.

## Easy to Use

Just select a game, choose a mode, and click **Patch**. The app handles everything else automatically, including ReShade/OptiScaler installation, DLSS 5 add-ons, and config tweaks.

<p align="center">
  <img src="src/frontend/src/assets/images/preview_2.png" alt="DLSS 5 Patcher - Details" width="500">
</p>

---

### Download App: [DLSS.5.Patcher.v1.2.0.zip](https://github.com/MRHRTZ/DLSS5-Patcher/releases/download/v1.2.0/DLSS.5.Patcher.v1.2.0.zip)

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
├── config.json                # Unified config (gpu_selection, reshade_setup_url, reshade_url, optiscaler_url, dlss5_url)
├── data/
│   ├── reshade-setup/         # ReShade with Addon Support
│   │   ├── Extracted/         # Pre-extracted ReShade64.dll & manifests
│   │   └── ReShade_Setup_6.8.0_Addon.exe  # Auto-downloaded from config if missing
│   ├── reshade/               # ReShade DLSS 5 add-ons & shaders
│   │   ├── dlss5-feed.addon64
│   │   ├── renodx-dlss5.addon64
│   │   └── reshade-shaders-source/
│   ├── optiscaler/            # OptiScaler proxy + backends
│   │   ├── OptiScaler.dll
│   │   ├── OptiScaler.ini.default
│   │   └── OptiScaler/        # libxess, amd_fidelityfx_*, D3D12_OptiScaler/
│   └── dlss5/                 # Shared DLSS Neural Rendering DLLs
│       ├── nvngx_dlss.dll
│       ├── nvngx_dlssnr.dll
│       └── nvngx.dll_dlssnr.dll
├── patcher.log                # Operation log
└── src/                       # Wails application source
```

## Configuration

`config.json` is auto-created if missing. Leave a `*_url` empty to show an error dialog instead of silently failing. `reshade_setup_url` supports direct `.exe` URLs:

```json
{
  "gpu_selection": "",
  "reshade_setup_url": "https://reshade.me/downloads/ReShade_Setup_6.8.0_Addon.exe",
  "reshade_url": "",
  "optiscaler_url": "",
  "dlss5_url": ""
}
```

## Changelog

See [RELEASE NOTES](https://github.com/MRHRTZ/DLSS5-Patcher/releases) for full details.

## License
This project is licensed under the MIT License. See the [LICENSE](https://mit-license.org) file for details.