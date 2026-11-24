//go:build !darwin && !linux && !windows

package desktop

import (
	"context"
	"errors"
)

func pickCheckoutFolder(context.Context, string, string) (string, error) {
	return "", errors.New("native folder selection is unavailable on this platform")
}
