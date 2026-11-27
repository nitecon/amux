package amuxruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/urfave/cli/v3"
)

var errCommandHelp = errors.New("command help displayed")

func commandFlags(name string, stderr io.Writer) *cli.Command {
	return &cli.Command{Name: name, Writer: stderr, ErrWriter: stderr, HideHelpCommand: true,
		OnUsageError: func(_ context.Context, _ *cli.Command, err error, _ bool) error {
			if name, ok := strings.CutPrefix(err.Error(), "flag provided but not defined: "); ok {
				return fmt.Errorf("unknown flag %q", name)
			}
			return err
		},
	}
}

// parseCommand uses urfave to bind the command flags before the legacy
// operation handlers run. Help exits before those handlers can perform work.
func parseCommand(ctx context.Context, cmd *cli.Command, args []string) error {
	parsed := false
	cmd.Action = func(context.Context, *cli.Command) error { parsed = true; return nil }
	if err := cmd.Run(ctx, append([]string{cmd.Name}, args...)); err != nil {
		return err
	}
	if !parsed {
		return errCommandHelp
	}
	return nil
}

func parseOperation(ctx context.Context, cmd *cli.Command, args []string) ([]string, error) {
	if err := parseCommand(ctx, cmd, args); err != nil {
		return nil, err
	}
	return cmd.Args().Slice(), nil
}
