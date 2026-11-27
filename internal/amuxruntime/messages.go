package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
	"github.com/urfave/cli/v3"
)

func runMessages(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	usage := "usage: amux messages [--gateway-url URL|--server URL] [--enrolled-profile PATH] [--project IDENT] [--api-key-env NAME] list [--limit N] [--offset N] | get ID | send [--subject TEXT] [--] BODY"
	if hasTaskHelp(args) {
		_, _ = fmt.Fprintln(stderr, usage)
		return nil
	}
	flags := commandFlags("amux messages", stderr)

	gatewayDefault := getenv("AMUX_GATEWAY_URL")
	gatewayURL := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "gateway-url", Value: gatewayDefault, Usage: "gateway URL (defaults to AMUX_GATEWAY_URL)", Destination: gatewayURL, Aliases: []string{"server"}})

	project := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "project", Value: getenv("AMUX_GATEWAY_PROJECT"), Usage: "gateway project identifier (defaults to AMUX_GATEWAY_PROJECT)", Destination: project})
	nativeKeyEnv := getenv("AMUX_GATEWAY_API_KEY_ENV")
	keyEnvDefault := nativeKeyEnv
	if keyEnvDefault == "" {
		keyEnvDefault = "GATEWAY_API_KEY"
	}
	keyEnv := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "api-key-env", Value: keyEnvDefault, Usage: "environment variable containing the gateway API credential", Destination: keyEnv})
	enrolledProfile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "enrolled gateway credential manifest", Destination: enrolledProfile})
	limit := new(uint64)
	flags.Flags = append(flags.Flags, &cli.Uint64Flag{Name: "limit", Value: 50, Usage: "message list page size (1 to 500)", Destination: limit})
	offset := new(uint64)
	flags.Flags = append(flags.Flags, &cli.Uint64Flag{Name: "offset", Value: 0, Usage: "message list offset", Destination: offset})
	subject := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "subject", Value: "", Usage: "optional publication subject", Destination: subject})
	positional, err := parseOperation(ctx, flags, args)
	if err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if len(positional) == 0 {
		return fmt.Errorf("%s", usage)
	}
	operation, opArgs := positional[0], positional[1:]
	if (*gatewayURL == "" && *enrolledProfile == "") || *project == "" {
		return fmt.Errorf("messages requires --gateway-url or --enrolled-profile and --project")
	}

	var (
		messageID int64
		origin    storage.AgentOrigin
		body      string
	)
	switch operation {
	case "list":
		if len(opArgs) != 0 || flags.IsSet("subject") {
			return fmt.Errorf("usage: amux messages list [--limit N] [--offset N]")
		}
		if *limit < 1 || *limit > 500 {
			return fmt.Errorf("message limit must be between 1 and 500")
		}
	case "get":
		if len(opArgs) != 1 || flags.IsSet("limit") || flags.IsSet("offset") || flags.IsSet("subject") {
			return fmt.Errorf("usage: amux messages get ID")
		}
		parsedID, parseErr := strconv.ParseInt(opArgs[0], 10, 64)
		if parseErr != nil || parsedID <= 0 {
			return fmt.Errorf("message ID must be a positive signed 64-bit integer")
		}
		messageID = parsedID
	case "send":
		if len(opArgs) != 1 || flags.IsSet("limit") || flags.IsSet("offset") {
			return fmt.Errorf("usage: amux messages send [--subject TEXT] [--] BODY")
		}
		var identityErr error
		origin, identityErr = messageActor(ctx, getenv)
		if identityErr != nil {
			return identityErr
		}
		body = opArgs[0]
	default:
		return fmt.Errorf("unknown messages operation %q", operation)
	}
	apiKey := ""
	if *enrolledProfile == "" {
		apiKey, err = messageAPIKey(getenv, *keyEnv, nativeKeyEnv, flags.IsSet("api-key-env"))
		if err != nil {
			return err
		}
	}
	client, _, err := gatewayClientForProfile(*gatewayURL, *enrolledProfile, apiKey)
	if err != nil {
		return err
	}
	var value any
	switch operation {
	case "list":
		value, err = client.ListProjectMessageThreads(ctx, *project, gatewayclient.ProjectMessageListQuery{
			State: "all", Limit: limit, Offset: *offset,
		})
	case "get":
		value, err = client.GetProjectMessageThread(ctx, *project, messageID)
	case "send":
		input := gatewayclient.ProjectMessageInput{Body: &body}
		if flags.IsSet("subject") {
			input.Subject = subject
		}
		value, err = client.SendProjectMessage(ctx, *project, input, origin)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(value)
}

func messageAPIKey(getenv func(string) string, keyEnv, nativeKeyEnv string, explicitlySet bool) (string, error) {
	key := getenv(keyEnv)
	if key == "" && !explicitlySet && nativeKeyEnv == "" {
		key = getenv("AMUX_GATEWAY_KEY")
	}
	if key == "" {
		return "", fmt.Errorf("gateway API credential is not configured")
	}
	return key, nil
}

func messageActor(ctx context.Context, getenv func(string) string) (storage.AgentOrigin, error) {
	origin, err := storage.NormalizeOrigin(&storage.AgentOrigin{
		SessionID: getenv("AMUX_AGENT_SESSION_ID"), InstanceID: getenv("AMUX_AGENT_INSTANCE_ID"),
		Provider: getenv("AMUX_AGENT_PROVIDER"), OS: getenv("AMUX_AGENT_OS"),
	})
	if err != nil || origin == nil || origin.SessionID == "00000000-0000-0000-0000-000000000000" || origin.InstanceID == "00000000-0000-0000-0000-000000000000" {
		return storage.AgentOrigin{}, fmt.Errorf("messages send requires a complete native identity from AMUX_AGENT_*")
	}
	launchDirectory := getenv("AMUX_AGENT_PROJECT")
	if strings.TrimSpace(launchDirectory) == "" {
		return storage.AgentOrigin{}, fmt.Errorf("messages send requires AMUX_AGENT_PROJECT")
	}
	currentDirectory, err := os.Getwd()
	if err != nil {
		return storage.AgentOrigin{}, fmt.Errorf("resolve current checkout: %w", err)
	}
	launch, err := projectidentity.Resolve(ctx, launchDirectory)
	if err != nil {
		return storage.AgentOrigin{}, fmt.Errorf("resolve agent checkout: %w", err)
	}
	current, err := projectidentity.Resolve(ctx, currentDirectory)
	if err != nil {
		return storage.AgentOrigin{}, fmt.Errorf("resolve current checkout: %w", err)
	}
	if launch.Directory != current.Directory {
		return storage.AgentOrigin{}, fmt.Errorf("message publication requires the agent's launch checkout")
	}
	return *origin, nil
}
