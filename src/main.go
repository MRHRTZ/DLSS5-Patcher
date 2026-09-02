package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func runDiagnostic(args []string) bool {
	if len(args) < 3 || (args[1] != "--diagnose" && args[1] != "--json-diagnostic") {
		return false
	}

	app := NewApp()
	app.diagnostic = true
	command := strings.ToLower(args[2])
	var output interface{}

	switch command {
	case "list":
		output = map[string]interface{}{
			"command": "list",
			"games":   app.DetectGames(),
		}
	case "preview":
		if len(args) < 4 {
			output = map[string]interface{}{"success": false, "error": "preview requires an executable or game path"}
		} else {
			path := args[3]
			output = map[string]interface{}{
				"command": "preview",
				"input":   path,
				"preview": app.GetGameFolderPreview(path),
				"details": app.GetGameDetails(path),
			}
		}
	case "patch", "uninstall":
		if len(args) < 4 {
			output = map[string]interface{}{"success": false, "error": command + " requires a game path"}
		} else {
			path := args[3]
			var result PatchResult
			if command == "patch" {
				result = app.PatchGame(path)
			} else {
				result = app.UninstallPatch(path)
			}
			output = map[string]interface{}{
				"command": command,
				"input":   path,
				"result":  result,
				"preview": app.GetGameFolderPreview(path),
				"details": app.GetGameDetails(path),
			}
		}
	default:
		output = map[string]interface{}{"success": false, "error": "unknown diagnostic command: " + command}
	}

	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fmt.Printf("{\"success\":false,\"error\":%q}\n", err.Error())
	} else {
		fmt.Println(string(encoded))
	}
	return true
}

func main() {
	if runDiagnostic(os.Args) {
		return
	}

	// Initialize logger
	initLogger()
	defer closeLogger()

	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title:  "DLSS 5 Patcher",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 10, G: 10, B: 10, A: 1},
		OnStartup:        app.startup,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		writeLog("Error: " + err.Error())
		println("Error:", err.Error())
	}
}
