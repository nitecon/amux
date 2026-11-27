package desktop

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/update"
)

func TestUpdateNowAutomaticallySchedulesVerifiedArchive(t *testing.T) {
	test.NewTempApp(t)
	installed := make(chan string, 1)
	panel := newAppControlsPanel("amux", func(archive string) error {
		installed <- archive
		return nil
	}, func(context.Context, string) (string, update.UpdateInfo, error) {
		return "bundle.zip", update.UpdateInfo{Available: true, Version: "v0.9.1"}, nil
	})
	test.Tap(panel.update)
	select {
	case archive := <-installed:
		if archive != "bundle.zip" {
			t.Fatalf("scheduled archive = %q", archive)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Update now did not schedule the verified update")
	}
}

func TestLaunchItemFileWritesPrivateFileAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autostart", "amux.desktop")
	executable := filepath.Join(t.TempDir(), "amux-desktop")
	if err := launchItemFile(path, executable, "[Desktop Entry]\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("launch item mode = %o, want 600", info.Mode().Perm())
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "[Desktop Entry]\n" {
		t.Fatalf("launch item content = %q, err = %v", content, err)
	}
}

func TestLaunchItemFileRequiresAbsoluteExecutable(t *testing.T) {
	err := launchItemFile(filepath.Join(t.TempDir(), "item"), "amux-desktop", "content")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative executable error = %v", err)
	}
}

func TestLaunchItemContentEscapesExecutablePaths(t *testing.T) {
	linux := linuxLaunchItemContent(`/Applications/Amux % Beta/"amux-desktop`)
	if !strings.Contains(linux, `Exec="/Applications/Amux %% Beta/\"amux-desktop"`) {
		t.Fatalf("Linux launch entry did not escape path: %q", linux)
	}
	mac := macLaunchItemContent(`/Applications/Amux & Beta/amux-desktop`)
	if !strings.Contains(mac, `<string>/Applications/Amux &amp; Beta/amux-desktop</string>`) {
		t.Fatalf("macOS launch plist did not escape path: %q", mac)
	}
}
