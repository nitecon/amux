package amuxruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
	"github.com/urfave/cli/v3"
)

func runTaskAgents(ctx context.Context, client *gatewayclient.Client, project string, args []string, stdout, stderr io.Writer) error {
	flags := commandFlags("tasks agents", stderr)
	osFilter := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "os", Value: "", Usage: "filter live agents by OS: linux, windows, or macos", Destination: osFilter})
	if err := parseCommand(ctx, flags, args); err != nil {
		return err
	}
	if flags.Args().Len() != 0 {
		return fmt.Errorf("tasks agents does not accept positional arguments")
	}
	directory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve current checkout: %w", err)
	}
	identity, err := projectidentity.Resolve(ctx, directory)
	if err != nil {
		return fmt.Errorf("resolve current checkout identity: %w", err)
	}
	result, err := client.TaskAgents(ctx, project, identity.Upstream, *osFilter)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}
