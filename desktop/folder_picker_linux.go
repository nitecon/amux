package desktop

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rymdport/portal/filechooser"
)

func pickCheckoutFolder(ctx context.Context, title, initial string) (string, error) {
	type result struct {
		files []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		files, err := filechooser.OpenFile("", title, &filechooser.OpenFileOptions{
			Directory: true, AcceptLabel: "Select Folder", CurrentFolder: initial,
		})
		done <- result{files, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case selected := <-done:
		if selected.err != nil {
			return "", fmt.Errorf("could not open the desktop folder picker: %w", selected.err)
		}
		if len(selected.files) == 0 {
			return "", nil
		}
		folder, ok := strings.CutPrefix(selected.files[0], "file://")
		if !ok || !filepath.IsAbs(folder) {
			return "", fmt.Errorf("choose a folder on this machine")
		}
		return folder, nil
	}
}
