package desktop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func pickCheckoutFolder(ctx context.Context, title, initial string) (string, error) {
	const script = `Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = $env:AMUX_PICKER_TITLE
$dialog.SelectedPath = $env:AMUX_PICKER_INITIAL
try {
    if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
        [Console]::Write($dialog.SelectedPath)
    }
} finally {
    $dialog.Dispose()
}`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Env = append(os.Environ(), "AMUX_PICKER_TITLE="+title, "AMUX_PICKER_INITIAL="+initial)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not open the Windows folder picker: %w", err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}
