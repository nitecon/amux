package amuxruntime

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
)

func runTaskAgents(ctx context.Context, client *gatewayclient.Client, project string, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("tasks agents", flag.ContinueOnError)
	flags.SetOutput(stderr)
	osFilter := flags.String("os", "", "filter live agents by OS: linux, windows, or macos")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
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
