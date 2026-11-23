//go:build !darwin && !linux && !windows

package desktop

import "os"

func openWorkspacePreviewFile(root *os.Root, relative string) (*os.File, error) {
	return root.Open(relative)
}
