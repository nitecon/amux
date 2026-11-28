//go:build !desktop

package desktopapp

import (
	"context"
	"errors"
)

func Run(context.Context, []string) error {
	return errors.New("desktop support requires an amux build with the desktop tag")
}
