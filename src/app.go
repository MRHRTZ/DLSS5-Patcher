package main

import (
	"bytes"
	"context"
	"debug/pe"
	"fmt"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows/registry"
)

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
	reshadeConfigFile = "ReShadeConfig.json"
	defaultReShadeURL = "https://reshade.me/downloads/ReShade_Setup_6.8.0.exe"
	defaultReShadeExe = "ReShade_Setup_6.8.0.exe"
)

type reshadeConfig struct {
	URL string `json:"url"`
}

// getReShadeSetup resolves the ReShade setup executable path, checking a
// config file first, then well-known local installs, then the bundled Addon
// build as a last resort. If none exists, it will be downloaded on demand.
func getReShadeSetup() (string, error) {
	candidates := []string{
		// 1. User-configured path
		getAssetPath(reshadeConfigFile),
		getAssetPath(filepath.Join("ReShade", reshadeConfigFile)),
		getAssetPath(filepath.Join("ReShade", "ReShade_Setup_6.8.0_Addon.exe")),
		getAssetPath(filepath.Join("ReShade", defaultReShadeExe)),
	}

	configuredURL := ""
	if cfg, err := os.ReadFile(candidates[0]); err == nil {
		var config reshadeConfig
		if err := json.Unmarshal(cfg, &config); err != nil {
			writeLog("getReShadeSetup: invalid JSON config: " + err.Error())
		} else {
			configuredURL = strings.TrimSpace(config.URL)
		}
	}
	for _, c := range candidates[1:] {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}

	downloadURL := defaultReShadeURL
	if configuredURL != "" {
		downloadURL = configuredURL
	}
	writeLog("getReShadeSetup: No ReShade installer found locally, downloading from " + downloadURL)
	dest := getAssetPath(filepath.Join("ReShade", defaultReShadeExe))
	if err := downloadFile(downloadURL, dest); err != nil {
		return "", fmt.Errorf("failed to download ReShade installer: %v", err)
	}
	return dest, nil
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
	Size              uint32
	Usage             uint32
	ProcessID         uint32
	DefaultHeapID     uintptr
	ModuleID          uint32
	Threads           uint32
	ParentProcessID   uint32
	PriClassBase      int32
	Flags             uint32
	ExeFile           [260]uint16
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

// App struct
type App struct {
	ctx             context.Context
	preExistingFiles map[string]bool
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
	Name          string      `json:"name"`
	Path          string      `json:"path"`
	Executable    string      `json:"executable"`
	RenderingAPI  string      `json:"renderingAPI"`
	DLSSVersion   string      `json:"dlssVersion"`
	DLSS5Addon    string      `json:"dlss5Addon"`
	ReshadeStatus string      `json:"reshadeStatus"`
	IsInstalled   bool        `json:"isInstalled"`
	DLLList       []DLLDetail `json:"dllList"`
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
	peFile, err := pe.Open(exePath)
	if err != nil {
		return false, ""
	}
	defer peFile.Close()

	if peFile.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		return false, ""
	}

	libs, err := peFile.ImportedLibraries()
	if err != nil {
		return false, ""
	}

	for _, lib := range libs {
		l := strings.ToLower(lib)
		if strings.Contains(l, "vulkan-1.dll") {
			return true, "vulkan"
		}
		if strings.Contains(l, "d3d12.dll") || strings.Contains(l, "dxgi.dll") {
			return true, "dxgi"
		}
		if strings.Contains(l, "d3d11.dll") {
			return true, "d3d11"
		}
		if strings.Contains(l, "d3d10.dll") {
			return true, "d3d10"
		}
		if strings.Contains(l, "d3d9.dll") {
			return true, "d3d9"
		}
		if strings.Contains(l, "opengl32.dll") {
			return true, "opengl"
		}
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

	libs, err := peFile.ImportedLibraries()
	if err != nil {
		return result
	}
	for _, lib := range libs {
		l := strings.ToLower(lib)
		switch {
		case strings.Contains(l, "vulkan-1.dll") || strings.Contains(l, "vulkan"):
			result = struct {
				api   string
				label string
			}{api: "vulkan", label: "Vulkan"}
		case strings.Contains(l, "d3d12.dll") || strings.Contains(l, "dxgi.dll"):
			result = struct {
				api   string
				label string
			}{api: "dxgi", label: "DirectX 12"}
		case strings.Contains(l, "d3d11.dll"):
			result = struct {
				api   string
				label string
			}{api: "d3d11", label: "DirectX 11"}
		case strings.Contains(l, "d3d10.dll"):
			result = struct {
				api   string
				label string
			}{api: "d3d10", label: "DirectX 10"}
		case strings.Contains(l, "d3d9.dll"):
			result = struct {
				api   string
				label string
			}{api: "d3d9", label: "DirectX 9"}
		case strings.Contains(l, "opengl32.dll"):
			result = struct {
				api   string
				label string
			}{api: "opengl", label: "OpenGL"}
		}
		if result.api != "" {
			break
		}
	}
	return result
}

// getReshadeInfo probes a directory for a ReShade hook DLL and returns the
// matched file name and a report of whether it supports add-ons
func getReshadeInfo(dir string) (string, string, bool) {
	hookCandidates := []string{"dxgi.dll", "d3d12.dll", "d3d11.dll", "d3d9.dll", "d3d10.dll", "opengl32.dll", "dinput8.dll"}
	for _, name := range hookCandidates {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		ver := getDLLFileVersion(p)
		support := false
		if ver != "" {
			if b, err := os.ReadFile(p); err == nil {
				support = bytes.Contains(b, []byte("Searching for add-ons"))
			}
		}
		return p, ver, support
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
	if targetExe == "" {
		topExes, _ := filepath.Glob(filepath.Join(gameDir, "*.exe"))
		for _, exe := range topExes {
			if !isIgnoredExe(filepath.Base(exe)) {
				targetExe = exe
				targetDir = gameDir
				break
			}
		}
	}

	if targetExe == "" && !stat.IsDir() {
		if !isIgnoredExe(filepath.Base(cleanPath)) {
			targetExe = cleanPath
			targetDir = filepath.Dir(cleanPath)
		}
	}

	// Repacked releases keep multiple identical cracked executables in side
	// folders (_crack, _original files, ...). For a normal game folder the root
	// launcher is always the one the player runs, so make it the fallback.
	if targetExe == "" {
		for _, exe := range a.allRootExes(gameDir) {
			if !isIgnoredExe(filepath.Base(exe)) {
				targetExe = exe
				targetDir = filepath.Dir(exe)
				break
			}
		}
	}

	if targetExe == "" {
		return "", "", "", "", fmt.Errorf("no valid game executable found in: %s", gameDir)
	}

	if launchExe == "" {
		rootExes := a.allRootExes(gameDir)
		for _, exe := range rootExes {
			if !isIgnoredExe(filepath.Base(exe)) {
				launchExe = exe
				break
			}
		}
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

// getEpicManifestGames retrieves installed game directories from Epic Games Launcher manifests
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
		runtime.EventsEmit(a.ctx, "scan:game", game)
		writeLog(fmt.Sprintf("DetectGames: Found '%s' at %s (Patched: %v, API: %s)", name, path, isInstalled, detectedAPI))
	}

	updateStatus := func(msg string) {
		runtime.EventsEmit(a.ctx, "scan:status", msg)
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
	runtime.EventsEmit(a.ctx, "scan:complete", len(games))
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

	files := []string{
		"nvngx_dlss.dll",
		"nvngx_dlssnr.dll",
		"renodx-dlss5.addon64",
	}

	for _, file := range files {
		if _, err := os.Stat(filepath.Join(targetDir, file)); os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// detectGameAPI detects the rendering API used by a game executable via fast PE headers & 2MB buffer scan
func (a *App) detectGameAPI(exePath string) string {
	writeLog("detectGameAPI: Analyzing executable " + exePath)

	if peFile, err := pe.Open(exePath); err == nil {
		defer peFile.Close()
		if libs, err := peFile.ImportedLibraries(); err == nil {
			for _, lib := range libs {
				l := strings.ToLower(lib)
				if strings.Contains(l, "vulkan-1.dll") || strings.Contains(l, "vulkan") {
					writeLog("detectGameAPI: Detected Vulkan API via PE import")
					return "vulkan"
				}
				if strings.Contains(l, "d3d12.dll") || strings.Contains(l, "dxgi.dll") {
					writeLog("detectGameAPI: Detected Direct3D 12 / DXGI API via PE import")
					return "dxgi"
				}
				if strings.Contains(l, "d3d11.dll") {
					writeLog("detectGameAPI: Detected Direct3D 11 API via PE import")
					return "d3d11"
				}
				if strings.Contains(l, "d3d10.dll") {
					writeLog("detectGameAPI: Detected Direct3D 10 API via PE import")
					return "d3d10"
				}
				if strings.Contains(l, "d3d9.dll") {
					writeLog("detectGameAPI: Detected Direct3D 9 API via PE import")
					return "d3d9"
				}
				if strings.Contains(l, "opengl32.dll") {
					writeLog("detectGameAPI: Detected OpenGL API via PE import")
					return "opengl"
				}
			}
		}
	}

	f, err := os.Open(exePath)
	if err != nil {
		writeLog("detectGameAPI: Failed to open executable: " + err.Error())
		return "dxgi"
	}
	defer f.Close()

	buf := make([]byte, 2*1024*1024)
	n, _ := f.Read(buf)
	content := strings.ToLower(string(buf[:n]))

	if strings.Contains(content, "vulkan-1.dll") || strings.Contains(content, "vulkan") {
		return "vulkan"
	}
	if strings.Contains(content, "opengl32.dll") || strings.Contains(content, "opengl") {
		return "opengl"
	}
	if strings.Contains(content, "d3d9.dll") {
		return "d3d9"
	}
	if strings.Contains(content, "d3d10.dll") {
		return "d3d10"
	}
	if strings.Contains(content, "d3d11.dll") {
		return "d3d11"
	}
	if strings.Contains(content, "d3d12.dll") || strings.Contains(content, "dxgi.dll") {
		return "dxgi"
	}

	writeLog("detectGameAPI: Defaulting to DXGI")
	return "dxgi"
}

// GetGameDetails analyzes game files and returns complete component versions, API, and DLL list
func (a *App) GetGameDetails(gamePathOrExe string) GameDetails {
	details := GameDetails{
		DLSSVersion:   "Not Available",
		DLSS5Addon:    "Not Installed",
		ReshadeStatus: "Not Installed",
		DLLList:       []DLLDetail{},
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

	if relExe, err := filepath.Rel(rootDir, targetExe); err == nil {
		details.Executable = relExe
	} else {
		details.Executable = targetExe
	}

	switch detectedAPI {
	case "dxgi", "d3d12":
		details.RenderingAPI = "DirectX 12"
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
		details.RenderingAPI = "DirectX 12"
	}

	details.IsInstalled = a.checkDLSS5Installed(rootDir)

	// Check DLSS 5 add-on status consistently
	renodxPath := filepath.Join(targetDir, "renodx-dlss5.addon64")
	feedPath := filepath.Join(targetDir, "dlss5-feed.addon64")
	hasAddon := false
	if _, err := os.Stat(renodxPath); err == nil {
		hasAddon = true
	} else if _, err := os.Stat(feedPath); err == nil {
		hasAddon = true
	}

	hasDLSSDLLs := false
	if _, err := os.Stat(filepath.Join(targetDir, "nvngx_dlss.dll")); err == nil {
		if _, err := os.Stat(filepath.Join(targetDir, "nvngx_dlssnr.dll")); err == nil {
			hasDLSSDLLs = true
		}
	}

	if hasAddon && hasDLSSDLLs {
		details.DLSS5Addon = "Installed"
	} else if hasAddon || hasDLSSDLLs {
		details.DLSS5Addon = "Incomplete"
	} else {
		details.DLSS5Addon = "Not Installed"
	}

	_, reshadeVer, reshadeAddon := getReshadeInfo(targetDir)
	if reshadeVer != "" {
		clean := "Installed"
		if reshadeAddon {
			clean = "Installed + add-on"
		} else if details.DLSS5Addon == "Installed" || details.DLSS5Addon == "Incomplete" {
			clean = "Installed (legacy proxy)"
		}
		details.ReshadeStatus = clean + " (" + reshadeVer + ")"
	} else if _, err := os.Stat(filepath.Join(targetDir, "ReShade.ini")); err == nil {
		if details.DLSS5Addon == "Installed" || details.DLSS5Addon == "Incomplete" {
			details.ReshadeStatus = "Installed (+ add-on)"
		} else {
			details.ReshadeStatus = "Installed"
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

// gameHasNativeDLSS reports whether the game ships its own DLSS runtime
// (nvngx_dlss.dll / nvngx_dlssd.dll / _nvngx.dll / Streamline sl.dlss.dll).
// Native-DLSS games use the Direct RenoDX add-on path and must NOT get the
// DLSS5-Feeder (it would race the game's own NGX session and crash).
func (a *App) gameHasNativeDLSS(gamePath string) bool {
	markers := []string{"nvngx_dlss.dll", "_nvngx.dll", "sl.dlss.dll", "nvngx_dlssd.dll", "nvngx_dlssg.dll"}
	dirs := a.findAllTargetDirs(gamePath)
	for _, dir := range dirs {
		for _, m := range markers {
			if p, err := os.Stat(filepath.Join(dir, m)); err == nil && !p.IsDir() {
				return true
			}
		}
	}
	return false
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

	reshadeSetup, setupErr := getReShadeSetup()
	writeLog("InstallReshade: ReShade setup path: " + reshadeSetup)
	writeLog(fmt.Sprintf("InstallReshade: Target EXE: %s in Directory: %s with API: %s", targetExe, targetDir, api))

	// If the game already ships a ReShade hook DLL, prefer the working build so
	// the player's existing ReShade (settings, preset, shader setup) is kept.
	// We only need to make sure the preset, config and add-ons are in place.
	if hookFile, _, addonSupport := getReshadeInfo(targetDir); hookFile != "" {
		writeLog("InstallReshade: Existing ReShade hook found at " + hookFile + " (add-on support: " + fmt.Sprintf("%v", addonSupport) + ") - skipping DLL replacement")
		a.ensureReshadeIni(targetDir, native)
		a.ensureReshadePreset(targetDir, native)
		a.copyEffects(targetDir)

		if !addonSupport {
			// The shipped ReShade has no add-on support; upgrade it to the
			// bundled add-on build so DLSS 5 feed/renodx add-ons can load.
			writeLog("InstallReshade: Existing ReShade lacks add-on support, replacing with bundled add-on build")
			moduleName := "dxgi.dll"
			switch api {
			case "d3d11":
				moduleName = "d3d11.dll"
			case "d3d9":
				moduleName = "d3d9.dll"
			case "opengl":
				moduleName = "opengl32.dll"
			}
a.copyReshadeFilesManually(targetDir, api, native)
			_ = a.installBackupDLL(targetDir, moduleName)
		}

		writeLog("InstallReshade: ReShade already present with add-on support, configuration updated for " + targetDir)
		return PatchResult{Success: true, Message: "ReShade already installed (add-on build found); configuration updated"}
	}

	if setupErr != nil {
		writeLog("InstallReshade: ERROR resolving ReShade setup: " + setupErr.Error())
		return a.copyReshadeFilesManually(targetDir, api, native)
	}

	if _, err := os.Stat(reshadeSetup); os.IsNotExist(err) {
		writeLog("InstallReshade: ERROR - ReShade setup not found at: " + reshadeSetup)
		return a.copyReshadeFilesManually(targetDir, api, native)
	}

	writeLog("InstallReshade: Executing ReShade setup via CLI...")
	cmd := exec.Command(reshadeSetup,
		targetExe,
		"--headless",
		"--api", api,
		"--state", "finished",
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
	if _, statErr := os.Stat(hookDLL); statErr != nil {
		writeLog("InstallReshade: Hook DLL not found after CLI setup. Falling back to manual copy.")
		a.copyReshadeFilesManually(targetDir, api, native)
	}

	a.ensureReshadeIni(targetDir, native)
	a.ensureReshadePreset(targetDir, native)
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
func (a *App) copyReshadeFilesManually(targetDir string, api string, native bool) PatchResult {
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

	extractedDLL := getAssetPath(filepath.Join("ReShade", "Extracted", "ReShade64.dll"))
	if _, err := os.Stat(extractedDLL); err != nil {
		reshadeSetup, _ := getReShadeSetup()
		if _, statErr := os.Stat(reshadeSetup); statErr == nil {
			extractReshadeDLL(reshadeSetup, filepath.Dir(extractedDLL))
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

// extractReshadeDLL extracts ReShade64.dll from the ReShade setup executable
func extractReshadeDLL(setupPath, outputDir string) error {
	cmd := exec.Command("7z", "x", setupPath, "-o"+outputDir, "ReShade64.dll", "-y")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("7z extraction failed: %v, output: %s", err, string(output))
	}
	return nil
}

// copyEffects copies shaders and textures to the target directory
func (a *App) copyEffects(targetDir string) error {
	writeLog("copyEffects: Copying shaders to " + targetDir)

	srcShaders := getAssetPath(filepath.Join("data", "reshade-shaders-source"))
	if _, err := os.Stat(srcShaders); err != nil {
		srcShaders = getAssetPath(filepath.Join("ReShade", "Effects", "reshade-shaders"))
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

	srcPath := getAssetPath("data")
	writeLog("InstallDLSS5: Source path: " + srcPath + " -> Target: " + targetDir)

	// Whether the game is native-DLSS decides the add-on set: federated
	// (feed + renodx) for games without DLSS, Direct (renodx only) for games
	// that ship nvngx_dlss.dll so we do not race their NGX session.
	native := a.gameHasNativeDLSS(gamePath)

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

	for _, file := range filesToShipping {
		src := filepath.Join(srcPath, file)
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
			src := filepath.Join(srcPath, file)
			dst := filepath.Join(dir, file)
			writeLog(fmt.Sprintf("InstallDLSS5: Copying %s to target dir %s", src, dst))
			if _, err := os.Stat(src); err == nil {
				_ = copyFile(src, dst)
			}
		}
	}

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

// PatchGame performs the complete patch: ReShade + DLSS 5
func (a *App) PatchGame(gamePath string) PatchResult {
	writeLog("=== PatchGame: Starting patch process for " + gamePath + " ===")

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

	writeLog("PatchGame: Step 1 - Backing up original files")
	backupResult := a.BackupOriginalFiles(gamePath)
	if !backupResult.Success {
		writeLog("PatchGame: Backup warning: " + backupResult.Message)
	}

	writeLog("PatchGame: Step 2 - Installing ReShade")
	result := a.InstallReshade(gamePath)
	if !result.Success {
		writeLog("PatchGame: FAILED - ReShade installation failed: " + result.Message)
		return result
	}

	writeLog("PatchGame: Step 3 - Installing DLSS 5 files")
	result = a.InstallDLSS5(gamePath)
	if !result.Success {
		writeLog("PatchGame: FAILED - DLSS 5 installation failed: " + result.Message)
		return result
	}

	writeLog("=== PatchGame: Patch process completed successfully ===")
	return PatchResult{Success: true, Message: "DLSS 5 Patch applied successfully!"}
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
	}

	foldersToRemove := []string{
		"reshade-shaders",
		"ReShade",
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

	reshadeSetup, _ := getReShadeSetup()
	var errors []string

	for _, dir := range targetDirs {
		writeLog("UninstallPatch: Processing directory: " + dir)

		if _, err := os.Stat(reshadeSetup); err == nil {
			exes, _ := filepath.Glob(filepath.Join(dir, "*.exe"))
			for _, exe := range exes {
				if isIgnoredExe(filepath.Base(exe)) {
					continue
				}
				writeLog(fmt.Sprintf("UninstallPatch: Running ReShade uninstaller for %s", exe))
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
			if data, err := os.ReadFile(filepath.Join(backupDir, "backup_info.txt")); err == nil {
				if m := regexp.MustCompile(`Files backed up:\s*(\d+)`).FindStringSubmatch(string(data)); len(m) == 2 && m[1] == "0" {
					writeLog("UninstallPatch: Empty backup detected at " + backupDir + ", removing marker only")
					for _, name := range filesToRemove {
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

		for _, file := range filesToRemove {
			fPath := filepath.Join(dir, file)
			if _, err := os.Stat(fPath); err != nil {
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

// GetSystemInfo returns system information
func (a *App) GetSystemInfo() map[string]string {
	return map[string]string{
		"os":        goruntime.GOOS,
		"arch":      goruntime.GOARCH,
		"goVersion": goruntime.Version(),
		"numCPU":    fmt.Sprintf("%d", goruntime.NumCPU()),
	}
}