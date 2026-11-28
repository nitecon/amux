package amuxruntime

import (
	"context"
	"io"

	"github.com/nitecon/amux/config"
)

func StartClient(ctx context.Context, state string, stdout, stderr io.Writer) error {
	path, err := config.DefaultClientPath()
	if err != nil {
		return err
	}
	return runConfiguredClient(ctx, path, state, stdout, stderr)
}
