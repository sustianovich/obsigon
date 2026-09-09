//go:build windows

package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf16"
)

const vaultPickerScript = `$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
Add-Type -AssemblyName System.Windows.Forms
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = "Selecciona la carpeta del vault de Obsidian"
$dialog.ShowNewFolderButton = $true
if (Test-Path -LiteralPath $env:OBSIGON_PICKER_INITIAL -PathType Container) {
    $dialog.SelectedPath = $env:OBSIGON_PICKER_INITIAL
}
if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
    [Console]::Write($dialog.SelectedPath)
    exit 0
}
exit 2`

func nativeVaultPickerAvailable() bool {
	return true
}

func pickNativeVaultDirectory(ctx context.Context, current string) (string, bool, error) {
	encoded := encodePowerShellCommand(vaultPickerScript)
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encoded)
	cmd.Env = append(os.Environ(), "OBSIGON_PICKER_INITIAL="+current)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("no se pudo abrir el selector de carpetas de Windows: %w", err)
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		return "", false, nil
	}
	return path, true, nil
}

func encodePowerShellCommand(script string) string {
	units := utf16.Encode([]rune(script))
	data := make([]byte, len(units)*2)
	for index, unit := range units {
		data[index*2] = byte(unit)
		data[index*2+1] = byte(unit >> 8)
	}
	return base64.StdEncoding.EncodeToString(data)
}
