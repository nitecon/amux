//go:build darwin || linux

package desktop

import (
	"os"
	"syscall"
)

func openWorkspacePreviewFile(root *os.Root, relative string) (*os.File, error) {
	// Nonblocking prevents a regular file replaced by a FIFO after Stat from
	// hanging the bounded preview worker while retaining os.Root confinement.
	return root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
