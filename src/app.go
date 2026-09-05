package main

import (
	"archive/zip"
	"bytes"
	"context"
	"debug/pe"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows/registry"
)

// --- DXGI GPU Detection via COM syscall ---

var (
	moddxgi  = syscall.NewLazyDLL("dxgi.dll")
	modole32 = syscall.NewLazyDLL("ole32.dll")

	procCreateDXGIFactory1 = moddxgi.NewProc("CreateDXGIFactory1")
	procCoInitializeEx     = modole32.NewProc("CoInitializeEx")
	procCoUninitialize     = modole32.NewProc("CoUninitialize")
)

// {770AAE78-F26F-4DBA-A829-253C83D1B387} — IDXGIFactory1
var IID_IDXGIFactory1 = [16]byte{0x78, 0xae, 0x7a, 0x77, 0x6f, 0xf2, 0xba, 0x4d, 0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}

// dxgiAdapterDesc1 mirrors the native DXGI_ADAPTER_DESC1 structure.
// Field order MUST match the Windows SDK definition exactly.
type dxgiAdapterDesc1 struct {
	Description           [128]uint16 // WCHAR[128] — comes first
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr // SIZE_T
	DedicatedSystemMemory uintptr // SIZE_T — was missing
	SharedSystemMemory    uintptr // SIZE_T
	AdapterLuid           struct {
		LowPart  uint32
		HighPart int32
	}
	Flags uint32
}

// vtableCall retrieves a COM vtable entry by index and calls it via syscall.SyscallN.
// objPtr is the pointer to the COM object (which points to its vtable pointer).
// idx is the vtable slot index (0-based, starting from IUnknown::QueryInterface).
func vtableCall(objPtr uintptr, idx int, args ...uintptr) (uintptr, uintptr, error) {
	vtbl := *(*uintptr)(unsafe.Pointer(objPtr))
	procAddr := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	r1, r2, err := syscall.SyscallN(procAddr, append([]uintptr{objPtr}, args...)...)
	return r1, r2, err
}

// vtableRelease calls IUnknown::Release (vtable index 2).
func vtableRelease(objPtr uintptr) {
	vtableCall(objPtr, 2)
}

// NRTier describes which DLSS 5 Neural Rendering shim a GPU should use.
type NRTier string

const (
	NRTierUnknown  NRTier = ""
	NRTierNone     NRTier = "none"
	NRTierRTX20_30 NRTier = "rtx20-30"
	NRTierRTX40_50 NRTier = "rtx40-50"
)

// gpuInfo holds detected GPU metadata.
type gpuInfo struct {
	Name                    string
	SupportsNeuralRendering bool   // true when the GPU can run DLSS NR (RTX 20-50)
	Vendor                  string // "NVIDIA", "AMD", "Intel", "Virtual", ""
	VRAM                    uint64 // dedicated video memory in bytes
	NRTier                  NRTier // which neural-renderer shim this GPU needs
}

// classifyNVIDIA determines the neural-rendering tier from an NVIDIA GPU name.
// RTX 20-30 series use the full 158MB model shim; RTX 40-50 use the small 108KB
// forwarder shim. Any other NVIDIA card (GTX etc.) cannot run the pass.
func classifyNVIDIA(name string) (neural bool, tier NRTier) {
	lower := strings.ToLower(name)
	hasRTX := strings.Contains(lower, "rtx")
	hasGTX := strings.Contains(lower, "gtx")
	if !hasRTX && !hasGTX {
		// Modern Quadro/RTX A-series advertises "RTX" in the name too.
		return false, NRTierNone
	}
	isRTX40 := regexp.MustCompile(`rtx\s?40\d\d`).MatchString(lower)
	isRTX50 := regexp.MustCompile(`rtx\s?50\d\d`).MatchString(lower)
	isRTX20 := regexp.MustCompile(`rtx\s?20\d\d`).MatchString(lower)
	isRTX30 := regexp.MustCompile(`rtx\s?30\d\d`).MatchString(lower)
	if isRTX40 || isRTX50 {
		return true, NRTierRTX40_50
	}
	if isRTX20 || isRTX30 {
		// RTX 20-30 can still need the 158MB model shim only for *loading* the
		// pass, but Neural Rendering itself does not run on them (the NGX
		// runtime rejects the model). Keep the tier so the correct shim is
		// deployed, but report neural support as false so DLSS-NR is disabled.
		return false, NRTierRTX20_30
	}
	// RTX without a clear 4-number marker (e.g. "RTX A6000", "Quadro RTX").
	// Assume modern RTX is 40-50 tier (safe default); GTX is none.
	if hasRTX {
		return true, NRTierRTX40_50
	}
	return false, NRTierNone
}

// isVirtualAdapter reports whether a GPU name belongs to a virtual or remote
// display adapter (Parsec, StarDesk, RDP, Remote Display, etc.) that should
// not be treated as the real renderer.
func isVirtualAdapter(name string) bool {
	lower := strings.ToLower(name)
	virtualKeywords := []string{
		"parsec", "stardesk", "stardock", "remote display", "remote desktop",
		"virtual display", "virtual adapter", "rdp", "mirror driver",
		"idd", "indirect display", "basic display", "virtualbox",
		"vmware", "hyper-v", "hyperv", "mirage", "nuvoton", "displaylink",
	}
	for _, kw := range virtualKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// detectGPUs enumerates every real (non-virtual) display adapter via DXGI,
// falling back to the registry display-adapter class.  It returns the GPUs the
// machine actually uses for rendering (NVIDIA / AMD / Intel) plus any virtual
// adapters at the end, so callers can offer a selection.
var (
	gpuCacheOnce bool
	gpuCacheList []gpuInfo
)

func refreshGPUCache() {
	gpuCacheOnce = false
	gpuCacheList = nil
}

func detectGPUs() []gpuInfo {
	if gpuCacheOnce {
		return gpuCacheList
	}
	gpuCacheOnce = true
	gpuCacheList = detectGPUsUncached()
	return gpuCacheList
}

func detectGPUsUncached() []gpuInfo {
	var real []gpuInfo
	var virtual []gpuInfo

	// Initialize COM as MTA.
	var mta uintptr // COINIT_MULTITHREADED = 0x2
	procCoInitializeEx.Call(0, uintptr(unsafe.Pointer(&mta)), 0x2)
	defer procCoUninitialize.Call()

	// CreateIDXGIFactory1.
	var factoryPtr uintptr
	ret, _, _ := procCreateDXGIFactory1.Call(
		uintptr(unsafe.Pointer(&IID_IDXGIFactory1)),
		uintptr(unsafe.Pointer(&factoryPtr)),
	)
	if ret == 0 && factoryPtr != 0 {
		adapterIdx := uint32(0)
		for {
			var adapterPtr uintptr
			// IDXGIFactory1::EnumAdapters1 = vtable index 12
			ret, _, _ = vtableCall(factoryPtr, 12, uintptr(adapterIdx), uintptr(unsafe.Pointer(&adapterPtr)))
			if ret != 0 || adapterPtr == 0 {
				break
			}

			// IDXGIAdapter1::GetDesc1 = vtable index 10
			var desc dxgiAdapterDesc1
			rev, _, _ := vtableCall(adapterPtr, 10, uintptr(unsafe.Pointer(&desc)))
			if rev == 0 {
				name := strings.TrimRight(syscall.UTF16ToString(desc.Description[:]), "\x00")
				vendor := gpuVendor(desc.VendorID, name)
				neural, tier := false, NRTierNone
				if vendor == "NVIDIA" {
					neural, tier = classifyNVIDIA(name)
				}
				info := gpuInfo{
					Name:                    name,
					SupportsNeuralRendering: neural,
					Vendor:                  vendor,
					VRAM:                    uint64(desc.DedicatedVideoMemory),
					NRTier:                  tier,
				}
				if isVirtualAdapter(name) || vendor == "Virtual" {
					virtual = append(virtual, info)
				} else {
					real = append(real, info)
				}
			}

			vtableRelease(adapterPtr)
			adapterIdx++
		}
		vtableRelease(factoryPtr)
		writeLog(fmt.Sprintf("detectGPUs: DXGI enumerated %d real + %d virtual adapters", len(real), len(virtual)))
	} else {
		writeLog("detectGPU: CreateDXGIFactory1 failed (" + fmt.Sprintf("%x", ret) + ") — falling back to registry detection")
	}

	// If DXGI failed or returned nothing usable, fall back to the registry.
	if len(real) == 0 {
		regReal, regVirtual := detectGPUsFromRegistry()
		real = append(real, regReal...)
		virtual = append(virtual, regVirtual...)
	}

	// De-duplicate by (name, vendor) — DXGI and registry may both add entries.
	seen := map[string]bool{}
	var dedup []gpuInfo
	for _, g := range append(real, virtual...) {
		key := strings.ToLower(g.Name + "|" + g.Vendor)
		if seen[key] {
			continue
		}
		seen[key] = true
		dedup = append(dedup, g)
	}

	return dedup
}

// detectGPUsFromRegistry reads the display adapter class from the registry.
func detectGPUsFromRegistry() (real, virtual []gpuInfo) {
	const adapterClass = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`
	for i := 0; i < 16; i++ {
		keyPath := fmt.Sprintf(`%s\%04d`, adapterClass, i)
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, keyPath, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		desc, _, err := k.GetStringValue("DriverDesc")
		if err != nil || desc == "" {
			_ = k.Close()
			continue
		}
		// Prefer the more descriptive adapter name if present.
		if adapterStr, _, err2 := k.GetStringValue("HardwareInformation.AdapterString"); err2 == nil && adapterStr != "" {
			desc = adapterStr
		}
		_ = k.Close()

		vendor := gpuVendor(0, desc)
		neural, tier := false, NRTierNone
		if vendor == "NVIDIA" {
			neural, tier = classifyNVIDIA(desc)
		}
		info := gpuInfo{
			Name:                    desc,
			SupportsNeuralRendering: neural,
			Vendor:                  vendor,
			NRTier:                  tier,
		}
		if isVirtualAdapter(desc) || vendor == "Virtual" {
			virtual = append(virtual, info)
		} else {
			real = append(real, info)
		}
	}
	return real, virtual
}

// gpuVendor maps a PCI vendor id (and, as a secondary heuristic, the name) to
// a friendly vendor string.
func gpuVendor(vendorID uint32, name string) string {
	switch vendorID {
	case 0x10DE:
		return "NVIDIA"
	case 0x1002:
		return "AMD"
	case 0x8086:
		return "Intel"
	case 0x1414:
		return "Virtual" // Microsoft Basic/RDP/Remote adapters
	}
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "nvidia"):
		return "NVIDIA"
	case strings.Contains(lower, "amd") || strings.Contains(lower, "radeon"):
		return "AMD"
	case strings.Contains(lower, "intel"):
		return "Intel"
	case strings.Contains(lower, "parsec") || strings.Contains(lower, "stardesk") ||
		strings.Contains(lower, "remote display") || strings.Contains(lower, "virtual"):
		return "Virtual"
	}
	return ""
}

// pickPreferredGPU chooses the GPU that should be active for DLSS / NN by
// default.  Order of preference: NVIDIA (any) with most VRAM, then any other
// real GPU (AMD/Intel) with most VRAM, then virtual adapters.
func pickPreferredGPU(real, virtual []gpuInfo) gpuInfo {
	active := append(append([]gpuInfo{}, real...), virtual...)
	if len(active) == 0 {
		return gpuInfo{Name: "Unknown", Vendor: "", SupportsNeuralRendering: false}
	}

	// Prefer NVIDIA discrete (real) first, then any real, then virtual.
	score := func(g gpuInfo) (prio int, vram uint64) {
		prio = 3 // virtual
		switch g.Vendor {
		case "NVIDIA":
			prio = 0
		case "AMD", "Intel":
			prio = 1
		case "":
			prio = 2
		}
		return prio, g.VRAM
	}

	best := active[0]
	bestPrio, bestVRAM := score(best)
	for _, g := range active[1:] {
		p, v := score(g)
		if p < bestPrio || (p == bestPrio && v > bestVRAM) {
			best, bestPrio, bestVRAM = g, p, v
		}
	}
	return best
}

// detectGPU returns the preferred GPU (the one the app should consider active).
// It consults the persisted user selection first, then auto-detects.
func detectGPU() gpuInfo {
	all := detectGPUs()
	var real, virtual []gpuInfo
	for _, g := range all {
		if g.Vendor == "Virtual" {
			virtual = append(virtual, g)
		} else {
			real = append(real, g)
		}
	}

	// If the user explicitly selected a GPU, honour it.
	if sel := selectedGPUName(); sel != "" {
		for _, g := range all {
			if equalGPUName(g.Name, sel) {
				return g
			}
		}
	}

	return pickPreferredGPU(real, virtual)
}

func equalGPUName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

const gpuSelectionFile = "gpu_selection.json"

// selectedGPUName reads the user's persisted GPU selection from config.json.
// Falls back to the legacy gpu_selection.json when config is empty (backward
// compatibility with older installs).
func selectedGPUName() string {
	if sel := loadAppConfig().GPUSelection; sel != "" {
		return sel
	}
	// Legacy fallback.
	p := getAssetPath(gpuSelectionFile)
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var sel struct {
		GPU string `json:"gpu"`
	}
	if err := json.Unmarshal(data, &sel); err != nil {
		return ""
	}
	return sel.GPU
}

// saveGPUSelection persists the user's chosen GPU name into config.json,
// preserving the other config fields.
func saveGPUSelection(name string) error {
	cfg := loadAppConfig()
	cfg.GPUSelection = name
	return writeAppConfig(cfg)
}

// writeAppConfig writes the consolidated config.json back to disk, creating the
// asset directory if needed.
func writeAppConfig(cfg appConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	p := getAssetPath(configFile)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
}

// GpuInfo is the serializable GPU model returned to the frontend.
type GpuInfo struct {
	Name                    string `json:"name"`
	SupportsNeuralRendering bool   `json:"supportsNeuralRendering"`
	Vendor                  string `json:"vendor"`
	VRAM                    uint64 `json:"vram"`
	Selected                bool   `json:"selected"`
	Active                  bool   `json:"active"`
	NRTier                  string `json:"nrTier"`
}

func gpuToInfo(g gpuInfo, selected string, activeName string) GpuInfo {
	return GpuInfo{
		Name:                    g.Name,
		SupportsNeuralRendering: g.SupportsNeuralRendering,
		Vendor:                  g.Vendor,
		VRAM:                    g.VRAM,
		Selected:                selected != "" && equalGPUName(g.Name, selected),
		Active:                  equalGPUName(g.Name, activeName),
		NRTier:                  string(g.NRTier),
	}
}

// GetGPUs returns every detected GPU and marks the currently active/selected one.
func (a *App) GetGPUs() []GpuInfo {
	all := detectGPUs()
	selected := selectedGPUName()
	active := detectGPU().Name
	infos := make([]GpuInfo, 0, len(all))
	for _, g := range all {
		infos = append(infos, gpuToInfo(g, selected, active))
	}
	return infos
}

// SelectGPU persists the user's chosen GPU by name and returns the active GPU
// after applying the selection.
func (a *App) SelectGPU(name string) GpuInfo {
	if name != "" {
		_ = saveGPUSelection(name)
		writeLog("SelectGPU: user selected GPU '" + name + "'")
	} else {
		// Clearing selection = re-enable auto detection.
		_ = saveGPUSelection("")
		writeLog("SelectGPU: GPU selection cleared — auto detection re-enabled")
	}
	active := detectGPU()
	return gpuToInfo(active, name, active.Name)
}

// RefreshGPUs re-enumerates the GPU list from scratch (bypassing the cache) and
// returns the freshly detected GPUs, preserving the current selection.
func (a *App) RefreshGPUs() []GpuInfo {
	refreshGPUCache()
	writeLog("RefreshGPUs: GPU cache cleared — re-detecting adapters")
	return a.GetGPUs()
}

var logFile *os.File

// initLogger initializes the log file
func initLogger() {
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	logPath := filepath.Join(exeDir, "patcher.log")

	var err error
	logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Failed to open log file: %v\n", err)
		return
	}

	writeLog("=== DLSS 5 Patcher Started ===")
}

// writeLog writes a message to the log file
func writeLog(message string) {
	if logFile == nil {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logMessage := fmt.Sprintf("[%s] %s\n", timestamp, message)
	logFile.WriteString(logMessage)
	logFile.Sync()
}

// closeLogger closes the log file
func closeLogger() {
	if logFile != nil {
		writeLog("=== DLSS 5 Patcher Closed ===\n")
		logFile.Close()
	}
}

// getAssetPath resolves asset paths relative to executable or working directory
func getAssetPath(subPath string) string {
	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		candidates := []string{
			filepath.Join(exeDir, subPath),
			filepath.Join(exeDir, "..", subPath),
			filepath.Join(exeDir, "..", "..", subPath),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	cwd, err := os.Getwd()
	if err == nil {
		candidates := []string{
			filepath.Join(cwd, subPath),
			filepath.Join(cwd, "..", subPath),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	return subPath
}

const (
	configFile        = "config.json"
	reshadeSetupDir   = "reshade-setup"
	defaultReShadeExe = "ReShade_Setup_6.8.0.exe"
	reshadeAddonExe   = "ReShade_Setup_6.8.0_Addon.exe"
)

// appConfig is the single consolidated configuration file (config.json). All
// settings live here: the active GPU selection plus one URL per downloadable
// data set. URLs are zip archives; if the matching local data is incomplete the
// app downloads it, and an empty URL means "show an error dialog instead".
type appConfig struct {
	GPUSelection    string `json:"gpu_selection"`
	ReShadeSetupURL string `json:"reshade_setup_url"`
	ReShadeURL      string `json:"reshade_url"`
	OptiScalerURL   string `json:"optiscaler_url"`
	DLSS5URL        string `json:"dlss5_url"`
}

// defaultAppConfig returns the default config with dummy download URLs. Only
// reshade_setup_url is left empty because the ReShade setup already ships
// locally (data/reshade-setup); the rest would need real sources.
func defaultAppConfig() appConfig {
	return appConfig{
		GPUSelection:    "",
		ReShadeSetupURL: "https://reshade.me/downloads/ReShade_Setup_6.8.0_Addon.exe",
		ReShadeURL:      "https://example.com/dlss5/reshade.zip",
		OptiScalerURL:   "https://example.com/dlss5/optiscaler.zip",
		DLSS5URL:        "https://example.com/dlss5/dlss5.zip",
	}
}

// loadAppConfig reads config.json from the asset path. If the file is missing
// or invalid it returns the default config (and, when writable, writes it out
// so the user has a starter file). URLs are trimmed of surrounding whitespace.
func loadAppConfig() appConfig {
	cfg := defaultAppConfig()
	p := getAssetPath(configFile)
	data, err := os.ReadFile(p)
	if err != nil {
		writeLog("loadAppConfig: config.json not found, using defaults (" + p + ")")
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		writeLog("loadAppConfig: invalid config.json: " + err.Error())
		return cfg
	}
	cfg.GPUSelection = strings.TrimSpace(cfg.GPUSelection)
	cfg.ReShadeSetupURL = strings.TrimSpace(cfg.ReShadeSetupURL)
	cfg.ReShadeURL = strings.TrimSpace(cfg.ReShadeURL)
	cfg.OptiScalerURL = strings.TrimSpace(cfg.OptiScalerURL)
	cfg.DLSS5URL = strings.TrimSpace(cfg.DLSS5URL)
	return cfg
}

// getReShadeSetup resolves the add-on enabled ReShade setup executable path
// (ReShade_Setup_6.8.0_Addon.exe). A plain ReShade cannot load the DLSS 5
// *.addon64 files ("limited add-on functionality"), so if the Addon build is
// missing it is downloaded from the config URL and saved into
// data/reshade-setup for reuse.
func (a *App) getReShadeSetup() (string, error) {
	setupDir := getAssetPath(filepath.Join("data", reshadeSetupDir))
	if err := a.ensureDataset("reshade-setup"); err != nil {
		return "", err
	}

	addonPath := filepath.Join(setupDir, reshadeAddonExe)
	if _, err := os.Stat(addonPath); err == nil {
		return addonPath, nil
	}

	// Addon build missing → download it from the config URL and save it. The
	// plain ReShade_Setup_6.8.0.exe (if present) is never used for the CLI
	// because it installs a limited add-on build.
	cfg := loadAppConfig()
	url := strings.TrimSpace(cfg.ReShadeSetupURL)
	if url == "" {
		return "", fmt.Errorf("ReShade add-on setup (ReShade_Setup_6.8.0_Addon.exe) not found in %s and no reshade_setup_url is set", setupDir)
	}
	a.emitPatchStatus("Downloading ReShade add-on setup from config...")
	writeLog("getReShadeSetup: downloading missing add-on setup from " + url)
	if err := downloadFile(url, addonPath); err != nil {
		return "", fmt.Errorf("failed to download ReShade add-on setup from %s: %v", url, err)
	}
	if _, err := os.Stat(addonPath); err != nil {
		return "", fmt.Errorf("downloaded ReShade add-on setup not found at %s", addonPath)
	}
	writeLog("getReShadeSetup: add-on setup downloaded to " + addonPath)
	a.ensureDataset("reshade-setup")
	return addonPath, nil
}

// downloadFile downloads url into dest with a simple progress log.
func downloadFile(url, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	writeLog(fmt.Sprintf("downloadFile: downloading %s -> %s", url, dest))
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	writeLog("downloadFile: download complete: " + dest)
	return nil
}

// dataDir returns the asset path for a sub-directory under data/.
func dataDir(sub string) string {
	return getAssetPath(filepath.Join("data", sub))
}

// datasetSpec describes a downloadable data set: its label (for messages), the
// local directory where it lives, its config URL (zip archive, may be empty),
// and the required files (relative paths) that prove it is complete.
type datasetSpec struct {
	label         string
	dir           string
	url           string
	requiredFiles []string
}

// requiredFilesFor returns the required relative files for a named dataset.
// reshade-setup only requires the extracted hook DLL (used by the manual
// fallback path); the setup exe is optional — if absent, the manual path
// already has the DLL. Accepting either plain or add-on build exe is handled
// by getReShadeSetup, not here.
func requiredFilesFor(label string) []string {
	switch label {
	case "reshade-setup":
		return []string{filepath.Join("Extracted", "ReShade64.dll")}
	case "reshade":
		return []string{
			"renodx-dlss5.addon64",
			"dlss5-feed.addon64",
			filepath.Join("reshade-shaders-source", "Shaders", "DLSS5_Feed.fx"),
		}
	case "optiscaler":
		return []string{
			"OptiScaler.dll",
			"OptiScaler.ini.default",
			filepath.Join("OptiScaler", "libxess.dll"),
		}
	case "dlss5":
		return []string{
			"nvngx_dlss.dll",
			"nvngx_dlssnr.dll",
			"nvngx.dll_dlssnr.dll",
		}
	}
	return nil
}

// datasetFromConfig builds a datasetSpec from the consolidated config for the
// requested label. Returns an error for an unknown label.
func datasetFromConfig(cfg appConfig, label string) (datasetSpec, error) {
	spec := datasetSpec{label: label, dir: dataDir(label), requiredFiles: requiredFilesFor(label)}
	if len(spec.requiredFiles) == 0 {
		return spec, fmt.Errorf("unknown data set: %s", label)
	}
	switch label {
	case "reshade-setup":
		spec.url = cfg.ReShadeSetupURL
	case "reshade":
		spec.url = cfg.ReShadeURL
	case "optiscaler":
		spec.url = cfg.OptiScalerURL
	case "dlss5":
		spec.url = cfg.DLSS5URL
	}
	return spec, nil
}

// dataComplete reports whether every required file exists under dir.
func dataComplete(dir string, required []string) bool {
	for _, r := range required {
		if _, err := os.Stat(filepath.Join(dir, r)); err != nil {
			return false
		}
	}
	return true
}

// showConfigError surfaces a missing-data error to the user via a native dialog
// (and the log) so a download-less setup fails with a clear message instead of
// a silent path error.
func (a *App) showConfigError(msg string) {
	writeLog("ConfigDataError: " + msg)
	if a.ctx != nil && !a.diagnostic {
		runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{
			Type:    runtime.ErrorDialog,
			Title:   "Missing Data",
			Message: msg,
		})
	}
}

// ensureDataset verifies that a data set is complete. If it is, nothing
// happens. If it is not, and a config URL is set, the zip is downloaded and
// extracted; an empty URL yields a clear error dialog.
func (a *App) ensureDataset(label string) error {
	cfg := loadAppConfig()
	spec, err := datasetFromConfig(cfg, label)
	if err != nil {
		a.showConfigError(err.Error())
		return err
	}
	if dataComplete(spec.dir, spec.requiredFiles) {
		writeLog(fmt.Sprintf("ensureDataset: '%s' already complete at %s", label, spec.dir))
		return nil
	}

	if spec.url == "" {
		err := fmt.Errorf("Data set '%s' is incomplete and no download URL is set in config.json (field '%s'). Please set a valid URL or restore the missing files in %s.",
			label, configFieldFor(label), spec.dir)
		a.showConfigError(err.Error())
		return err
	}

	// reshade-setup: if the URL points to a direct .exe download (not a zip
	// archive), download the exe itself then extract the hook DLL from it using
	// 7z. Other data sets always use zip archives.
	if label == "reshade-setup" && strings.HasSuffix(strings.ToLower(spec.url), ".exe") {
		writeLog(fmt.Sprintf("ensureDataset: '%s' downloading setup exe directly from %s", label, spec.url))
		if err := os.MkdirAll(spec.dir, 0755); err != nil {
			a.showConfigError(fmt.Sprintf("Failed to create directory for '%s': %v", label, err))
			return err
		}
		exeDest := filepath.Join(spec.dir, filepath.Base(spec.url))
		if err := downloadFile(spec.url, exeDest); err != nil {
			err2 := fmt.Errorf("Failed to download '%s' from %s: %v", label, spec.url, err)
			a.showConfigError(err2.Error())
			return err2
		}
		// Extract ReShade64.dll (and ReShade32.dll) from the downloaded exe.
		extractedDir := filepath.Join(spec.dir, "Extracted")
		for _, dll := range []string{"ReShade64.dll", "ReShade32.dll"} {
			if outErr := extractReshadeDLL(exeDest, extractedDir, dll); outErr != nil {
				writeLog(fmt.Sprintf("ensureDataset: 7z extract %s: %v", dll, outErr))
			}
		}
		// Copy manifest .json files next to the extracted DLLs for completeness.
		for _, j := range []string{"ReShade64.json", "ReShade32.json", "ReShade64_XR.json", "ReShade32_XR.json"} {
			cmd := exec.Command("7z", "x", exeDest, "-o"+extractedDir, j, "-y")
			cmd.CombinedOutput()
		}
	} else {
		writeLog(fmt.Sprintf("ensureDataset: '%s' incomplete at %s — downloading from %s", label, spec.dir, spec.url))
		if err := a.downloadAndExtractZip(spec.url, spec.dir); err != nil {
			err2 := fmt.Errorf("Failed to download data set '%s' from %s: %v", label, spec.url, err)
			a.showConfigError(err2.Error())
			return err2
		}
	}
	if !dataComplete(spec.dir, spec.requiredFiles) {
		err2 := fmt.Errorf("Downloaded archive for '%s' is still missing required files. The zip layout may not match expectations.", label)
		a.showConfigError(err2.Error())
		return err2
	}
	writeLog(fmt.Sprintf("ensureDataset: '%s' completed after download", label))
	return nil
}

// configFieldFor maps a dataset label back to its config.json field name.
func configFieldFor(label string) string {
	switch label {
	case "reshade-setup":
		return "reshade_setup_url"
	case "reshade":
		return "reshade_url"
	case "optiscaler":
		return "optiscaler_url"
	default:
		return "dlss5_url"
	}
}

// ensureAllDatasets validates every downloadable data set is present before a
// patch runs. Called at the start of the install paths.
func (a *App) ensureAllDatasets() error {
	for _, label := range []string{"reshade-setup", "reshade", "optiscaler", "dlss5"} {
		if err := a.ensureDataset(label); err != nil {
			return err
		}
	}
	return nil
}

// downloadAndExtractZip downloads a zip from url and extracts it into destDir.
// The archive is expected to contain the dataset's files at its root.
func (a *App) downloadAndExtractZip(url, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "dlss5-dataset-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)

	if err := downloadFile(url, tmpName); err != nil {
		return err
	}
	return extractZip(tmpName, destDir)
}

// extractZip unpacks a zip archive into destDir, guarding against path
// traversal (zip-slip) and creating directories as needed.
func extractZip(zipPath, destDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		clean := filepath.Clean(filepath.Join(destDir, f.Name))
		if !strings.HasPrefix(clean, filepath.Clean(destDir)+string(os.PathSeparator)) && clean != filepath.Clean(destDir) {
			writeLog(fmt.Sprintf("extractZip: skipping unsafe entry %q (zip-slip)", f.Name))
			continue
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(clean, 0755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			return err
		}
		out.Close()
		rc.Close()
	}
	return nil
}

// Windows Version API procedures for reading DLL file version
var (
	modVersion                  = syscall.NewLazyDLL("version.dll")
	procGetFileVersionInfoSizeW = modVersion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modVersion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modVersion.NewProc("VerQueryValueW")
)

type VS_FIXEDFILEINFO struct {
	Signature        uint32
	StrucVersion     uint32
	FileVersionMS    uint32
	FileVersionLS    uint32
	ProductVersionMS uint32
	ProductVersionLS uint32
	FileFlagsMask    uint32
	FileFlags        uint32
	FileOS           uint32
	FileType         uint32
	FileSubtype      uint32
	FileDateMS       uint32
	FileDateLS       uint32
}

// getDLLFileVersion extracts the file version string (e.g. "310.8.0.0") from a Windows DLL
func getDLLFileVersion(filePath string) string {
	if filePath == "" {
		return ""
	}
	pPath, err := syscall.UTF16PtrFromString(filePath)
	if err != nil {
		return ""
	}

	var dummy uint32
	size, _, _ := procGetFileVersionInfoSizeW.Call(
		uintptr(unsafe.Pointer(pPath)),
		uintptr(unsafe.Pointer(&dummy)),
	)
	if size == 0 {
		return ""
	}

	buf := make([]byte, size)
	ret, _, _ := procGetFileVersionInfoW.Call(
		uintptr(unsafe.Pointer(pPath)),
		0,
		uintptr(size),
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if ret == 0 {
		return ""
	}

	var subBlock *uint16
	subBlock, _ = syscall.UTF16PtrFromString("\\")
	var valuePtr uintptr
	var length uint32

	ret, _, _ = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(subBlock)),
		uintptr(unsafe.Pointer(&valuePtr)),
		uintptr(unsafe.Pointer(&length)),
	)
	if ret == 0 || valuePtr == 0 || length < uint32(unsafe.Sizeof(VS_FIXEDFILEINFO{})) {
		return ""
	}

	info := (*VS_FIXEDFILEINFO)(unsafe.Pointer(valuePtr))
	vMS := info.FileVersionMS
	vLS := info.FileVersionLS
	return fmt.Sprintf("%d.%d.%d.%d", vMS>>16, vMS&0xFFFF, vLS>>16, vLS&0xFFFF)
}

// Process Snapshot for running game detection
type PROCESSENTRY32W struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

// getRunningProcesses returns a set of lowercase executable names currently running on Windows
func getRunningProcesses() map[string]bool {
	procs := make(map[string]bool)
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return procs
	}
	defer syscall.CloseHandle(snapshot)

	var entry PROCESSENTRY32W
	entry.Size = uint32(unsafe.Sizeof(entry))

	if err := syscall.Process32First(snapshot, (*syscall.ProcessEntry32)(unsafe.Pointer(&entry))); err == nil {
		for {
			exeName := syscall.UTF16ToString(entry.ExeFile[:])
			if exeName != "" {
				procs[strings.ToLower(exeName)] = true
			}
			if err := syscall.Process32Next(snapshot, (*syscall.ProcessEntry32)(unsafe.Pointer(&entry))); err != nil {
				break
			}
		}
	}
	return procs
}

// getRunningProcessPIDs returns a map of lower-cased executable name -> PID
// for every running process, so callers can force-terminate a game.
func getRunningProcessPIDs() map[string]uint32 {
	procs := make(map[string]uint32)
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return procs
	}
	defer syscall.CloseHandle(snapshot)

	var entry PROCESSENTRY32W
	entry.Size = uint32(unsafe.Sizeof(entry))

	if err := syscall.Process32First(snapshot, (*syscall.ProcessEntry32)(unsafe.Pointer(&entry))); err == nil {
		for {
			exeName := syscall.UTF16ToString(entry.ExeFile[:])
			if exeName != "" {
				procs[strings.ToLower(exeName)] = entry.ProcessID
			}
			if err := syscall.Process32Next(snapshot, (*syscall.ProcessEntry32)(unsafe.Pointer(&entry))); err != nil {
				break
			}
		}
	}
	return procs
}

// isGameRunning checks if the target game process is currently running
func (a *App) isGameRunning(gamePath string) (bool, string) {
	targetDir, targetExe, launchExe, _, err := a.resolveGameTarget(gamePath)
	if err != nil {
		return false, ""
	}

	runningProcs := getRunningProcesses()

	exesToCheck := []string{
		filepath.Base(targetExe),
		filepath.Base(launchExe),
	}

	if topExes, err := filepath.Glob(filepath.Join(targetDir, "*.exe")); err == nil {
		for _, e := range topExes {
			exesToCheck = append(exesToCheck, filepath.Base(e))
		}
	}

	for _, exe := range exesToCheck {
		nameLower := strings.ToLower(exe)
		if isIgnoredExe(nameLower) {
			continue
		}
		if runningProcs[nameLower] {
			return true, exe
		}
	}

	return false, ""
}

// KillGameProcess force-terminates the running game process(es) for a game.
// Returns the names of the processes that were ended. Used by the front-end to
// offer an "End Task" escape hatch when a patch/uninstall is blocked because the
// game is still running.
func (a *App) KillGameProcess(gamePath string) (string, error) {
	if strings.TrimSpace(gamePath) == "" {
		return "", fmt.Errorf("game path cannot be empty")
	}

	targetDir, targetExe, launchExe, _, err := a.resolveGameTarget(gamePath)
	if err != nil {
		return "", err
	}

	exes := []string{}
	addExe := func(e string) {
		if e == "" {
			return
		}
		name := strings.ToLower(filepath.Base(e))
		if name != "" && !isIgnoredExe(name) {
			exes = append(exes, name)
		}
	}
	addExe(targetExe)
	addExe(launchExe)
	if topExes, err := filepath.Glob(filepath.Join(targetDir, "*.exe")); err == nil {
		for _, e := range topExes {
			addExe(e)
		}
	}

	if len(exes) == 0 {
		return "", fmt.Errorf("no game executable resolved")
	}

	pids := getRunningProcessPIDs()
	killed := []string{}
	for _, exe := range exes {
		if pid, ok := pids[exe]; ok {
			cmd := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", pid), "/F")
			if out, err := cmd.CombinedOutput(); err == nil {
				killed = append(killed, exe)
				writeLog(fmt.Sprintf("KillGameProcess: Terminated %s (PID %d)", exe, pid))
			} else {
				writeLog(fmt.Sprintf("KillGameProcess: Failed to terminate %s (PID %d): %v (%s)", exe, pid, err, strings.TrimSpace(string(out))))
			}
		}
	}

	if len(killed) == 0 {
		return "", fmt.Errorf("no running game process found to end task")
	}
	return strings.Join(killed, ", "), nil
}

// App struct
type App struct {
	ctx              context.Context
	preExistingFiles map[string]bool
	diagnostic       bool
}

// emitEvent no-ops in CLI diagnostic mode where there is no UI context.
func (a *App) emitEvent(name string, data ...interface{}) {
	if a.diagnostic || a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, name, data...)
}

// emitPatchStatus sends a live progress text line during patch/uninstall so
// the frontend progress bar shows what the backend is currently doing.
func (a *App) emitPatchStatus(msg string) {
	writeLog("PatchStatus: " + msg)
	a.emitEvent("patch:status", msg)
}

// GameInfo represents a detected game
type GameInfo struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	ExePath     string `json:"exePath"`
	IsInstalled bool   `json:"isInstalled"`
	DetectedAPI string `json:"detectedAPI"`
}

// DLLDetail represents version and path info for a single Streamline/DLSS DLL
type DLLDetail struct {
	RelPath string `json:"relPath"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// GameDetails represents full status, version details, and DLL lists for UI
type GameDetails struct {
	Name             string      `json:"name"`
	Path             string      `json:"path"`
	Executable       string      `json:"executable"`
	RenderingAPI     string      `json:"renderingAPI"`
	DLSSVersion      string      `json:"dlssVersion"`
	DLSS5Addon       string      `json:"dlss5Addon"`
	ReshadeStatus    string      `json:"reshadeStatus"`
	OptiScalerStatus string      `json:"optiScalerStatus"`
	IsInstalled      bool        `json:"isInstalled"`
	DLLList          []DLLDetail `json:"dllList"`
	GPUName          string      `json:"gpuName"`
	NeuralSupport    bool        `json:"neuralSupport"`
	NeuralNote       string      `json:"neuralNote"`
	// NeuralNoteLevel is "info" for informational guidance (e.g. DX11 → use ReShade)
	// or "warning" for hard limitations (e.g. NR unsupported on this GPU). Defaults
	// to "warning" when empty.
	NeuralNoteLevel string `json:"neuralNoteLevel"`
	// CoverArt is the game's cover art. Either a Steam CDN URL, a local image data
	// URI (base64), or empty when no art can be resolved.
	CoverArt string `json:"coverArt"`
}

// PatchResult represents the result of a patch operation
type PatchResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// Greet returns a greeting for the given name
func (a *App) Greet(name string) string {
	return fmt.Sprintf("Hello %s, It's show time!", name)
}

// isIgnoredFolder checks if a directory belongs to common non-game software, dev tools, or system utilities
func isIgnoredFolder(folderPath string) bool {
	lower := strings.ToLower(filepath.Clean(folderPath))
	ignoredDirs := []string{
		"\\nvm", "\\autohotkey", "\\cheat engine", "\\cpuid", "\\crystaldiskinfo", "\\crystaldiskmark",
		"\\git", "\\python", "\\vscode", "\\microsoft vscode", "\\visual studio", "\\node", "\\docker",
		"\\adobe", "\\google", "\\firefox", "\\mozilla", "\\7-zip", "\\winrar", "\\notepad",
		"\\obs-studio", "\\vlc", "\\audacity", "\\gimp", "\\blender", "\\postman", "\\vanguard",
		"\\easyanticheat", "\\battleye", "\\malwarebytes", "\\kaspersky", "\\avast", "\\microsoft",
		"\\dotnet", "\\windows", "\\sysinternals", "\\powertoys", "\\windowskits", "\\common files",
		"\\windows nt", "\\windows defender", "\\windows powershell", "\\powershell",
	}
	for _, pattern := range ignoredDirs {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// isIgnoredExe checks if an executable name is a launcher, helper, uninstaller, crash reporter, or non-game utility
func isIgnoredExe(filename string) bool {
	name := strings.ToLower(filename)
	ignoredPatterns := []string{
		"uninstall", "unins000", "setup", "updater", "crash", "crashpad",
		"crashreport", "benchmark", "config", "vc_redist", "redist",
		"dxsetup", "ueprereqsetup", "7z", "helper", "browser", "wizard",
		"bootstrapper", "hpatchz", "upload_crash", "createdump", "patcher",
		"dlss 5 patcher", "unitycrashhandler", "eossdk", "author-nvm",
		"autohotkey", "cheatengine", "cheat engine", "cpuz", "diskinfo",
		"diskmark", "node", "npm", "git", "python", "code", "chrome",
		"firefox", "cmd", "powershell", "vanguard", "easyanticheat",
		"trainer", "fling", "plitch", "cheat", "godmode", "plus 26 trainer",
	}
	for _, pattern := range ignoredPatterns {
		if strings.Contains(name, pattern) {
			return true
		}
	}
	return false
}

// isValidGameExe checks if an executable is a valid 64-bit 3D game executable
func isValidGameExe(exePath string) (bool, string) {
	if isIgnoredExe(filepath.Base(exePath)) {
		return false, ""
	}
	// Desktop/utility apps installed under Program Files (RustDesk, browsers, tools,
	// etc.) register install locations in the Windows Uninstall registry. They are
	// not games, so never validate them even though some import a graphics API.
	lowerPath := strings.ToLower(exePath)
	if strings.Contains(lowerPath, `\program files (x86)\`) ||
		strings.Contains(lowerPath, `\program files\`) {
		return false, ""
	}
	peFile, err := pe.Open(exePath)
	if err != nil {
		return false, ""
	}
	defer peFile.Close()

	if peFile.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		return false, ""
	}

	api := peImportedAPI(exePath)
	if api != "" {
		return true, api
	}

	return false, ""
}

// singleLevelExes lists executable files directly inside the folder
func singleLevelExes(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.exe"))
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// hasRepackSideDirs detects a repacked release layout (_crack / _original files
// side folders, which are fingerprints of a repack that runs from the root)
func hasRepackSideDirs(gameDir string) bool {
	for _, name := range []string{"_crack", "_original files", "_original", "_originals", "_cracked", "_pristine", "_extras"} {
		if s, err := os.Stat(filepath.Join(gameDir, name)); err == nil && s.IsDir() {
			return true
		}
	}
	return false
}

// allRootExes collects the root-level executables of a game directory
func (a *App) allRootExes(gameDir string) []string {
	rootExes, err := singleLevelExes(gameDir)
	if err != nil {
		return nil
	}
	sort.Strings(rootExes)
	return rootExes
}

// bestRootGameExe picks the most likely main game executable from the root
// folder. The main game.exe is almost always the LARGEST executable, and (for
// 3D games) the one that imports a rendering API. Handlers/helpers/setups are
// ignored, so the winner is: biggest API-importing exe, else biggest exe.
func (a *App) bestRootGameExe(gameDir string) (string, string) {
	var bestAPI, bestAny string
	var bestAPISize, bestAnySize int64

	exes, _ := singleLevelExes(gameDir)
	for _, exe := range exes {
		base := filepath.Base(exe)
		if isIgnoredExe(base) {
			continue
		}
		info, err := os.Stat(exe)
		if err != nil {
			continue
		}
		if api := a.hasApiImports(exe); api.api != "" && info.Size() > bestAPISize {
			bestAPI = exe
			bestAPISize = info.Size()
		}
		if info.Size() > bestAnySize {
			bestAny = exe
			bestAnySize = info.Size()
		}
	}

	if bestAPI != "" {
		return bestAPI, filepath.Dir(bestAPI)
	}
	if bestAny != "" {
		return bestAny, filepath.Dir(bestAny)
	}
	return "", ""
}

// hasApiImports inspects a PE's import table and reports any recognized
// rendering library (used to tell a playable root launcher from a stub).
func (a *App) hasApiImports(exePath string) struct {
	api   string
	label string
} {
	result := struct {
		api   string
		label string
	}{}

	peFile, err := pe.Open(exePath)
	if err != nil {
		return result
	}
	defer peFile.Close()

	switch api := peImportedAPI(exePath); api {
	case "d3d12":
		result.api, result.label = "d3d12", "DirectX 12"
	case "d3d11":
		result.api, result.label = "d3d11", "DirectX 11"
	case "vulkan":
		result.api, result.label = "vulkan", "Vulkan"
	case "d3d10":
		result.api, result.label = "d3d10", "DirectX 10"
	case "d3d9":
		result.api, result.label = "d3d9", "DirectX 9"
	case "opengl":
		result.api, result.label = "opengl", "OpenGL"
	case "dxgi":
		result.api, result.label = "dxgi", "DXGI"
	}

	return result
}

// isAddonCapableReShade reports whether a ReShade hook DLL ships FULL add-on
// support (*.addon64). The standard ReShade build has only "limited add-on
// functionality": it still scans for *.addon64 but deliberately skips loading
// them (see ReShade.log: "Skipped loading add-on from '%s' because this build
// of ReShade has only limited add-on functionality"). Only the add-on build
// ("*_Addon.exe" / the fused ReShade64.dll) actually loads them. The full
// build contains the "Loading add-on from" loader strings and never contains
// the "limited add-on functionality" message, which is the reliable marker.
func isAddonCapableReShade(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte("Loading add-on from")) && !bytes.Contains(b, []byte("limited add-on functionality"))
}

// getReshadeInfo probes a directory for an add-on-capable ReShade hook DLL and
// returns the matched file name and a report of whether it supports add-ons
func getReshadeInfo(dir string) (string, string, bool) {
	hookCandidates := []string{"dxgi.dll", "d3d12.dll", "d3d11.dll", "d3d9.dll", "d3d10.dll", "opengl32.dll", "dinput8.dll"}
	for _, name := range hookCandidates {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		ver := getDLLFileVersion(p)
		// Only report as an add-on-capable ReShade hook if the DLL actually
		// contains ReShade code AND can load .addon64 add-ons. Games may ship
		// their own dxgi.dll / d3d12.dll (NOT ReShade), and the plain ReShade
		// build lacks the add-on runtime, so neither counts as installed.
		if ver == "" || !isAddonCapableReShade(p) {
			continue
		}
		return p, ver, true
	}
	return "", "", false
}

// cleanDLL applies a two-pass delete for a file that may be held open briefly
// by anti-virus scanners or killed ReShade sessions
func cleanDLL(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	for i := 0; i < 3; i++ {
		if err := os.Remove(path); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return os.Remove(path)
}

// resolveGameTarget finds the primary game executable and the actual target directory
func (a *App) resolveGameTarget(gamePathOrExe string) (targetDir string, targetExe string, launchExe string, detectedAPI string, err error) {
	if strings.TrimSpace(gamePathOrExe) == "" {
		return "", "", "", "", fmt.Errorf("game path cannot be empty")
	}

	cleanPath := filepath.Clean(gamePathOrExe)
	if isIgnoredFolder(cleanPath) {
		return "", "", "", "", fmt.Errorf("path is ignored software directory: %s", cleanPath)
	}

	stat, err := os.Stat(cleanPath)
	if err != nil {
		return "", "", "", "", fmt.Errorf("path does not exist: %s", cleanPath)
	}

	var gameDir string
	if stat.IsDir() {
		gameDir = cleanPath
	} else {
		gameDir = filepath.Dir(cleanPath)
		launchExe = cleanPath
	}

	shippingPatterns := []string{
		filepath.Join(gameDir, "*", "Binaries", "Win64", "*-Win64-Shipping.exe"),
		filepath.Join(gameDir, "Binaries", "Win64", "*-Win64-Shipping.exe"),
		filepath.Join(gameDir, "*", "Binaries", "Win64", "*.exe"),
		filepath.Join(gameDir, "Binaries", "Win64", "*.exe"),
		filepath.Join(gameDir, "_retail_", "*.exe"),
		filepath.Join(gameDir, "games", "*", "*.exe"),
	}

	for _, pattern := range shippingPatterns {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if !isIgnoredExe(filepath.Base(m)) {
				targetExe = m
				targetDir = filepath.Dir(m)
				break
			}
		}
		if targetExe != "" {
			break
		}
	}

	// Some repacked releases ship a launcher at the root (Expedition33_Steam.exe)
	// that simply boots the Unreal shipping binary; the shipping executable is
	// still the actual game. Keep the shipping binary as the primary target.
	// Add-ons, DXGI hooks and DLSS DLLs are spread to every hook directory by
	// InstallDLSS5 / InstallReshade anyway.

	if targetExe == "" && !stat.IsDir() {
		if !isIgnoredExe(filepath.Base(cleanPath)) {
			targetExe = cleanPath
			targetDir = filepath.Dir(cleanPath)
		}
	}

	// Repacked releases keep multiple identical cracked executables in side
	// folders (_crack, _original files, ...). For a normal game folder the root
	// launcher is always the one the player runs, so make it the fallback.
	//
	// The main game executable is (almost) always the LARGEST exe in the root
	// folder, and the one that actually imports a rendering API. Prefer the
	// biggest API-importing exe, then the biggest exe overall.
	if targetExe == "" {
		targetExe, targetDir = a.bestRootGameExe(gameDir)
	}

	if targetExe == "" {
		return "", "", "", "", fmt.Errorf("no valid game executable found in: %s", gameDir)
	}

	if launchExe == "" {
		launchExe, _ = a.bestRootGameExe(gameDir)
		if launchExe == "" {
			launchExe = targetExe
		}
	}

	detectedAPI = a.detectGameAPI(targetExe)
	return targetDir, targetExe, launchExe, detectedAPI, nil
}

// findAllTargetDirs finds all directories within a game path where binaries/DLLs could be located
func (a *App) findAllTargetDirs(gamePath string) []string {
	if strings.TrimSpace(gamePath) == "" {
		return nil
	}

	cleanPath := filepath.Clean(gamePath)
	if isIgnoredFolder(cleanPath) {
		return nil
	}

	stat, err := os.Stat(cleanPath)
	if err != nil {
		return nil
	}

	var rootDir string
	if stat.IsDir() {
		rootDir = cleanPath
	} else {
		rootDir = filepath.Dir(cleanPath)
	}

	dirMap := make(map[string]bool)
	dirMap[rootDir] = true

	// The patcher may have placed files at the game root for a repacked
	// launcher; scan it for trackable markers so uninstall cleans it too.
	rootTracked := []string{"dxgi.dll", "d3d11.dll", "d3d12.dll", "nvngx_dlss.dll", "nvngx_dlssnr.dll", "renodx-dlss5.addon64", "dlss5-feed.addon64", "ReShade.ini", "reshade-shaders"}
	for _, name := range rootTracked {
		if _, err := os.Stat(filepath.Join(rootDir, name)); err == nil {
			dirMap[rootDir] = true
			break
		}
	}

	if targetDir, _, _, _, err := a.resolveGameTarget(rootDir); err == nil && targetDir != "" {
		dirMap[targetDir] = true
	}

	subPatterns := []string{
		filepath.Join(rootDir, "*", "Binaries", "Win64"),
		filepath.Join(rootDir, "Binaries", "Win64"),
		filepath.Join(rootDir, "*", "Binaries", "ThirdParty", "Win64"),
		filepath.Join(rootDir, "Binaries", "ThirdParty", "Win64"),
		filepath.Join(rootDir, "*", "Plugins", "*", "Binaries", "ThirdParty", "Win64"),
		filepath.Join(rootDir, "*", "Plugins", "*", "*", "Binaries", "ThirdParty", "Win64"),
		filepath.Join(rootDir, "Plugins", "*", "Binaries", "ThirdParty", "Win64"),
		filepath.Join(rootDir, "Plugins", "*", "*", "Binaries", "ThirdParty", "Win64"),
		filepath.Join(rootDir, "*", "Engine", "Binaries", "ThirdParty", "NVIDIA", "*", "Win64"),
		filepath.Join(rootDir, "Engine", "Binaries", "ThirdParty", "NVIDIA", "*", "Win64"),
		filepath.Join(rootDir, "_retail_"),
		filepath.Join(rootDir, "games", "*"),
	}

	for _, pattern := range subPatterns {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if s, err := os.Stat(m); err == nil && s.IsDir() {
				lower := strings.ToLower(m)
				if !strings.Contains(lower, "backup") && !strings.Contains(lower, "redist") && !strings.Contains(lower, "xess") && !strings.Contains(lower, "nvaftermath") {
					dirMap[m] = true
				}
			}
		}
	}

	filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			rel, _ := filepath.Rel(rootDir, path)
			depth := len(strings.Split(rel, string(os.PathSeparator)))
			lower := strings.ToLower(info.Name())
			if depth > 8 || strings.HasPrefix(lower, ".") || strings.Contains(lower, "backup") || strings.Contains(lower, "redist") || strings.Contains(lower, "xess") || strings.Contains(lower, "nvaftermath") {
				return filepath.SkipDir
			}
			return nil
		}

		lower := strings.ToLower(info.Name())
		if strings.HasSuffix(lower, ".exe") ||
			lower == "dxgi.dll" ||
			lower == "nvngx_dlss.dll" ||
			lower == "nvngx_dlssnr.dll" ||
			lower == "sl.dlss.dll" ||
			lower == "sl.interposer.dll" ||
			lower == "sl.deepdvc.dll" ||
			lower == "sl.dlss_g.dll" ||
			lower == "sl.pcl.dll" ||
			lower == "sl.reflex.dll" ||
			lower == "reshade.ini" {
			dirMap[filepath.Dir(path)] = true
		}
		return nil
	})

	var result []string
	for d := range dirMap {
		lowerD := strings.ToLower(d)
		if !strings.Contains(lowerD, "backup") && !strings.Contains(lowerD, "redist") && !strings.Contains(lowerD, "xess") && !strings.Contains(lowerD, "nvaftermath") {
			result = append(result, d)
		}
	}
	return result
}

// isSameOrChildDirectory checks if path is the same as or nested under base directory
func isSameOrChildDirectory(path, base string) bool {
	pClean := filepath.Clean(path)
	bClean := filepath.Clean(base)
	rel, err := filepath.Rel(bClean, pClean)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// getSteamLibraries discovers all Steam library paths from libraryfolders.vdf and common locations
func getSteamLibraries() []string {
	var libs []string
	seen := make(map[string]bool)

	steamInstallPaths := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Steam"),
		filepath.Join(os.Getenv("ProgramFiles"), "Steam"),
	}

	for drive := 'C'; drive <= 'Z'; drive++ {
		drivePath := fmt.Sprintf("%c:\\", drive)
		if _, err := os.Stat(drivePath); err == nil {
			steamInstallPaths = append(steamInstallPaths,
				filepath.Join(drivePath, "Steam"),
				filepath.Join(drivePath, "SteamLibrary"),
				filepath.Join(drivePath, "Games", "Steam"),
				filepath.Join(drivePath, "Games", "SteamLibrary"),
			)
		}
	}

	for _, steamPath := range steamInstallPaths {
		vdfPath := filepath.Join(steamPath, "steamapps", "libraryfolders.vdf")
		data, err := os.ReadFile(vdfPath)
		if err != nil {
			continue
		}

		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "\"path\"") {
				parts := strings.Split(line, "\"")
				if len(parts) >= 4 {
					libPath := filepath.Clean(strings.ReplaceAll(parts[3], "\\\\", "\\"))
					commonPath := filepath.Join(libPath, "steamapps", "common")
					if _, err := os.Stat(commonPath); err == nil {
						k := strings.ToLower(commonPath)
						if !seen[k] {
							seen[k] = true
							libs = append(libs, commonPath)
						}
					}
				}
			}
		}
	}

	return libs
}

// steamAppsDirs returns every Steam "steamapps" directory on the system.
// The Steam libraries are discovered via libraryfolders.vdf (getSteamLibraries
// already yields their ...\steamapps\common paths; we map those up to steamapps).
func steamAppsDirs() []string {
	seen := make(map[string]bool)
	var dirs []string
	add := func(d string) {
		d = filepath.Clean(d)
		if d != "" {
			k := strings.ToLower(d)
			if !seen[k] {
				seen[k] = true
				dirs = append(dirs, d)
			}
		}
	}
	for _, common := range getSteamLibraries() {
		// common = ...\steamapps\common  →  steamapps = parent
		add(filepath.Dir(common))
	}
	// Also cover the main install location directly.
	for _, p := range []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Steam"),
		filepath.Join(os.Getenv("ProgramFiles"), "Steam"),
	} {
		add(filepath.Join(p, "steamapps"))
	}
	return dirs
}

// resolveSteamAppInfo finds the Steam app ID for a game folder by matching its name
// against each appmanifest's "installdir". Returns the app ID and the steamapps dir
// it was found in (empty if not a Steam game).
func resolveSteamAppInfo(gameRoot string) (appID, steamapps string) {
	folderName := strings.ToLower(filepath.Base(filepath.Clean(gameRoot)))
	if folderName == "" {
		return "", ""
	}
	for _, appsDir := range steamAppsDirs() {
		manifests, err := filepath.Glob(filepath.Join(appsDir, "appmanifest_*.acf"))
		if err != nil {
			continue
		}
		for _, m := range manifests {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			content := string(data)
			id := ""
			installDir := ""
			// Parse the two fields we need: "appid" and "installdir".
			for _, line := range strings.Split(content, "\n") {
				t := strings.TrimSpace(line)
				if id == "" && strings.HasPrefix(t, "\"appid\"") {
					parts := strings.Split(t, "\"")
					if len(parts) >= 4 {
						id = strings.TrimSpace(parts[3])
					}
				}
				if strings.HasPrefix(t, "\"installdir\"") {
					parts := strings.Split(t, "\"")
					if len(parts) >= 4 {
						installDir = strings.ToLower(strings.TrimSpace(parts[3]))
					}
				}
			}
			if installDir == folderName && id != "" {
				return id, appsDir
			}
		}
	}
	return "", ""
}

// httpImageExists returns true if url responds successfully with an image (uses GET so it
// works on servers that reject HEAD; the body download is short-circuited when possible).
func httpImageExists(url string) bool {
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err == nil {
		resp, err := client.Do(req)
		if err == nil {
			good := resp.StatusCode == http.StatusOK
			ct := resp.Header.Get("Content-Type")
			if good && strings.HasPrefix(ct, "image/") {
				resp.Body.Close()
				return true
			}
			resp.Body.Close()
		}
	}
	// Fall back to a lightweight GET for servers that don't support HEAD.
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	ct := resp.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "image/")
}

// steamHeaderImage queries Steam's appdetails API and returns the game's live header/capsule
// image URL. This is the reliable source for newer titles (e.g. Heartopia) whose art is not
// exposed on the legacy steam/apps/<id>/ CDN paths.
func steamHeaderImage(appID string) string {
	apiURL := "https://store.steampowered.com/api/appdetails?appids=" + appID + "&l=english&cc=us"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}
	raw, ok := payload[appID]
	if !ok {
		return ""
	}
	var app struct {
		Success bool `json:"success"`
		Data    struct {
			LibraryCapsule string `json:"library_capsule"`
			CapsuleImage   string `json:"capsule_image"`
			HeaderImage    string `json:"header_image"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &app); err != nil || !app.Success {
		return ""
	}
	// Prefer a portrait capsule when available, otherwise the wide header.
	if app.Data.LibraryCapsule != "" {
		return app.Data.LibraryCapsule
	}
	if app.Data.CapsuleImage != "" {
		return app.Data.CapsuleImage
	}
	return app.Data.HeaderImage
}

// resolveCoverArt returns cover art for a game: a local image data URI (from the Steam
// librarycache, most reliable/offline) if present, otherwise a working Steam CDN URL.
func resolveCoverArt(gameRoot string) string {
	appID, steamapps := resolveSteamAppInfo(gameRoot)
	if appID == "" {
		return ""
	}

	// 1) Local Steam library cache is the most reliable and works offline.
	candidates := []string{
		filepath.Join(steamapps, "appcache", "librarycache", appID+"_library_600x900.jpg"),
		filepath.Join(steamapps, "appcache", "librarycache", appID+"_header.jpg"),
		filepath.Join(steamapps, "appcache", "librarycache", appID+".jpg"),
	}
	for _, c := range candidates {
		if b, err := os.ReadFile(c); err == nil && len(b) > 0 {
			mime := "image/jpeg"
			if strings.HasSuffix(strings.ToLower(c), ".png") {
				mime = "image/png"
			}
			return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
		}
	}

	// 2) Legacy portrait CDN path (works for most titles); only keep it if it resolves.
	legacy := "https://cdn.cloudflare.steamstatic.com/steam/apps/" + appID + "/library_600x900.jpg"
	if httpImageExists(legacy) {
		return legacy
	}

	// 3) Steam appdetails API for the live art URL (handles newer games like Heartopia).
	if u := steamHeaderImage(appID); u != "" {
		return u
	}

	return legacy
}

func getEpicManifestGames() []string {
	var paths []string
	manifestDir := filepath.Join(os.Getenv("ProgramData"), "Epic", "EpicGamesLauncher", "Data", "Manifests")
	items, err := filepath.Glob(filepath.Join(manifestDir, "*.item"))
	if err != nil {
		return paths
	}

	for _, item := range items {
		data, err := os.ReadFile(item)
		if err != nil {
			continue
		}
		content := string(data)
		idx := strings.Index(content, "\"InstallLocation\"")
		if idx != -1 {
			sub := content[idx:]
			colonIdx := strings.Index(sub, ":")
			if colonIdx != -1 {
				rest := sub[colonIdx+1:]
				startQuote := strings.Index(rest, "\"")
				if startQuote != -1 {
					endQuote := strings.Index(rest[startQuote+1:], "\"")
					if endQuote != -1 {
						loc := rest[startQuote+1 : startQuote+1+endQuote]
						locClean := filepath.Clean(strings.ReplaceAll(loc, "\\\\", "\\"))
						if _, err := os.Stat(locClean); err == nil && !isIgnoredFolder(locClean) {
							paths = append(paths, locClean)
						}
					}
				}
			}
		}
	}
	return paths
}

// getRegistryGames scans Windows Uninstall registry keys for installed game locations (filtering non-game tools)
func getRegistryGames() []string {
	var paths []string
	seen := make(map[string]bool)

	rootKeys := []struct {
		key  registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\GOG.com\Games`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\GOG.com\Games`},
		{registry.LOCAL_MACHINE, `SOFTWARE\Ubisoft\Launcher\Installs`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Ubisoft\Launcher\Installs`},
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	}

	for _, k := range rootKeys {
		key, err := registry.OpenKey(k.key, k.path, registry.READ)
		if err != nil {
			continue
		}
		subkeys, err := key.ReadSubKeyNames(-1)
		key.Close()
		if err != nil {
			continue
		}

		for _, subkeyName := range subkeys {
			subKeyPath := k.path + `\` + subkeyName
			subKey, err := registry.OpenKey(k.key, subKeyPath, registry.READ)
			if err != nil {
				continue
			}

			valNames := []string{"InstallLocation", "InstallDir", "PATH", "Path", "TargetDir"}
			for _, valName := range valNames {
				val, _, err := subKey.GetStringValue(valName)
				if err == nil && strings.TrimSpace(val) != "" {
					clean := filepath.Clean(val)
					if isIgnoredFolder(clean) {
						continue
					}
					if stat, err := os.Stat(clean); err == nil && stat.IsDir() {
						keyLower := strings.ToLower(clean)
						if !seen[keyLower] {
							topExes, _ := filepath.Glob(filepath.Join(clean, "*.exe"))
							subExes, _ := filepath.Glob(filepath.Join(clean, "*", "Binaries", "Win64", "*.exe"))
							allExes := append(topExes, subExes...)

							hasValidGame := false
							for _, exe := range allExes {
								if ok, _ := isValidGameExe(exe); ok {
									hasValidGame = true
									break
								}
							}

							if hasValidGame {
								seen[keyLower] = true
								paths = append(paths, clean)
							}
						}
					}
				}
			}
			subKey.Close()
		}
	}
	return paths
}

// DetectGames scans all drives sequentially for games (Steam, Epic, Registry, Ubisoft, GOG, EA, Xbox, etc.)
func (a *App) DetectGames() []GameInfo {
	writeLog("DetectGames: Starting sequential multi-drive game detection scan")
	var games []GameInfo
	seenPaths := make(map[string]bool)

	addGame := func(name, path, launchExe, detectedAPI string, isInstalled bool) {
		game := GameInfo{
			Name:        name,
			Path:        path,
			ExePath:     launchExe,
			IsInstalled: isInstalled,
			DetectedAPI: detectedAPI,
		}
		games = append(games, game)
		a.emitEvent("scan:game", game)
		writeLog(fmt.Sprintf("DetectGames: Found '%s' at %s (Patched: %v, API: %s)", name, path, isInstalled, detectedAPI))
	}

	updateStatus := func(msg string) {
		a.emitEvent("scan:status", msg)
	}

	updateStatus("Checking Epic Games Launcher manifests...")
	for _, gamePath := range getEpicManifestGames() {
		if isIgnoredFolder(gamePath) {
			continue
		}
		key := strings.ToLower(gamePath)
		if seenPaths[key] {
			continue
		}
		seenPaths[key] = true

		updateStatus(fmt.Sprintf("scanning folder %s...", gamePath))
		_, targetExe, launchExe, detectedAPI, err := a.resolveGameTarget(gamePath)
		if err != nil || targetExe == "" {
			continue
		}
		isInstalled := a.checkDLSS5Installed(gamePath)
		addGame(filepath.Base(gamePath), gamePath, launchExe, detectedAPI, isInstalled)
	}

	updateStatus("Scanning Windows Registry for installed games...")
	for _, gamePath := range getRegistryGames() {
		if isIgnoredFolder(gamePath) {
			continue
		}
		key := strings.ToLower(gamePath)
		if seenPaths[key] {
			continue
		}
		seenPaths[key] = true

		updateStatus(fmt.Sprintf("scanning registry game %s...", gamePath))
		_, targetExe, launchExe, detectedAPI, err := a.resolveGameTarget(gamePath)
		if err != nil || targetExe == "" {
			continue
		}
		isInstalled := a.checkDLSS5Installed(gamePath)
		addGame(filepath.Base(gamePath), gamePath, launchExe, detectedAPI, isInstalled)
	}

	var searchPaths []string
	searchPaths = append(searchPaths, getSteamLibraries()...)

	userProfile := os.Getenv("USERPROFILE")
	if userProfile != "" {
		searchPaths = append(searchPaths,
			filepath.Join(userProfile, "Games"),
			filepath.Join(userProfile, "Saved Games"),
		)
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData != "" {
		searchPaths = append(searchPaths,
			filepath.Join(localAppData, "EpicGames"),
			filepath.Join(localAppData, "GOG.com", "Games"),
		)
	}

	for drive := 'C'; drive <= 'Z'; drive++ {
		drivePath := fmt.Sprintf("%c:\\", drive)
		if _, err := os.Stat(drivePath); err != nil {
			continue
		}
		searchPaths = append(searchPaths,
			filepath.Join(drivePath, "Games"),
			filepath.Join(drivePath, "Game"),
			filepath.Join(drivePath, "Juegos"),
			filepath.Join(drivePath, "Spiele"),
			filepath.Join(drivePath, "Steam", "steamapps", "common"),
			filepath.Join(drivePath, "SteamLibrary", "steamapps", "common"),
			filepath.Join(drivePath, "steamapps", "common"),
			filepath.Join(drivePath, "Games", "Steam", "steamapps", "common"),
			filepath.Join(drivePath, "Games", "SteamLibrary", "steamapps", "common"),
			filepath.Join(drivePath, "Epic Games"),
			filepath.Join(drivePath, "EpicGames"),
			filepath.Join(drivePath, "Games", "Epic Games"),
			filepath.Join(drivePath, "Ubisoft", "Ubisoft Game Launcher", "games"),
			filepath.Join(drivePath, "Ubisoft Game Launcher", "games"),
			filepath.Join(drivePath, "Ubisoft Games"),
			filepath.Join(drivePath, "Games", "Ubisoft"),
			filepath.Join(drivePath, "Games", "Ubisoft Games"),
			filepath.Join(drivePath, "GOG Galaxy", "Games"),
			filepath.Join(drivePath, "GOG Games"),
			filepath.Join(drivePath, "Games", "GOG Games"),
			filepath.Join(drivePath, "Games", "GOG Galaxy", "Games"),
			filepath.Join(drivePath, "EA Games"),
			filepath.Join(drivePath, "Origin Games"),
			filepath.Join(drivePath, "Games", "EA Games"),
			filepath.Join(drivePath, "Games", "Origin Games"),
			filepath.Join(drivePath, "XboxGames"),
			filepath.Join(drivePath, "Games", "XboxGames"),
			filepath.Join(drivePath, "Riot Games"),
		)
	}

	for _, basePath := range searchPaths {
		if isIgnoredFolder(basePath) {
			continue
		}
		if _, err := os.Stat(basePath); os.IsNotExist(err) {
			continue
		}
		entries, err := os.ReadDir(basePath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			gamePath := filepath.Join(basePath, entry.Name())
			if isIgnoredFolder(gamePath) {
				continue
			}
			key := strings.ToLower(gamePath)
			if seenPaths[key] {
				continue
			}
			seenPaths[key] = true

			updateStatus(fmt.Sprintf("scanning folder %s...", gamePath))
			_, targetExe, launchExe, detectedAPI, err := a.resolveGameTarget(gamePath)
			if err != nil || targetExe == "" {
				continue
			}
			isInstalled := a.checkDLSS5Installed(gamePath)
			addGame(entry.Name(), gamePath, launchExe, detectedAPI, isInstalled)
		}
	}

	updateStatus("")
	a.emitEvent("scan:complete", len(games))
	writeLog(fmt.Sprintf("DetectGames: Sequential scan complete. Found %d games across all drives", len(games)))
	return games
}

// checkDLSS5Installed checks if DLSS 5 is completely installed in the game folder
func (a *App) checkDLSS5Installed(gamePath string) bool {
	if strings.TrimSpace(gamePath) == "" {
		return false
	}

	targetDir, _, _, _, err := a.resolveGameTarget(gamePath)
	if err != nil || targetDir == "" {
		return false
	}

	// The patcher ships two add-on files: renodx-dlss5.addon64 (the core
	// DLSS 5 neural-renderer add-on, always deployed) and dlss5-feed.addon64
	// (only for non-native-DLSS games). Games can legitimately ship their own
	// nvngx_dlssnrg.dll / nvngx_dlss.dll (this game ships BOTH in its DLSS
	// plugin dir), so those DLLs alone never indicate a patch; the add-on
	// files are the only patcher-owned marker. renodx-dlss5.addon64 always lives
	// beside (or near) the ReShade hook, so check any candidate dir.

	allDirs := a.findAllTargetDirs(gamePath)
	for _, dir := range allDirs {
		if _, err := os.Stat(filepath.Join(dir, "renodx-dlss5.addon64")); err == nil {
			return true
		}
		// OptiScaler uses dxgi.dll proxy + OptiScaler.ini
		if _, err := os.Stat(filepath.Join(dir, "OptiScaler.ini")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "dxgi.dll")); err == nil {
				return true
			}
		}
	}
	return false
}

// apiFromLibraryNames maps a set of imported library names to a rendering API code.
// Priority matters: d3d11.dll is checked BEFORE dxgi.dll because DirectX 11 games also
// import dxgi.dll (for the swapchain), so a DX11 title would otherwise be mislabeled as the
// ambiguous "dxgi". d3d12.dll is a definitive DirectX 12 signal; dxgi.dll on its own stays
// ambiguous (could be either D3D11 or D3D12).
func apiFromLibraryName(lib string) string {
	l := strings.ToLower(lib)
	switch {
	case strings.Contains(l, "d3d11.dll"):
		return "d3d11"
	case strings.Contains(l, "d3d12.dll"):
		return "d3d12"
	case strings.Contains(l, "vulkan-1.dll") || strings.Contains(l, "vulkan"):
		return "vulkan"
	case strings.Contains(l, "d3d10.dll"):
		return "d3d10"
	case strings.Contains(l, "d3d9.dll"):
		return "d3d9"
	case strings.Contains(l, "opengl32.dll"):
		return "opengl"
	case strings.Contains(l, "dxgi.dll"):
		return "dxgi"
	}
	return ""
}

// apiScore ranks API codes so the most advanced renderer wins when several DLLs
// advertise different APIs (e.g. Control ships both a DX11 and a DX12 renderer DLL).
func apiScore(api string) int {
	switch api {
	case "d3d12":
		return 10
	case "vulkan":
		return 9
	case "d3d11":
		return 3
	case "d3d10":
		return 2
	case "d3d9":
		return 1
	case "dxgi":
		return 0
	case "opengl":
		return -1
	}
	return -2
}

// peImportSectionLocator maps an RVA to a file offset using the section headers.
// Unlike debug/pe.ImportedLibraries (which skips sections whose PointerToRawData is 0
// and so returns empty for many modern game binaries), this walks every section.
func peRVAtoFileOffset(peFile *pe.File, rva uint32) (int64, bool) {
	for _, s := range peFile.Sections {
		if rva >= s.VirtualAddress && rva < s.VirtualAddress+s.Size {
			off := int64(s.Offset) + int64(rva-s.VirtualAddress)
			return off, true
		}
	}
	return 0, false
}

// parsePEImportLibraries reads the PE import table manually and returns all imported
// library names. debug/pe.ImportedLibraries() is unreliable on this toolchain (it returns
// nothing for many stripped/large game executables because it skips sections whose raw
// offset is 0), so we resolve the import directory and DLL name strings ourselves.
func parsePEImportLibraries(exePath string) []string {
	var libs []string

	peFile, err := pe.Open(exePath)
	if err != nil {
		return libs
	}
	defer peFile.Close()

	var idd pe.DataDirectory
	switch oh := peFile.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		if oh.NumberOfRvaAndSizes <= uint32(pe.IMAGE_DIRECTORY_ENTRY_IMPORT) {
			return libs
		}
		idd = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_IMPORT]
	case *pe.OptionalHeader32:
		if oh.NumberOfRvaAndSizes <= uint32(pe.IMAGE_DIRECTORY_ENTRY_IMPORT) {
			return libs
		}
		idd = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_IMPORT]
	default:
		return libs
	}

	if idd.VirtualAddress == 0 {
		return libs
	}

	f, err := os.Open(exePath)
	if err != nil {
		return libs
	}
	defer f.Close()

	// The import directory is an array of IMAGE_IMPORT_DESCRIPTOR (20 bytes each),
	// terminated by an all-zero entry.
	const descriptorSize = 20
	off, ok := peRVAtoFileOffset(peFile, idd.VirtualAddress)
	if !ok {
		return libs
	}

	for i := 0; i < 128; i++ { // cap against runaway descriptors
		descOff := off + int64(i*descriptorSize)
		if _, err := f.Seek(descOff, io.SeekStart); err != nil {
			break
		}
		desc := make([]byte, descriptorSize)
		if _, err := io.ReadFull(f, desc); err != nil {
			break
		}
		nameRVA := littleEndianUint32(desc[12:16]) // IMAGE_IMPORT_DESCRIPTOR.Name field
		if nameRVA == 0 {
			break
		}
		nameFileOff, ok := peRVAtoFileOffset(peFile, nameRVA)
		if !ok {
			// Name RVA did not resolve to a section with file data; skip this entry.
			continue
		}
		if _, err := f.Seek(nameFileOff, io.SeekStart); err != nil {
			continue
		}
		var nameBytes []byte
		buf := make([]byte, 1)
		for {
			if _, err := io.ReadFull(f, buf); err != nil {
				break
			}
			if buf[0] == 0 {
				break
			}
			nameBytes = append(nameBytes, buf[0])
			if len(nameBytes) > 256 {
				break
			}
		}
		if len(nameBytes) > 0 {
			libs = append(libs, string(nameBytes))
		}
	}
	return libs
}

func littleEndianUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// peImportedAPI parses a PE's import table and returns the best recognized rendering API.
func peImportedAPI(exePath string) string {
	best := ""
	bestScore := -2
	for _, lib := range parsePEImportLibraries(exePath) {
		if api := apiFromLibraryName(lib); api != "" {
			if s := apiScore(api); s > bestScore {
				best = api
				bestScore = s
			}
		}
	}
	return best
}

// detectSiblingDLLAPI inspects DLLs in the same directory as the executable. Some games
// (e.g. Remedy's Control) keep their actual renderer in a companion DLL (d3d_rmdwin10_f.dll
// imports d3d12.dll, d3d_rmdwin7_f.dll imports d3d11.dll) instead of the launcher exe, so the
// exe import scan finds nothing. We scan a bounded set of sibling DLLs and keep the most
// advanced renderer found.
func detectSiblingDLLAPI(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best := ""
	bestScore := -2
	scanned := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if !strings.HasSuffix(name, ".dll") {
			continue
		}
		// Focus on likely renderer/engine DLLs, and cap the total scanned so big
		// frameworks folders stay fast.
		if scanned >= 60 {
			break
		}
		scanned++
		p := filepath.Join(dir, e.Name())
		if api := peImportedAPI(p); api != "" {
			if s := apiScore(api); s > bestScore {
				best = api
				bestScore = s
			}
		}
	}
	return best
}

// detectGameAPI detects the rendering API used by a game executable via fast PE headers & 2MB buffer scan
func (a *App) detectGameAPI(exePath string) string {
	writeLog("detectGameAPI: Analyzing executable " + exePath)

	if api := peImportedAPI(exePath); api != "" {
		writeLog("detectGameAPI: Detected API " + api + " via PE import")
		return api
	}

	f, err := os.Open(exePath)
	if err == nil {
		// Scan the WHOLE file (not just the first 2MB). The import name table that
		// lists d3d11.dll / d3d12.dll sits deep inside large shipping binaries
		// (e.g. a 93MB UE game keeps those strings at ~79MB), so a small prefix
		// read misses them.
		fi, _ := f.Stat()
		var content string
		if fi != nil && fi.Size() > 0 {
			buf := make([]byte, fi.Size())
			n, _ := io.ReadFull(f, buf)
			content = strings.ToLower(string(buf[:n]))
		}
		f.Close()
		if strings.Contains(content, "d3d11.dll") {
			return "d3d11"
		}
		if strings.Contains(content, "d3d12.dll") {
			return "d3d12"
		}
		if strings.Contains(content, "vulkan-1.dll") || strings.Contains(content, "vulkan") {
			return "vulkan"
		}
		if strings.Contains(content, "d3d10.dll") {
			return "d3d10"
		}
		if strings.Contains(content, "d3d9.dll") {
			return "d3d9"
		}
		if strings.Contains(content, "opengl32.dll") {
			return "opengl"
		}
		if strings.Contains(content, "dxgi.dll") {
			return "dxgi"
		}
	} else {
		writeLog("detectGameAPI: Failed to open executable: " + err.Error())
	}

	// Fallback: some games load their renderer from a companion DLL in the same folder.
	if api := detectSiblingDLLAPI(filepath.Dir(exePath)); api != "" {
		writeLog("detectGameAPI: Detected API " + api + " via sibling renderer DLL")
		return api
	}

	writeLog("detectGameAPI: Defaulting to DXGI")
	return "dxgi"
}

// GetGameDetails analyzes game files and returns complete component versions, API, and DLL list
func (a *App) GetGameDetails(gamePathOrExe string) GameDetails {
	details := GameDetails{
		DLSSVersion:      "Not Available",
		DLSS5Addon:       "Not Installed",
		ReshadeStatus:    "Not Installed",
		OptiScalerStatus: "Not Installed",
		DLLList:          []DLLDetail{},
	}

	if strings.TrimSpace(gamePathOrExe) == "" {
		return details
	}

	targetDir, targetExe, _, detectedAPI, err := a.resolveGameTarget(gamePathOrExe)
	if err != nil || targetExe == "" {
		return details
	}

	cleanPath := filepath.Clean(gamePathOrExe)
	var rootDir string
	if stat, err := os.Stat(cleanPath); err == nil && stat.IsDir() {
		rootDir = cleanPath
	} else {
		rootDir = filepath.Dir(cleanPath)
	}

	details.Name = filepath.Base(rootDir)
	details.Path = rootDir
	details.CoverArt = resolveCoverArt(rootDir)

	if relExe, err := filepath.Rel(rootDir, targetExe); err == nil {
		details.Executable = relExe
	} else {
		details.Executable = targetExe
	}

	switch detectedAPI {
	case "d3d12":
		details.RenderingAPI = "DirectX 12"
	case "dxgi":
		// dxgi.dll is the swapchain/backbuffer layer used by BOTH DirectX 11 and
		// DirectX 12 games, so labeling it "DirectX 12" is misleading (UE4
		// titles commonly import dxgi+d3d12 yet run on D3D11). Show it neutrally.
		details.RenderingAPI = "DXGI (DirectX 11/12)"
	case "d3d11":
		details.RenderingAPI = "DirectX 11"
	case "d3d10":
		details.RenderingAPI = "DirectX 10"
	case "d3d9":
		details.RenderingAPI = "Direct3D 9"
	case "vulkan":
		details.RenderingAPI = "Vulkan"
	case "opengl":
		details.RenderingAPI = "OpenGL"
	default:
		details.RenderingAPI = "DXGI (DirectX 11/12)"
	}

	// Neural-rendering support & guidance. DLSS NR runs only on RTX 40/50
	// (the model rejects everything older), and OptiScaler's NR pass needs a
	// DirectX 12 pipeline. Surface both so the UI can warn instead of mislead.
	gpu := detectGPU()
	details.GPUName = gpu.Name
	switch gpu.NRTier {
	case NRTierRTX40_50:
		details.NeuralSupport = true
		details.NeuralNote = ""
	case NRTierRTX20_30:
		details.NeuralSupport = false
		details.NeuralNote = "Neural Rendering is not supported on this GPU (RTX 20-30). Only DLSS upscaling will be active."
	default:
		details.NeuralSupport = false
		details.NeuralNote = "No supported NVIDIA GPU detected — DLSS Neural Rendering is unavailable. DLSS upscaling may still work."
	}
	if gpu.NRTier == NRTierRTX40_50 {
		// Neural Rendering is supported; only guide the method choice. OptiScaler
		// supports DirectX 12, DirectX 11 (via the D3D11On12 bridge) and Vulkan, so
		// those can all run the NR pass. Only DX9/10 and OpenGL fall back to ReShade,
		// since OptiScaler has no path for them. Informational (blue), not a warning.
		switch detectedAPI {
		case "d3d9", "d3d10", "opengl":
			details.NeuralNote = "This game runs on " + details.RenderingAPI + ". OptiScaler does not support this API, so use the ReShade method for Neural Rendering here."
			details.NeuralNoteLevel = "info"
		}
	}

	details.IsInstalled = a.checkDLSS5Installed(rootDir)
	// DLSS 5 files are spread across the game tree (ReShade hook dir, DLSS
	// plugin dir, repack root,. .) depending on whetherthe game ships native
	// DLSS. Evaluate presence across every candidate directory so the status does
	// NOT falsely report "Incomplete" for direct/native installs that place
	// nvngx_dlss.dll in a plugin folder rather than the shipping folder.

	allDirs := a.findAllTargetDirs(rootDir)
	checkAddonStatus := func() string {
		hasRenodx := false
		hasFeed := false
		// renodx-dlss5.addon64 is the patcher's always-deployed core add-on;
		// dlss5-feed.addon64 accompanies it only for non-native-DLSS games.

		// Games may ship their own nvngx_dlss.dll / nvngx_dlssnr.dll, so those do
		// NOT prove a patch — only the add-on files do. Still report a partial
		// install if an add-on is present without the neural renderer DLLs shortly
		// after an interrupted patch.
		hasNR := false
		for _, dir := range allDirs {
			if _, err := os.Stat(filepath.Join(dir, "renodx-dlss5.addon64")); err == nil {
				hasRenodx = true
			}
			if _, err := os.Stat(filepath.Join(dir, "dlss5-feed.addon64")); err == nil {
				hasFeed = true
			}
			if _, err := os.Stat(filepath.Join(dir, "nvngx_dlssnr.dll")); err == nil {
				hasNR = true
			}
		}
		switch {
		case hasRenodx:
			if hasNR {
				return "Installed"
			}
			return "Incomplete"
		case hasFeed:
			if hasNR {
				return "Installed"
			}
			return "Incomplete"
		default:
			return "Not Installed"
		}
	}
	details.DLSS5Addon = checkAddonStatus()

	_, reshadeVer, reshadeAddon := getReshadeInfo(targetDir)
	if reshadeVer != "" {
		clean := "Installed"
		switch {
		case reshadeAddon && details.DLSS5Addon == "Installed":
			clean = "Installed + DLSS 5 add-on"
		case details.DLSS5Addon == "Installed":
			clean = "Installed + DLSS 5 add-on"
		case details.DLSS5Addon == "Incomplete":
			clean = "Installed (DLSS 5 add-on incomplete)"
		default:
			// A ReShade build still present after uninstall is purely the game's
			// own shipped one; don't dress it up as this patcher's work.
			if reshadeAddon {
				clean = "Installed (built-in add-on build)"
			} else {
				clean = "Installed (built-in)"
			}
		}
		details.ReshadeStatus = clean + " (" + reshadeVer + ")"
	} else if _, err := os.Stat(filepath.Join(targetDir, "ReShade.ini")); err == nil {
		if details.DLSS5Addon == "Installed" || details.DLSS5Addon == "Incomplete" {
			details.ReshadeStatus = "Installed (+ add-on)"
		} else {
			details.ReshadeStatus = "Installed"
		}
	}

	// Detect OptiScaler installation
	if _, err := os.Stat(filepath.Join(targetDir, "OptiScaler.ini")); err == nil {
		if _, err := os.Stat(filepath.Join(targetDir, "dxgi.dll")); err == nil {
			details.OptiScalerStatus = "Installed"
		} else {
			details.OptiScalerStatus = "Installed (proxy missing)"
		}
	}

	targetDirs := a.findAllTargetDirs(rootDir)
	dlssVer := ""

	knownDLLNames := []string{
		"nvngx_dlss.dll",
		"nvngx_dlssnr.dll",
		"sl.dlss.dll",
		"sl.dlss_g.dll",
		"sl.interposer.dll",
		"sl.deepdvc.dll",
		"sl.pcl.dll",
		"sl.reflex.dll",
		"sl.common.dll",
		"sl.nis.dll",
	}

	seenDLLPaths := make(map[string]bool)

	for _, dir := range targetDirs {
		for _, dllName := range knownDLLNames {
			fullPath := filepath.Join(dir, dllName)
			key := strings.ToLower(fullPath)
			if seenDLLPaths[key] {
				continue
			}
			if _, err := os.Stat(fullPath); err == nil {
				seenDLLPaths[key] = true
				ver := getDLLFileVersion(fullPath)
				relPath, _ := filepath.Rel(rootDir, fullPath)

				details.DLLList = append(details.DLLList, DLLDetail{
					RelPath: relPath,
					Name:    dllName,
					Version: ver,
				})

				if (dllName == "nvngx_dlss.dll" || dllName == "sl.dlss.dll") && ver != "" && dlssVer == "" {
					dlssVer = ver
				}
			}
		}
	}

	if dlssVer != "" {
		details.DLSSVersion = dlssVer
	}

	return details
}

// BrowseExe opens a file dialog to select a game executable
func (a *App) BrowseExe() (string, error) {
	filePath, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select Game Executable",
		Filters: []runtime.FileFilter{
			{DisplayName: "Executable Files (*.exe)", Pattern: "*.exe"},
		},
	})
	return filePath, err
}

// GetGameFolderPreview returns the folder path and detected API for a given exe
func (a *App) GetGameFolderPreview(exePath string) GameInfo {
	if strings.TrimSpace(exePath) == "" {
		return GameInfo{Name: "Unknown", Path: "", ExePath: "", IsInstalled: false, DetectedAPI: "dxgi"}
	}

	targetDir, _, launchExe, detectedAPI, err := a.resolveGameTarget(exePath)
	gameDir := filepath.Dir(exePath)
	if err == nil && targetDir != "" {
		gameDir = filepath.Dir(exePath)
	}

	gameName := filepath.Base(gameDir)
	if launchExe == "" {
		launchExe = exePath
	}

	return GameInfo{
		Name:        gameName,
		Path:        gameDir,
		ExePath:     launchExe,
		IsInstalled: a.checkDLSS5Installed(gameDir),
		DetectedAPI: detectedAPI,
	}
}

// installBackupDLL moves an existing hook DLL into the patcher's backup
// directory and copies a replacement into the game folder (ReShade upgrade path)
func (a *App) installBackupDLL(dir, moduleName string) error {
	existing := filepath.Join(dir, moduleName)
	if _, err := os.Stat(existing); err != nil {
		return nil
	}
	backupDir := filepath.Join(dir, ".dlss5_backup")
	os.MkdirAll(backupDir, 0755)
	dst := filepath.Join(backupDir, moduleName)
	return copyFile(existing, dst)
}

// isDXVKDLL reports whether the given DLL is a DXVK translator (a Direct3D 9 ->
// Vulkan wrapper). DXVK embeds its name in the binary, so a case-insensitive
// full-buffer scan is a reliable fingerprint.
func isDXVKDLL(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	// Scan in chunks to avoid loading very large files fully into memory.
	buf := make([]byte, 1<<20) // 1MB
	const needle = "dxvk"
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := strings.ToLower(string(buf[:n]))
			if strings.Contains(chunk, needle) {
				return true
			}
		}
		if err != nil {
			break
		}
	}
	return false
}

// backupAndRemoveDXVK detects a DXVK d3d9.dll in a game folder and moves it (plus
// its shader cache/log) into the patcher backup so it cannot block a ReShade DX9
// install. DXVK occupies the exact d3d9.dll name ReShade needs for DX9 games, so
// the two cannot coexist. Non-DXVK d3d9.dll files (a game's own runtime, or a real
// ReShade hook) are left untouched.
func (a *App) backupAndRemoveDXVK(dir string) {
	d3d9 := filepath.Join(dir, "d3d9.dll")
	if st, err := os.Stat(d3d9); err != nil || st.IsDir() {
		return
	}
	if !isDXVKDLL(d3d9) {
		writeLog("backupAndRemoveDXVK: d3d9.dll present but not DXVK, leaving it")
		return
	}

	backupDir := filepath.Join(dir, ".dlss5_backup", "dxvk_backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		writeLog("backupAndRemoveDXVK: failed to create backup dir: " + err.Error())
		return
	}
	// Back up and then remove each DXVK artifact. copyFile returns nil for
	// missing sources; here we only move files that exist.
	toRemove := []string{"d3d9.dll", "d3d9.dxvk-cache", "d3d9.dxvk-log", "d3d9.log"}
	for _, name := range toRemove {
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := copyFile(src, filepath.Join(backupDir, name)); err != nil {
			writeLog("backupAndRemoveDXVK: backup failed for " + name + ": " + err.Error())
			continue
		}
		_ = os.Remove(src)
		writeLog("backupAndRemoveDXVK: backed up + removed " + name)
	}
	writeLog("backupAndRemoveDXVK: DXVK hook cleared from " + dir)
}

// gameHasNativeDLSS reports whether the game itself drives DLSS from its ACTIVE
// rendering folder (the resolved shipping dir / launcher root). A stray copy of
// nvngx_dlss.dll inside a third-party plugin folder (e.g.
// Plugins\DLSS\Binaries\ThirdParty\Win64) does NOT mean the game runs native
// DLSS — UE4 repacks often ship such binaries unused, and the game may render
// via D3D11 where RenoDX's D3D12-only NGX hooks never fire. Native-DLSS
// games use the Direct RenoDX add-on path and must NOT get the DLSS5-Feeder (it
// would race the game's own NGX session and crash), whereas non-native games NEED
// the feed to fabricate depth/motion input for the neural renderer.

func (a *App) gameHasNativeDLSS(gamePath string) bool {
	targetDir, _, launchExe, _, err := a.resolveGameTarget(gamePath)
	if err != nil || targetDir == "" {
		return false
	}
	dirs := []string{targetDir}
	if launchExe != "" {
		launchDir := filepath.Dir(launchExe)
		if launchDir != targetDir {
			dirs = append(dirs, launchDir)
		}
	}
	markers := []string{"nvngx_dlss.dll", "_nvngx.dll", "sl.dlss.dll", "nvngx_dlssd.dll", "nvngx_dlssg.dll"}
	for _, dir := range dirs {
		for _, m := range markers {
			if p, err := os.Stat(filepath.Join(dir, m)); err == nil && !p.IsDir() {
				return true
			}
		}
	}
	return false
}

// nativeModeLabel returns a human-readable description of the install mode.
func nativeModeLabel(native bool) string {
	if native {
		return "Direct (no feeder)"
	}
	return "Federated (feed + renodx)"
}

// InstallReshade installs ReShade to the target game folder using command line or manual fallback
func (a *App) InstallReshade(gamePath string) PatchResult {
	writeLog("InstallReshade: Starting ReShade installation for " + gamePath)

	if strings.TrimSpace(gamePath) == "" {
		return PatchResult{Success: false, Message: "Game path cannot be empty"}
	}

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	if isSameOrChildDirectory(gamePath, exeDir) {
		return PatchResult{Success: false, Message: "Cannot patch the patcher directory itself"}
	}

	if running, procName := a.isGameRunning(gamePath); running {
		writeLog(fmt.Sprintf("InstallReshade: BLOCKED - Game process '%s' is currently running", procName))
		return PatchResult{
			Success: false,
			Message: fmt.Sprintf("Game is currently running (%s). Please close the game first and try again.", procName),
		}
	}

	targetDir, targetExe, _, api, err := a.resolveGameTarget(gamePath)
	if err != nil {
		writeLog("InstallReshade: ERROR resolving target: " + err.Error())
		return PatchResult{Success: false, Message: err.Error()}
	}
	native := a.gameHasNativeDLSS(gamePath)

	// A DX9 game running through DXVK (d3d9.dll -> Vulkan) blocks ReShade's own
	// d3d9 hook. If present, back it up and remove it so ReShade can install.
	a.backupAndRemoveDXVK(targetDir)

	a.emitPatchStatus("Preparing ReShade data files (download from config if needed)...")
	// Ensure all required data sets are complete (download from config URLs if
	// needed) before proceeding.
	if err := a.ensureAllDatasets(); err != nil {
		return PatchResult{Success: false, Message: "Missing data. Please check the error dialog."}
	}

	a.emitPatchStatus("Resolving ReShade add-on setup...")
	reshadeSetup, setupErr := a.getReShadeSetup()
	writeLog("InstallReshade: ReShade setup path: " + reshadeSetup)
	writeLog(fmt.Sprintf("InstallReshade: Target EXE: %s in Directory: %s with API: %s", targetExe, targetDir, api))

	// If the game already ships an add-on capable ReShade hook DLL, prefer the
	// working build so the player's existing ReShade (settings, preset, shader
	// setup) is kept. We only need to make sure the preset, config and add-ons
	// are in place.
	if hookFile, _, addonSupport := getReshadeInfo(targetDir); hookFile != "" {
		writeLog("InstallReshade: Existing ReShade hook found at " + hookFile + " (add-on support: " + fmt.Sprintf("%v", addonSupport) + ") - skipping DLL replacement")
		a.emitPatchStatus("Updating ReShade configuration (existing add-on build found)...")
		a.ensureReshadeIni(targetDir, native)
		a.ensureReshadePreset(targetDir, native)
		a.copyEffects(targetDir)

		if !addonSupport {
			// The shipped ReShade has no add-on support; upgrade it to the
			// add-on build so DLSS 5 feed/renodx add-ons can load.
			writeLog("InstallReshade: Existing ReShade lacks add-on support, replacing with add-on build")
			a.emitPatchStatus("Replacing ReShade with add-on enabled build...")
			moduleName := "dxgi.dll"
			switch api {
			case "d3d11":
				moduleName = "d3d11.dll"
			case "d3d9":
				moduleName = "d3d9.dll"
			case "opengl":
				moduleName = "opengl32.dll"
			}
			a.copyReshadeFilesManually(targetDir, targetExe, api, native)
			_ = a.installBackupDLL(targetDir, moduleName)
		}

		writeLog("InstallReshade: ReShade already present with add-on support, configuration updated for " + targetDir)
		return PatchResult{Success: true, Message: "ReShade already installed (add-on build found); configuration updated"}
	}

	if setupErr != nil {
		writeLog("InstallReshade: ERROR resolving ReShade add-on setup: " + setupErr.Error())
		return PatchResult{Success: false, Message: setupErr.Error()}
	}

	if _, err := os.Stat(reshadeSetup); os.IsNotExist(err) {
		writeLog("InstallReshade: ERROR - ReShade add-on setup not found at: " + reshadeSetup)
		return PatchResult{Success: false, Message: "ReShade add-on setup not found at " + reshadeSetup}
	}

	a.emitPatchStatus("Running ReShade add-on installer (headless)...")
	writeLog(fmt.Sprintf("InstallReshade: Executing ReShade setup via CLI... (%s)", api))
	cmd := exec.Command(reshadeSetup,
		targetExe,
		"--headless",
		"--api", api,
		"--preset", gamePath+"ReShadePreset.ini",
		// "--state", "finished",
	)
	cmd.Dir = targetDir
	output, err := cmd.CombinedOutput()
	writeLog(fmt.Sprintf("InstallReshade: CLI Output: %s", string(output)))

	var reshadeModuleName string
	switch api {
	case "d3d9":
		reshadeModuleName = "d3d9.dll"
	case "d3d10":
		reshadeModuleName = "d3d10.dll"
	case "d3d11":
		reshadeModuleName = "d3d11.dll"
	case "d3d12", "dxgi", "vulkan":
		reshadeModuleName = "dxgi.dll"
	case "opengl":
		reshadeModuleName = "opengl32.dll"
	case "ddraw":
		reshadeModuleName = "ddraw.dll"
	default:
		reshadeModuleName = "dxgi.dll"
	}

	hookDLL := filepath.Join(targetDir, reshadeModuleName)
	// Verify the CLI actually left an add-on-capable hook behind. The standard
	// ReShade build only has "limited add-on functionality" and skips .addon64
	// files (so the DLSS 5 add-ons never appear), so mere existence is not
	// enough. If the add-on build is missing, replace it with the extracted
	// ReShade64.dll from the add-on setup.
	if _, statErr := os.Stat(hookDLL); statErr != nil {
		writeLog("InstallReshade: Hook DLL not created by CLI: " + statErr.Error())
		return PatchResult{Success: false, Message: "ReShade installer did not create " + reshadeModuleName + ". Please check the log."}
	}
	if !isAddonCapableReShade(hookDLL) {
		writeLog("InstallReshade: Hook DLL is not add-on capable; replacing with add-on ReShade64.dll")
		a.emitPatchStatus("Replacing ReShade hook with add-on enabled build...")
		a.copyReshadeFilesManually(targetDir, targetExe, api, native)
	}

	a.emitPatchStatus("Writing ReShade configuration & presets...")
	a.ensureReshadeIni(targetDir, native)
	a.ensureReshadePreset(targetDir, native)
	a.emitPatchStatus("Copying ReShade shaders (iMMERSE / DLSS5 feed)...")
	a.copyEffects(targetDir)

	writeLog("InstallReshade: ReShade installation completed for " + targetDir)
	return PatchResult{Success: true, Message: fmt.Sprintf("ReShade installed successfully (API: %s)", api)}
}

// ensureReshadeIni writes a complete ReShade.ini with recursive shader paths
// and forces the RenoDX DLSS5 add-on's upscaling off (NREnableUpscaling=0)
// across every config section the add-on may read. Re-writing the value in the
// overlay is fragile, so we pre-seed all candidate sections.
func (a *App) ensureReshadeIni(targetDir string, native bool) error {
	iniContent := `[ADDON]
EnableHooks=2
NREnableUpscaling=0

[Generic]
EnableHooks=2
NREnableUpscaling=0

[DLSSNR]
EnableHooks=2
NREnableUpscaling=0

[RenoDX.DLSS5]
EnableHooks=2
NREnableUpscaling=0

[renodx-dlss5]
EnableHooks=2
NREnableUpscaling=0

[GENERAL]
AddonPath=.\
CurrentPresetPath=.\ReShadePreset.ini
EffectSearchPaths=.\reshade-shaders\Shaders,.\reshade-shaders\Shaders\**
IntermediateCachePath=C:\Users\hanif\AppData\Local\Temp\ReShade
NoDebugInfo=1
NoEffectCache=0
NoReloadOnInit=0
PerformanceMode=0
PreprocessorDefinitions=DLSS5_MV_PROVIDER=2
PresetFiles=.\ReShadePreset.ini
PresetPath=.\ReShadePreset.ini
PresetShortcutKeys=
PresetShortcutPaths=
PresetTransitionDuration=1000
ScreenshotFormat=0
ScreenshotPath=
ShowClock=0
ShowFPS=0
SkipLoadingDisabledEffects=0
StartupPresetPath=
TextureSearchPaths=.\reshade-shaders\Textures,.\reshade-shaders\Textures\**
TutorialProgress=4

[INPUT]
ForceShortcutModifiers=1
InputProcessing=2
KeyEffects=0,0,0,0
KeyFPS=0,0,0,0
KeyFrametime=0,0,0,0
KeyNextPreset=0,0,0,0
KeyOverlay=36,0,0,0
KeyPreviousPreset=0,0,0,0
KeyReload=0,0,0,0
KeyScreenshot=44,0,0,0

[OVERLAY]
AutoSavePreset=1
ClockFormat=0
FPSPosition=1
Language=
OverlayCollapsed=DLSS 5 Neural Rendering@renodx-dlss5.addon64
ShowClock=0
ShowForceLoadEffectsButton=1
ShowFPS=2
ShowFrameTime=0
ShowPresetName=0
ShowPresetTransitionMessage=1
ShowScreenshotMessage=1
TutorialProgress=4
VariableListHeight=200.000000
VariableListUseTabs=0
`
	iniPath := filepath.Join(targetDir, "ReShade.ini")
	writeLog("ensureReshadeIni: Creating/Updating ReShade.ini at " + iniPath + " (native=" + fmt.Sprintf("%v", native) + ")")
	return os.WriteFile(iniPath, []byte(iniContent), 0644)
}

// ensureReshadePreset creates or updates ReShadePreset.ini with clean default settings
// so iMMERSE Launchpad and DLSS 5 Feed are pre-configured in order at the top
func (a *App) ensureReshadePreset(targetDir string, native bool) error {
	presetPath := filepath.Join(targetDir, "ReShadePreset.ini")
	writeLog(fmt.Sprintf("ensureReshadePreset: Configuring ReShadePreset.ini at %s (native DLSS: %v)", presetPath, native))

	techniques := "Techniques=MartysMods_Launchpad@MartysMods_LAUNCHPAD.fx,DLSS5_Feed@DLSS5_Feed.fx\nTechniqueSorting=MartysMods_Launchpad@MartysMods_LAUNCHPAD.fx,DLSS5_Feed@DLSS5_Feed.fx"
	iniContent := techniques + `
PreprocessorDefinitions=DLSS5_MV_PROVIDER=2

[GENERAL]
` + techniques + `
PreprocessorDefinitions=DLSS5_MV_PROVIDER=2
`

	return os.WriteFile(presetPath, []byte(iniContent), 0644)
}

// copyReshadeFilesManually copies ReShade files manually
// is32BitExe reports whether the given PE executable is a 32-bit (x86) build.
// ReShade ships separate 32- and 64-bit hooks and a 32-bit game cannot load a
// 64-bit DLL, so the correct hook must be chosen based on the game's bitness.
func is32BitExe(exePath string) bool {
	if exePath == "" {
		return false
	}
	f, err := pe.Open(exePath)
	if err != nil {
		writeLog("is32BitExe: cannot open " + exePath + ": " + err.Error())
		return false
	}
	defer f.Close()
	return f.Machine == pe.IMAGE_FILE_MACHINE_I386
}

func (a *App) copyReshadeFilesManually(targetDir, targetExe, api string, native bool) PatchResult {
	writeLog("copyReshadeFilesManually: Starting manual ReShade copy with API: " + api)

	var reshadeModuleName string
	switch api {
	case "d3d9":
		reshadeModuleName = "d3d9.dll"
	case "d3d10":
		reshadeModuleName = "d3d10.dll"
	case "d3d11":
		reshadeModuleName = "d3d11.dll"
	case "d3d12", "dxgi", "vulkan":
		reshadeModuleName = "dxgi.dll"
	case "opengl":
		reshadeModuleName = "opengl32.dll"
	case "ddraw":
		reshadeModuleName = "ddraw.dll"
	default:
		reshadeModuleName = "dxgi.dll"
	}

	// Pick the 32- or 64-bit ReShade hook to match the game's architecture. A
	// 64-bit hook silently fails to load in a 32-bit game (and vice versa).
	dllName := "ReShade64.dll"
	if is32BitExe(targetExe) {
		dllName = "ReShade32.dll"
	}
	writeLog("copyReshadeFilesManually: Bitness of " + targetExe + ": " + dllName)

	extractedDLL := getAssetPath(filepath.Join("data", reshadeSetupDir, "Extracted", dllName))
	if _, err := os.Stat(extractedDLL); err != nil {
		reshadeSetup, _ := a.getReShadeSetup()
		if _, statErr := os.Stat(reshadeSetup); statErr == nil {
			extractReshadeDLL(reshadeSetup, filepath.Dir(extractedDLL), dllName)
		}
	}

	if _, err := os.Stat(extractedDLL); err == nil {
		dstDLL := filepath.Join(targetDir, reshadeModuleName)
		writeLog(fmt.Sprintf("copyReshadeFilesManually: Copying %s to %s", extractedDLL, dstDLL))
		if err := copyFile(extractedDLL, dstDLL); err != nil {
			writeLog("copyReshadeFilesManually: ERROR copying DLL: " + err.Error())
			return PatchResult{Success: false, Message: "Failed to copy ReShade DLL: " + err.Error()}
		}
	}

	a.ensureReshadeIni(targetDir, native)
	a.ensureReshadePreset(targetDir, native)
	a.copyEffects(targetDir)

	return PatchResult{Success: true, Message: fmt.Sprintf("ReShade files copied manually (API: %s, Module: %s)", api, reshadeModuleName)}
}

// extractReshadeDLL extracts a ReShade DLL (ReShade32/64.dll) from the ReShade
// setup executable
func extractReshadeDLL(setupPath, outputDir, dllName string) error {
	cmd := exec.Command("7z", "x", setupPath, "-o"+outputDir, dllName, "-y")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("7z extraction failed: %v, output: %s", err, string(output))
	}
	return nil
}

// copyEffects copies shaders and textures to the target directory
func (a *App) copyEffects(targetDir string) error {
	writeLog("copyEffects: Copying shaders to " + targetDir)

	srcShaders := getAssetPath(filepath.Join("data", "reshade", "reshade-shaders-source"))
	if _, err := os.Stat(srcShaders); err != nil {
		srcShaders = getAssetPath(filepath.Join("data", reshadeSetupDir, "Effects", "reshade-shaders"))
	}

	dstShaders := filepath.Join(targetDir, "reshade-shaders")
	writeLog(fmt.Sprintf("copyEffects: From %s to %s", srcShaders, dstShaders))

	err := copyDir(srcShaders, dstShaders)
	if err != nil {
		writeLog("copyEffects: ERROR - " + err.Error())
	} else {
		writeLog("copyEffects: Shaders copied successfully")
	}
	return err
}

// InstallDLSS5 copies DLSS 5 files to the target game directory and Streamline/ThirdParty plugin subdirectories
func (a *App) InstallDLSS5(gamePath string) PatchResult {
	writeLog("InstallDLSS5: Starting DLSS 5 installation for " + gamePath)

	if strings.TrimSpace(gamePath) == "" {
		return PatchResult{Success: false, Message: "Game path cannot be empty"}
	}

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	if isSameOrChildDirectory(gamePath, exeDir) {
		return PatchResult{Success: false, Message: "Cannot patch the patcher directory itself"}
	}

	if running, procName := a.isGameRunning(gamePath); running {
		writeLog(fmt.Sprintf("InstallDLSS5: BLOCKED - Game process '%s' is currently running", procName))
		return PatchResult{
			Success: false,
			Message: fmt.Sprintf("Game is currently running (%s). Please close the game first and try again.", procName),
		}
	}

	targetDir, _, _, _, err := a.resolveGameTarget(gamePath)
	if err != nil {
		return PatchResult{Success: false, Message: err.Error()}
	}

	// Ensure reshade + dlss5 data sets are complete before deploying files.
	a.emitPatchStatus("Preparing DLSS 5 data files...")
	for _, label := range []string{"reshade", "dlss5"} {
		if err := a.ensureDataset(label); err != nil {
			return PatchResult{Success: false, Message: "Missing data. Please check the error dialog."}
		}
	}

	srcPath := getAssetPath(filepath.Join("data", "reshade"))
	dlss5Path := getAssetPath(filepath.Join("data", "dlss5"))
	writeLog("InstallDLSS5: Reshade source: " + srcPath + " | DLSS5 shared: " + dlss5Path + " -> Target: " + targetDir)

	// Whether the game is native-DLSS decides the add-on set: federated
	// (feed + renodx) for games without DLSS, Direct (renodx only) for games
	// that ship nvngx_dlss.dll so we do not race their NGX session.
	native := a.gameHasNativeDLSS(gamePath)
	writeLog(fmt.Sprintf("InstallDLSS5: native DLSS runtime in active dir: %v (mode: %s)", native, nativeModeLabel(native)))
	if native {
		a.emitPatchStatus("Native DLSS detected -> installing RenoDX add-on only (no feeder)...")
	} else {
		a.emitPatchStatus("No native DLSS -> installing RenoDX + DLSS5-Feed add-ons...")
	}

	// Detect GPU to decide whether neural rendering (nvngx_dlssnr.dll) is
	// supported.  Only RTX 50 (Blackwell) series GPUs can run DLSSNR; on
	// older cards the NGX runtime rejects the feature with 0xBAD00001.
	// The neural renderer DLL is ALWAYS deployed regardless — on non-RTX 50
	// cards it is simply ignored, but it must be present for neural
	// rendering to ever run (and when GPU detection is inconclusive).
	gpu := detectGPU()
	if !gpu.SupportsNeuralRendering {
		writeLog(fmt.Sprintf("InstallDLSS5: GPU '%s' — Neural Rendering may not be supported, but nvngx_dlssnr.dll will still be deployed", gpu.Name))
	}

	// The DLSS 5 feed add-on must accompany the RenoDX add-on so the neural
	// renderer's guides are produced by ReShade for 64-bit D3D12 games too.
	filesToShipping := []string{
		"renodx-dlss5.addon64",
		"nvngx_dlss.dll",
		"nvngx_dlssnr.dll",
	}
	if !native {
		filesToShipping = append(filesToShipping, "dlss5-feed.addon64")
	}
	if native {
		// Direct mode leaves the game's own upscaler binary in place so NGX
		// hook targets line up; only spread renodx + the neural renderer DLL.
		filesToShipping = []string{"renodx-dlss5.addon64", "nvngx_dlssnr.dll"}
	}

	a.emitPatchStatus("Copying DLSS 5 add-on files to game folder...")
	for _, file := range filesToShipping {
		// Resolve source: check reshade-specific dir first, then shared dlss5 dir
		src := filepath.Join(srcPath, file)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			src = filepath.Join(dlss5Path, file)
		}
		dst := filepath.Join(targetDir, file)

		writeLog(fmt.Sprintf("InstallDLSS5: Copying %s to shipping dir %s", src, dst))

		if _, err := os.Stat(src); os.IsNotExist(err) {
			writeLog("InstallDLSS5: ERROR - Source file not found: " + file)
			return PatchResult{Success: false, Message: "Source file not found: " + file}
		}

		if err := copyFile(src, dst); err != nil {
			writeLog(fmt.Sprintf("InstallDLSS5: ERROR - Failed to copy %s: %v", file, err))
			if strings.Contains(err.Error(), "used by another process") || strings.Contains(err.Error(), "Access is denied") {
				return PatchResult{Success: false, Message: fmt.Sprintf("Failed to copy %s: File is locked by a running process. Please close the game and try again.", file)}
			}
			return PatchResult{Success: false, Message: "Failed to copy " + file + ": " + err.Error()}
		}
	}

	// Add-ons and the feed only load where ReShade's hook DLL lives. The
	// resolved target may be a repack root whose ReShade hook actually sits in
	// a subdirectory (e.g. Binaries\Win64\dxgi.dll), so spread add-ons to every
	// directory that ships a ReShade hook (or has a playable game exe) too.
	allTargetDirs := a.findAllTargetDirs(gamePath)
	allHookDirs := a.findAllReShadeHookDirs(gamePath)
	addonDirs := allTargetDirs
	for _, hookDir := range allHookDirs {
		addonDirs = append(addonDirs, hookDir)
	}
	addonDirs = a.uniqueDirs(addonDirs)

	addonFiles := []string{"dlss5-feed.addon64", "renodx-dlss5.addon64", "nvngx_dlss.dll", "nvngx_dlssnr.dll"}
	if native {
		// Direct mode: no Feeder, keep the game's existing upscaler runtime.
		addonFiles = []string{"renodx-dlss5.addon64", "nvngx_dlssnr.dll"}
	}
	a.emitPatchStatus("Spreading DLSS 5 add-ons to hook directories...")
	for _, dir := range addonDirs {
		if isSameOrChildDirectory(dir, targetDir) {
			continue
		}
		lowerDir := strings.ToLower(dir)
		if strings.Contains(lowerDir, "backup") || strings.Contains(lowerDir, "redist") || strings.Contains(lowerDir, "xess") || strings.Contains(lowerDir, "nvaftermath") {
			continue
		}

		hasDLSSOrStreamline := false
		checkFiles := []string{"nvngx_dlss.dll", "sl.dlss.dll", "sl.interposer.dll", "sl.dlss_g.dll", "sl.deepdvc.dll", "dxgi.dll", "renodx-dlss5.addon64", "dlss5-feed.addon64"}
		for _, cf := range checkFiles {
			if _, err := os.Stat(filepath.Join(dir, cf)); err == nil {
				hasDLSSOrStreamline = true
				break
			}
		}

		for _, file := range addonFiles {
			if file == "renodx-dlss5.addon64" || file == "dlss5-feed.addon64" {
				// Add-ons belong to every directory that ReShade hooks.
				src := filepath.Join(srcPath, file)
				dst := filepath.Join(dir, file)
				writeLog(fmt.Sprintf("InstallDLSS5: Copying %s to target dir %s", src, dst))
				if _, err := os.Stat(src); err == nil {
					_ = copyFile(src, dst)
				}
				continue
			}
			// nvngx_dlssnr.dll / nvngx_dlss.dll only go where a DLSS runtime,
			// Streamline runtime or existing renodx/feed lives.
			if !hasDLSSOrStreamline {
				continue
			}
			// Resolve from reshade dir first, then shared dlss5 dir
			src := filepath.Join(srcPath, file)
			if _, err := os.Stat(src); os.IsNotExist(err) {
				src = filepath.Join(dlss5Path, file)
			}
			dst := filepath.Join(dir, file)
			writeLog(fmt.Sprintf("InstallDLSS5: Copying %s to target dir %s", src, dst))
			if _, err := os.Stat(src); err == nil {
				_ = copyFile(src, dst)
			}
		}
	}

	a.emitPatchStatus("DLSS 5 add-ons installed.")
	writeLog("InstallDLSS5: All DLSS 5 files installed successfully across target directories")
	return PatchResult{Success: true, Message: "DLSS 5 files installed successfully"}
}

// findAllReShadeHookDirs returns every directory under the game that contains
// a ReShade hook DLL (dxgi.dll / d3d11.dll / d3d9.dll / d3d12.dll / opengl32.dll)
func (a *App) findAllReShadeHookDirs(gamePath string) []string {
	var dirs []string
	allDirs := a.findAllTargetDirs(gamePath)
	for _, dir := range allDirs {
		for _, name := range []string{"dxgi.dll", "d3d11.dll", "d3d9.dll", "d3d12.dll", "opengl32.dll"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				dirs = append(dirs, dir)
				break
			}
		}
	}
	return dirs
}

// uniqueDirs returns a deduplicated list of directories
func (a *App) uniqueDirs(dirs []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, d := range dirs {
		key := strings.ToLower(filepath.Clean(d))
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, d)
	}
	return result
}

// InstallOptiScaler installs OptiScaler as a dxgi.dll proxy with auto GPU detection
func (a *App) InstallOptiScaler(gamePath string) PatchResult {
	writeLog("InstallOptiScaler: Starting OptiScaler installation for " + gamePath)

	if strings.TrimSpace(gamePath) == "" {
		return PatchResult{Success: false, Message: "Game path cannot be empty"}
	}

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	if isSameOrChildDirectory(gamePath, exeDir) {
		return PatchResult{Success: false, Message: "Cannot patch the patcher directory itself"}
	}

	if running, procName := a.isGameRunning(gamePath); running {
		writeLog(fmt.Sprintf("InstallOptiScaler: BLOCKED - Game process '%s' is currently running", procName))
		return PatchResult{
			Success: false,
			Message: fmt.Sprintf("Game is currently running (%s). Please close the game first and try again.", procName),
		}
	}

	targetDir, _, _, detectedAPI, err := a.resolveGameTarget(gamePath)
	if err != nil {
		return PatchResult{Success: false, Message: err.Error()}
	}

	// Ensure optiscaler + dlss5 data sets are complete before deploying.
	for _, label := range []string{"optiscaler", "dlss5"} {
		if err := a.ensureDataset(label); err != nil {
			return PatchResult{Success: false, Message: "Missing data. Please check the error dialog."}
		}
	}

	srcPath := getAssetPath(filepath.Join("data", "optiscaler"))
	dlss5Path := getAssetPath(filepath.Join("data", "dlss5"))
	writeLog("InstallOptiScaler: OptiScaler source: " + srcPath + " | DLSS5 shared: " + dlss5Path + " -> Target: " + targetDir)

	// Detect GPU for automatic configuration
	gpu := detectGPU()
	isNvidia := strings.Contains(strings.ToLower(gpu.Name), "nvidia")
	supportsNR := gpu.SupportsNeuralRendering
	writeLog(fmt.Sprintf("InstallOptiScaler: GPU='%s' nvidia=%v supportsNR=%v tier=%q", gpu.Name, isNvidia, supportsNR, gpu.NRTier))

	// Step 1: Create OptiScaler subdirectory in target
	optiSubDir := filepath.Join(targetDir, "OptiScaler")
	if err := os.MkdirAll(optiSubDir, 0755); err != nil {
		writeLog("InstallOptiScaler: ERROR - Failed to create OptiScaler dir: " + err.Error())
		return PatchResult{Success: false, Message: "Failed to create OptiScaler directory: " + err.Error()}
	}
	os.MkdirAll(filepath.Join(optiSubDir, "D3D12_OptiScaler"), 0755)

	// Step 2: Copy backend DLLs to OptiScaler subdirectory
	backendFiles := []string{
		"nvngx_dlss.dll",
		"nvngx_dlssd.dll",
		"nvngx_dlssg.dll",
		"nvngx_deepdvc.dll",
		"dlss-enabler-headless.dll",
		"amd_fidelityfx_framegeneration_dx12.dll",
		"amd_fidelityfx_upscaler_dx12.dll",
		"amd_fidelityfx_loader_dx12.dll",
		"amd_fidelityfx_vk.dll",
		"libxess.dll",
		"libxess_dx11.dll",
		"libxess_fg.dll",
		"libxell.dll",
	}
	for _, file := range backendFiles {
		src := filepath.Join(srcPath, "OptiScaler", file)
		dst := filepath.Join(optiSubDir, file)
		if _, err := os.Stat(src); err == nil {
			if err := copyFile(src, dst); err != nil {
				writeLog(fmt.Sprintf("InstallOptiScaler: WARNING - Failed to copy %s: %v", file, err))
			} else {
				writeLog(fmt.Sprintf("InstallOptiScaler: Copied %s to %s", file, optiSubDir))
			}
		}
	}

	// Copy D3D12Core.dll
	d3d12Src := filepath.Join(srcPath, "OptiScaler", "D3D12_OptiScaler", "D3D12Core.dll")
	d3d12Dst := filepath.Join(optiSubDir, "D3D12_OptiScaler", "D3D12Core.dll")
	if _, err := os.Stat(d3d12Src); err == nil {
		_ = copyFile(d3d12Src, d3d12Dst)
	}

	// Step 3: Copy and configure OptiScaler.ini
	iniSrc := filepath.Join(srcPath, "OptiScaler.ini.default")
	iniDst := filepath.Join(targetDir, "OptiScaler.ini")
	if _, err := os.Stat(iniSrc); err == nil {
		if err := copyFile(iniSrc, iniDst); err != nil {
			writeLog("InstallOptiScaler: ERROR - Failed to copy OptiScaler.ini: " + err.Error())
			return PatchResult{Success: false, Message: "Failed to copy OptiScaler.ini: " + err.Error()}
		}
		writeLog("InstallOptiScaler: Copied OptiScaler.ini")

		// Configure INI based on GPU and detected API
		configureOptiScalerINI(iniDst, isNvidia, supportsNR, detectedAPI)
	}

	// Step 4: Deploy the Neural Rendering shim + model to the game ROOT dir
	// (the folder holding dxgi.dll / the executable), exactly where OptiScaler
	// DLSS-NR (build 433cc11 +) expects them: nvngx.dll_dlssnr.dll (shim) and
	// nvngx_dlssnr.dll (model). The shim depends on the GPU tier:
	//   - RTX 20-30: the full 158MB model shim (nvngx.dll_dlssnr_rtx20.dll)
	//   - RTX 40-50 / modern RTX: the small 108KB forwarder shim (nvngx.dll_dlssnr.dll)
	//   - non-NVIDIA / no NR: the 40-50 forwarder is still deployed but DlssNr is
	//     disabled in the INI so nothing tries to run the pass.
	nrShimSrc := filepath.Join(dlss5Path, "nvngx.dll_dlssnr.dll")
	if gpu.NRTier == NRTierRTX20_30 {
		alt := filepath.Join(dlss5Path, "nvngx.dll_dlssnr_rtx20.dll")
		if _, err := os.Stat(alt); err == nil {
			nrShimSrc = alt
		}
	}
	// Shim to game root as nvngx.dll_dlssnr.dll.
	nrShimDst := filepath.Join(targetDir, "nvngx.dll_dlssnr.dll")
	if _, err := os.Stat(nrShimSrc); err == nil {
		_ = copyFile(nrShimSrc, nrShimDst)
		writeLog(fmt.Sprintf("InstallOptiScaler: Copied NR shim %s as nvngx.dll_dlssnr.dll (tier %q)", filepath.Base(nrShimSrc), gpu.NRTier))
	}

	// Model to game root as nvngx_dlssnr.dll. For the 40-50 tier the model is a
	// separate 158MB file that the small shim loads; for the 20-30 tier the shim
	// already IS the model, so we simply keep the same 158MB file in both roles.
	nrSrc := filepath.Join(dlss5Path, "nvngx_dlssnr.dll")
	nrDst := filepath.Join(targetDir, "nvngx_dlssnr.dll")
	if _, err := os.Stat(nrSrc); err == nil {
		if err := copyFile(nrSrc, nrDst); err != nil {
			writeLog("InstallOptiScaler: WARNING - Failed to copy NR model as nvngx_dlssnr.dll: " + err.Error())
		} else {
			writeLog("InstallOptiScaler: Copied NR model as nvngx_dlssnr.dll")
		}
	} else {
		writeLog("InstallOptiScaler: WARNING - nvngx_dlssnr.dll not found in " + dlss5Path)
	}

	// Step 6: Rename OptiScaler.dll to dxgi.dll (proxy DLL)
	optiSrc := filepath.Join(srcPath, "OptiScaler.dll")
	optiDst := filepath.Join(targetDir, "dxgi.dll")
	if _, err := os.Stat(optiSrc); err != nil {
		return PatchResult{Success: false, Message: "OptiScaler.dll not found in data directory"}
	}
	if err := copyFile(optiSrc, optiDst); err != nil {
		writeLog("InstallOptiScaler: ERROR - Failed to copy OptiScaler.dll as dxgi.dll: " + err.Error())
		return PatchResult{Success: false, Message: "Failed to install OptiScaler proxy: " + err.Error()}
	}
	writeLog("InstallOptiScaler: Installed OptiScaler.dll as dxgi.dll (proxy)")

	// Step 7: Spread OptiScaler to additional hook directories
	allTargetDirs := a.findAllTargetDirs(gamePath)
	allHookDirs := a.findAllReShadeHookDirs(gamePath)
	addonDirs := allTargetDirs
	for _, hookDir := range allHookDirs {
		addonDirs = append(addonDirs, hookDir)
	}
	addonDirs = a.uniqueDirs(addonDirs)

	for _, dir := range addonDirs {
		if isSameOrChildDirectory(dir, targetDir) {
			continue
		}
		lowerDir := strings.ToLower(dir)
		if strings.Contains(lowerDir, "backup") || strings.Contains(lowerDir, "redist") {
			continue
		}

		hasHook := false
		for _, cf := range []string{"dxgi.dll", "d3d11.dll", "d3d12.dll", "nvngx_dlss.dll"} {
			if _, err := os.Stat(filepath.Join(dir, cf)); err == nil {
				hasHook = true
				break
			}
		}
		if !hasHook {
			continue
		}

		// Copy OptiScaler.ini and proxy dll to additional dirs
		_ = copyFile(iniDst, filepath.Join(dir, "OptiScaler.ini"))
		_ = copyFile(optiDst, filepath.Join(dir, "dxgi.dll"))
		// NR shim + model to the hook dir root (same layout as the main target).
		_ = copyFile(nrShimDst, filepath.Join(dir, "nvngx.dll_dlssnr.dll"))
		_ = copyFile(nrDst, filepath.Join(dir, "nvngx_dlssnr.dll"))

		// Create OptiScaler subdirectory and copy backends
		dirOpti := filepath.Join(dir, "OptiScaler")
		os.MkdirAll(dirOpti, 0755)
		os.MkdirAll(filepath.Join(dirOpti, "D3D12_OptiScaler"), 0755)
		for _, file := range backendFiles {
			src := filepath.Join(optiSubDir, file)
			dst := filepath.Join(dirOpti, file)
			if _, err := os.Stat(src); err == nil {
				_ = copyFile(src, dst)
			}
		}
		_ = copyFile(d3d12Dst, filepath.Join(dirOpti, "D3D12_OptiScaler", "D3D12Core.dll"))
	}

	writeLog("InstallOptiScaler: All OptiScaler files installed successfully")
	return PatchResult{Success: true, Message: "OptiScaler installed successfully"}
}

// configureOptiScalerINI modifies OptiScaler.ini based on detected GPU
func configureOptiScalerINI(iniPath string, isNvidia bool, supportsNR bool, detectedAPI string) {
	data, err := os.ReadFile(iniPath)
	if err != nil {
		writeLog("configureOptiScalerINI: Failed to read INI: " + err.Error())
		return
	}
	content := string(data)

	// Disable FPS overlay by default
	content = regexp.MustCompile(`(?m)^ShowFps\s*=\s*auto`).ReplaceAllString(content, "ShowFps=false")
	writeLog("configureOptiScalerINI: FPS overlay disabled")

	// Configure spoofing based on GPU vendor
	if isNvidia {
		content = regexp.MustCompile(`(?m)^Dxgi\s*=\s*auto`).ReplaceAllString(content, "Dxgi=false")
		writeLog("configureOptiScalerINI: NVIDIA detected — Dxgi spoofing disabled")
	} else {
		content = regexp.MustCompile(`(?m)^Dxgi\s*=\s*auto`).ReplaceAllString(content, "Dxgi=true")
		writeLog("configureOptiScalerINI: AMD/Intel detected — Dxgi spoofing enabled")
	}

	// DirectX 11 games run through OptiScaler's D3D11On12 bridge. Force FSR 3.x
	// upscaling (fsr31_12) on that path: it avoids the DLSS preset-override
	// warning ("presets are overridden externally" from NVIDIA App / Inspector)
	// that appears when DLSS is selected on a bridged DX11 title.
	if detectedAPI == "d3d11" {
		content = regexp.MustCompile(`(?m)^Dx11Upscaler\s*=\s*auto`).ReplaceAllString(content, "Dx11Upscaler=fsr31_12")
		writeLog("configureOptiScalerINI: DX11 game detected — Dx11Upscaler forced to FSR 3.1 (D3D11On12)")
	}

	// Configure Neural Rendering based on GPU. Any RTX tier that can run NR
	// (RTX 20-30 and RTX 40-50) gets DlssNr Enabled, otherwise it is disabled.
	if supportsNR {
		content = regexp.MustCompile(`(?m)(\[DlssNr\][\s\S]*?Enabled\s*=\s*)auto`).ReplaceAllString(content, "${1}true")
		writeLog("configureOptiScalerINI: RTX GPU detected — DLSS 5 Neural Rendering enabled")
	} else {
		content = regexp.MustCompile(`(?m)(\[DlssNr\][\s\S]*?Enabled\s*=\s*)auto`).ReplaceAllString(content, "${1}false")
		writeLog("configureOptiScalerINI: Non-RTX GPU — DLSS 5 Neural Rendering disabled")
	}

	if err := os.WriteFile(iniPath, []byte(content), 0644); err != nil {
		writeLog("configureOptiScalerINI: Failed to write INI: " + err.Error())
	}
}

// PatchGame performs the complete patch: ReShade + DLSS 5 or OptiScaler
func (a *App) PatchGame(gamePath string) PatchResult {
	return a.PatchGameWithMode(gamePath, "reshade")
}

// detectInstalledMode returns the mode currently installed on a game, or "" if
// no patcher mode is detected. It uses the patcher's OWN deploy markers only,
// so residual game-shipped files (ReShade.ini, reshade-shaders, dxgi.dll) never
// trigger a false "mode installed" that would wrongly block a re-patch.
func (a *App) detectInstalledMode(gamePath string) string {
	targetDir, _, _, _, err := a.resolveGameTarget(gamePath)
	if err != nil || targetDir == "" {
		return ""
	}
	allDirs := a.findAllTargetDirs(gamePath)
	// OptiScaler marker: OptiScaler.ini + its proxy dxgi.dll (same as
	// checkDLSS5Installed). OptiScaler.ini alone is patcher-owned — games never
	// ship it — so require the proxy too.
	for _, dir := range allDirs {
		if _, err := os.Stat(filepath.Join(dir, "OptiScaler.ini")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "dxgi.dll")); err == nil {
				return "optiscaler"
			}
		}
	}
	// ReShade markers must be patcher-owned add-on files. renodx-dlss5.addon64
	// is always deployed by InstallDLSS5, so its presence proves the patcher's
	// ReShade install is still in place (residual ReShade.ini / reshade-shaders
	// from the game itself are NOT treated as "installed").
	for _, dir := range allDirs {
		if _, err := os.Stat(filepath.Join(dir, "renodx-dlss5.addon64")); err == nil {
			return "reshade"
		}
		if _, err := os.Stat(filepath.Join(dir, "dlss5-feed.addon64")); err == nil {
			return "reshade"
		}
	}
	return ""
}

// PatchGameWithMode performs the complete patch using the selected mode ("reshade" or "optiscaler")
func (a *App) PatchGameWithMode(gamePath string, mode string) PatchResult {
	writeLog(fmt.Sprintf("=== PatchGameWithMode: Starting patch process for %s (mode: %s) ===", gamePath, mode))

	if strings.TrimSpace(gamePath) == "" {
		writeLog("PatchGame: FAILED - Empty game path provided")
		return PatchResult{Success: false, Message: "No game path specified"}
	}

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	if isSameOrChildDirectory(gamePath, exeDir) {
		writeLog("PatchGame: FAILED - Attempted to patch patcher directory")
		return PatchResult{Success: false, Message: "Cannot patch the patcher directory itself"}
	}

	if running, procName := a.isGameRunning(gamePath); running {
		writeLog(fmt.Sprintf("PatchGame: BLOCKED - Game process '%s' is currently running", procName))
		return PatchResult{
			Success: false,
			Message: fmt.Sprintf("Game is currently running (%s). Please close the game first and try again.", procName),
		}
	}

	// Block cross-mode repatching: if a different mode is already installed,
	// the user must uninstall it first to avoid file conflicts.
	existingMode := a.detectInstalledMode(gamePath)
	if existingMode != "" && existingMode != mode {
		label := "ReShade"
		if existingMode == "optiscaler" {
			label = "OptiScaler"
		}
		writeLog(fmt.Sprintf("PatchGameWithMode: BLOCKED - %s is already installed; uninstall %s before switching to %s", label, label, mode))
		return PatchResult{
			Success: false,
			Message: fmt.Sprintf("%s is already installed on this game. Please uninstall it first before switching to a different patch method.", label),
		}
	}

	writeLog("PatchGameWithMode: Step 1 - Backing up original files")
	a.emitPatchStatus("Backing up original game files...")
	backupResult := a.BackupOriginalFiles(gamePath)
	if !backupResult.Success {
		writeLog("PatchGameWithMode: Backup warning: " + backupResult.Message)
	}

	if mode == "optiscaler" {
		writeLog("PatchGameWithMode: Step 2 - Installing OptiScaler")
		result := a.InstallOptiScaler(gamePath)
		if !result.Success {
			writeLog("PatchGameWithMode: FAILED - OptiScaler installation failed: " + result.Message)
			return result
		}
	} else {
		writeLog("PatchGameWithMode: Step 2 - Installing ReShade")
		result := a.InstallReshade(gamePath)
		if !result.Success {
			writeLog("PatchGameWithMode: FAILED - ReShade installation failed: " + result.Message)
			return result
		}

		writeLog("PatchGameWithMode: Step 3 - Installing DLSS 5 files")
		result = a.InstallDLSS5(gamePath)
		if !result.Success {
			writeLog("PatchGameWithMode: FAILED - DLSS 5 installation failed: " + result.Message)
			return result
		}
	}

	writeLog("=== PatchGameWithMode: Patch process completed successfully ===")
	return PatchResult{Success: true, Message: "Patch applied successfully (" + mode + ")!"}
}

// backupDefaultConfig is a small helper that remembers a config file for
// uninstall cleanup (files that exist at install time are never deleted there)
func backupDefaultConfig(dir string, entries map[string]bool) {
	for _, name := range []string{"ReShade.ini", "ReShade.log", "ReShade.log1", "ReShadePreset.ini", "dlss5-feed.cfg", "dlss5-feed.log", "reshade-shaders", "dxgi.dll"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			entries[strings.ToLower(filepath.Join(dir, name))] = true
		}
	}
}

// BackupOriginalFiles backs up existing files before patching in all target directories
func (a *App) BackupOriginalFiles(gamePath string) PatchResult {
	if strings.TrimSpace(gamePath) == "" {
		return PatchResult{Success: false, Message: "Game path cannot be empty"}
	}

	targetDirs := a.findAllTargetDirs(gamePath)
	filesToBackup := []string{
		"d3d9.dll",
		"d3d10.dll",
		"d3d11.dll",
		"d3d12.dll",
		"dxgi.dll",
		"opengl32.dll",
		"ddraw.dll",
		"nvngx_dlss.dll",
		"nvngx_dlssnr.dll",
	}

	backedUpTotal := 0
	a.preExistingFiles = make(map[string]bool)
	for _, targetDir := range targetDirs {
		lowerDir := strings.ToLower(targetDir)
		if strings.Contains(lowerDir, "backup") {
			continue
		}

		dirCount := 0
		backupDir := filepath.Join(targetDir, ".dlss5_backup")
		// Remember what already exists so uninstall never deletes a file the
		// game shipped (the old code dropped pre-existing markers with a 0-file
		// backup, leaving ReShade behind on the next run).
		for _, name := range []string{"d3d9.dll", "d3d10.dll", "d3d11.dll", "d3d12.dll", "dxgi.dll", "opengl32.dll", "ddraw.dll", "nvngx_dlss.dll", "nvngx_dlssnr.dll"} {
			if _, err := os.Stat(filepath.Join(targetDir, name)); err == nil {
				a.preExistingFiles[strings.ToLower(filepath.Join(targetDir, name))] = true
			}
		}
		backupDefaultConfig(targetDir, a.preExistingFiles)

		if _, err := os.Stat(backupDir); err == nil {
			// Backup directory already exists from a previous run - keep it.
		} else {
			os.MkdirAll(backupDir, 0755)
		}

		for _, file := range filesToBackup {
			srcPath := filepath.Join(targetDir, file)
			if _, err := os.Stat(srcPath); err == nil {
				dstPath := filepath.Join(backupDir, file)
				writeLog(fmt.Sprintf("BackupOriginalFiles: Backing up %s in %s", file, targetDir))
				if err := copyFile(srcPath, dstPath); err == nil {
					dirCount++
				}
			}
		}
		backedUpTotal += dirCount

		metadataPath := filepath.Join(backupDir, "backup_info.txt")
		metadata := fmt.Sprintf("Backup created: %s\nTarget Dir: %s\nFiles backed up: %d\n",
			time.Now().Format("2006-01-02 15:04:05"), targetDir, dirCount)
		os.WriteFile(metadataPath, []byte(metadata), 0644)

		// Record which files already existed before we patched, per directory,
		// so cleanup can distinguish user/game files from patcher-owned ones.
		preexistingPath := filepath.Join(backupDir, "preexisting.txt")
		var names []string
		for name := range a.preExistingFiles {
			if strings.HasPrefix(strings.ToLower(name), strings.ToLower(targetDir)) {
				names = append(names, name)
			}
		}
		os.WriteFile(preexistingPath, []byte(strings.Join(names, "\n")), 0644)
	}

	return PatchResult{Success: true, Message: fmt.Sprintf("Original files backed up (%d files across target directories)", backedUpTotal)}
}

// LaunchGame launches the game executable directly
func (a *App) LaunchGame(exePath string) PatchResult {
	writeLog("LaunchGame: Attempting to launch game: " + exePath)

	if strings.TrimSpace(exePath) == "" {
		return PatchResult{Success: false, Message: "Executable path cannot be empty"}
	}

	if _, err := os.Stat(exePath); os.IsNotExist(err) {
		writeLog("LaunchGame: ERROR - Executable not found: " + exePath)
		return PatchResult{Success: false, Message: "Executable not found: " + exePath}
	}

	absPath, err := filepath.Abs(exePath)
	if err != nil {
		return PatchResult{Success: false, Message: "Failed to get absolute path: " + err.Error()}
	}

	gameDir := filepath.Dir(absPath)
	writeLog("LaunchGame: Game directory: " + gameDir)
	writeLog("LaunchGame: Launching: " + absPath)

	cmd := exec.Command(absPath)
	cmd.Dir = gameDir

	if err := cmd.Start(); err != nil {
		writeLog("LaunchGame: ERROR - Failed to launch game: " + err.Error())
		return PatchResult{Success: false, Message: "Failed to launch game: " + err.Error()}
	}

	writeLog("LaunchGame: Game launched successfully: " + filepath.Base(absPath))
	return PatchResult{Success: true, Message: "Game launched successfully: " + filepath.Base(absPath)}
}

// UninstallPatch thoroughly removes ReShade and DLSS 5 files from the game folder and all subfolders
func (a *App) UninstallPatch(gamePath string) PatchResult {
	writeLog("=== UninstallPatch: Starting uninstall process for " + gamePath + " ===")

	if strings.TrimSpace(gamePath) == "" {
		return PatchResult{Success: false, Message: "Game path cannot be empty"}
	}

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	if isSameOrChildDirectory(gamePath, exeDir) {
		return PatchResult{Success: false, Message: "Cannot uninstall from patcher directory"}
	}

	if running, procName := a.isGameRunning(gamePath); running {
		writeLog(fmt.Sprintf("UninstallPatch: BLOCKED - Game process '%s' is currently running", procName))
		return PatchResult{
			Success: false,
			Message: fmt.Sprintf("Game is currently running (%s). Please close the game first and try again.", procName),
		}
	}

	targetDirs := a.findAllTargetDirs(gamePath)
	writeLog(fmt.Sprintf("UninstallPatch: Found %d target directories to clean: %v", len(targetDirs), targetDirs))

	filesToRemove := []string{
		"d3d9.dll",
		"d3d10.dll",
		"d3d11.dll",
		"d3d12.dll",
		"dxgi.dll",
		"opengl32.dll",
		"ddraw.dll",
		"ReShade.ini",
		"ReShade.log",
		"ReShade.log1",
		"ReShadePreset.ini",
		"ReShade.dll",
		"ReShade64.dll",
		"ReShade32.dll",
		"nvngx_dlss.dll",
		"nvngx_dlssnr.dll",
		"dlss5-feed.addon64",
		"dlss5-feed.addon32",
		"dlss5-feed.cfg",
		"dlss5-feed.log",
		"renodx-dlss5.addon64",
		"renodx-dlss5.addon32",
		"OptiScaler.ini",
		"OptiScaler.log",
		"nvngx.dll_dlssnr.dll",
		"nvngx.dll_dlssnr_rtx20.dll",
		"Nvngx_dlssr.dll",
		"Nvngx_dlssm.dll",
	}

	// The DLSS 5 add-on / feed files are placed by this patcher and never
	// ship with a game, so they are ALWAYS patcher-owned and must be purged on
	// uninstall even from folders whose backup marker is empty (the patcher
	// spreads add-ons to hook dirs without backing them tip there).
	addonFilesOwned := []string{
		"dlss5-feed.addon64",
		"dlss5-feed.addon32",
		"dlss5-feed.cfg",
		"dlss5-feed.log",
		"renodx-dlss5.addon64",
		"renodx-dlss5.addon32",
	}

	// ReShade-specific files and folders are treated as fully patcher/third
	// party-owned and are purged clean on uninstall regardless of whether they
	// pre-existed. Games do not ship these paths, so there is no risk of
	// destroying a game's own file.
	reshadeFilesOwned := map[string]bool{
		"ReShade.ini":       true,
		"ReShade.log":       true,
		"ReShade.log1":      true,
		"ReShadePreset.ini": true,
		"ReShade.dll":       true,
		"ReShade64.dll":     true,
		"ReShade32.dll":     true,
	}
	reshadeFoldersOwned := map[string]bool{
		"reshade-shaders": true,
		"ReShade":         true,
	}

	foldersToRemove := []string{
		"reshade-shaders",
		"ReShade",
		"OptiScaler",
	}

	// Any ReShade hook DLL or add-on that the game shipped must never be
	// deleted, even though a repacked game may carry an old ReShade proxy.
	// Default to protect-everything unless the patcher owns the files.
	if a.preExistingFiles == nil {
		a.preExistingFiles = make(map[string]bool)
	}
	for _, dir := range targetDirs {
		preLst := filepath.Join(dir, ".dlss5_backup", "preexisting.txt")
		if data, err := os.ReadFile(preLst); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					a.preExistingFiles[line] = true
				}
			}
		}
	}

	reshadeSetup, _ := a.getReShadeSetup()
	var errors []string

	for _, dir := range targetDirs {
		writeLog("UninstallPatch: Processing directory: " + dir)
		a.emitPatchStatus("Processing folder: " + dir + "...")

		if _, err := os.Stat(reshadeSetup); err == nil {
			exes, _ := filepath.Glob(filepath.Join(dir, "*.exe"))
			for _, exe := range exes {
				if isIgnoredExe(filepath.Base(exe)) {
					continue
				}
				writeLog(fmt.Sprintf("UninstallPatch: Running ReShade uninstaller for %s", exe))
				a.emitPatchStatus("Running ReShade uninstaller for " + filepath.Base(exe) + "...")
				cmd := exec.Command(reshadeSetup,
					exe,
					"--headless",
					"--state", "uninstall",
				)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				writeLog(fmt.Sprintf("UninstallPatch: Output: %s (err: %v)", string(out), err))
			}
		}

		backupDir := filepath.Join(dir, ".dlss5_backup")
		hasBackup := false
		dirProtected := make(map[string]bool)
		if _, err := os.Stat(backupDir); err == nil {
			hasBackup = true

			// A 0-file backup means the patcher never touched this folder;
			// the files there belong to the game itself. Drop the marker and
			// treat everything below as protected so they survive uninstall.
			// Exception: nvngx_dlss.dll / nvngx_dlssnr.dll are deployed by
			// this patcher. If nothing was backed up here, they cannot be the
			// game's own — they are patcher leftovers and MUST be removed.
			if data, err := os.ReadFile(filepath.Join(backupDir, "backup_info.txt")); err == nil {
				if m := regexp.MustCompile(`Files backed up:\s*(\d+)`).FindStringSubmatch(string(data)); len(m) == 2 && m[1] == "0" {
					writeLog("UninstallPatch: Empty backup detected at " + backupDir + ", removing marker only")
					for _, name := range filesToRemove {
						if name == "nvngx_dlss.dll" || name == "nvngx_dlssnr.dll" {
							continue
						}
						dirProtected[strings.ToLower(filepath.Join(dir, name))] = true
					}
					for _, name := range foldersToRemove {
						dirProtected[strings.ToLower(filepath.Join(dir, name))] = true
					}
					for _, name := range []string{"ReShade.log1", "dlss5-feed.cfg", "dlss5-feed.log"} {
						dirProtected[strings.ToLower(filepath.Join(dir, name))] = true
					}
					os.RemoveAll(backupDir)
					hasBackup = false
				}
			}

			if hasBackup {
				writeLog("UninstallPatch: Restoring backup from " + backupDir)
				a.emitPatchStatus("Restoring original files from backup...")
				entries, err := os.ReadDir(backupDir)
				if err == nil {
					for _, entry := range entries {
						if entry.IsDir() || entry.Name() == "backup_info.txt" || entry.Name() == "preexisting.txt" {
							continue
						}
						src := filepath.Join(backupDir, entry.Name())
						dst := filepath.Join(dir, entry.Name())
						os.Remove(dst)
						if err := copyFile(src, dst); err != nil {
							errors = append(errors, fmt.Sprintf("Failed to restore %s: %v", entry.Name(), err))
						} else {
							writeLog(fmt.Sprintf("UninstallPatch: Restored %s", entry.Name()))
						}
					}
				}
			}
		}

		// Add-on files are patcher-owned, never shipped by games — purge them
		// unconditionally in every target dir regardless of backup protection.
		a.emitPatchStatus("Removing DLSS 5 add-on files...")
		for _, addon := range addonFilesOwned {
			addonPath := filepath.Join(dir, addon)
			if _, err := os.Stat(addonPath); err != nil {
				continue
			}
			writeLog(fmt.Sprintf("UninstallPatch: Removing patcher add-on %s", addonPath))
			if err := cleanDLL(addonPath); err != nil {
				writeLog(fmt.Sprintf("UninstallPatch: Failed to remove add-on %s: %v", addonPath, err))
			}
		}

		a.emitPatchStatus("Removing ReShade / DLSS 5 files...")
		for _, file := range filesToRemove {
			fPath := filepath.Join(dir, file)
			if _, err := os.Stat(fPath); err != nil {
				continue
			}
			// ReShade-specific files are always clean-uninstalled, ignoring the
			// pre-existing / backup protection (they are patcher-owned paths).
			if reshadeFilesOwned[file] {
				writeLog(fmt.Sprintf("UninstallPatch: Removing ReShade file %s (clean uninstall)", fPath))
				if err := cleanDLL(fPath); err != nil {
					writeLog(fmt.Sprintf("UninstallPatch: Failed to remove %s: %v", fPath, err))
				}
				continue
			}
			if dirProtected[strings.ToLower(fPath)] || a.preExistingFiles[strings.ToLower(fPath)] {
				continue
			}
			writeLog(fmt.Sprintf("UninstallPatch: Removing %s", fPath))
			if err := cleanDLL(fPath); err != nil {
				writeLog(fmt.Sprintf("UninstallPatch: Failed to remove %s: %v", fPath, err))
			}
		}

		for _, folder := range foldersToRemove {
			fPath := filepath.Join(dir, folder)
			if _, err := os.Stat(fPath); err != nil {
				continue
			}
			// ReShade folders are always clean-uninstalled too.
			if reshadeFoldersOwned[folder] {
				writeLog(fmt.Sprintf("UninstallPatch: Removing ReShade folder %s (clean uninstall)", fPath))
				os.RemoveAll(fPath)
				continue
			}
			if dirProtected[strings.ToLower(fPath)] || a.preExistingFiles[strings.ToLower(fPath)] {
				writeLog(fmt.Sprintf("UninstallPatch: Protecting pre-existing folder %s", fPath))
				continue
			}
			writeLog(fmt.Sprintf("UninstallPatch: Removing folder %s", fPath))
			os.RemoveAll(fPath)
		}

		if hasBackup {
			os.RemoveAll(backupDir)
		}
	}

	if len(errors) > 0 {
		return PatchResult{
			Success: false,
			Message: "Uninstall completed with some warnings:\n" + strings.Join(errors, "\n"),
		}
	}

	writeLog("=== UninstallPatch: Uninstall completed successfully ===")
	a.emitPatchStatus("Uninstall completed.")
	return PatchResult{Success: true, Message: "ReShade and DLSS 5 uninstalled successfully!"}
}

// copyFile copies a single file
func copyFile(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, input, 0644)
}

// copyDir recursively copies a directory
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		dstPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}

		return copyFile(path, dstPath)
	})
}

// GetAppVersion returns the application version
func (a *App) GetAppVersion() string {
	return "1.0.0"
}

// GetSystemInfo returns system information including detected GPU
func (a *App) GetSystemInfo() map[string]string {
	gpu := detectGPU()
	nrStatus := "No"
	if gpu.SupportsNeuralRendering {
		nrStatus = "Yes"
	}
	return map[string]string{
		"os":           goruntime.GOOS,
		"arch":         goruntime.GOARCH,
		"goVersion":    goruntime.Version(),
		"numCPU":       fmt.Sprintf("%d", goruntime.NumCPU()),
		"gpu":          gpu.Name,
		"neuralRender": nrStatus,
		"nrTier":       string(gpu.NRTier),
	}
}
