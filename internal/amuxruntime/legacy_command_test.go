package amuxruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/update"
	"github.com/urfave/cli/v3"
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	if len(args) > 0 && args[0] == "--generate-client-config" {
		return generateClientConfig(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "--reload-config" {
		if len(args) == 3 && args[1] == "--config" {
			return requestClientReloadAt(ctx, args[2], stdout)
		}
		if len(args) != 1 {
			return fmt.Errorf("usage: amux --reload-config [--config PATH]")
		}
		return requestClientReload(ctx, stdout)
	}
	if len(args) > 0 && args[0] == "--update" {
		if len(args) != 1 {
			return fmt.Errorf("usage: amux --update")
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		archive, info, err := update.Stage(ctx, executable)
		if err != nil {
			return err
		}
		if !info.Available {
			_, err = fmt.Fprintf(stdout, "amux %s is up to date.\n", info.Version)
			return err
		}
		if err := update.StartApply(executable, archive, nil, nil); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Installing verified amux %s after this command exits.\n", info.Version)
		return err
	}
	if len(args) > 0 && args[0] == "--apply-update" {
		if len(args) < 2 {
			return fmt.Errorf("usage: amux --apply-update ARCHIVE [--restart-desktop --wait-pid PID ... -- ARGS...]")
		}
		restartArgs := []string(nil)
		waitPIDs := []int(nil)
		index := 2
		if index < len(args) && args[index] == "--restart-desktop" {
			restartArgs = []string{}
			index++
		}
		for index < len(args) && args[index] == "--wait-pid" {
			if index+1 >= len(args) {
				return fmt.Errorf("--wait-pid requires a process id")
			}
			pid, err := strconv.Atoi(args[index+1])
			if err != nil || pid <= 0 {
				return fmt.Errorf("invalid --wait-pid")
			}
			waitPIDs = append(waitPIDs, pid)
			index += 2
		}
		if index < len(args) {
			if args[index] != "--" || restartArgs == nil {
				return fmt.Errorf("unexpected --apply-update arguments")
			}
			restartArgs = append(restartArgs, args[index+1:]...)
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if err := update.StartApply(executable, args[1], waitPIDs, restartArgs); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "Update helper started. The previous desktop and CLI will close before the verified package is installed.")
		return err
	}
	if len(args) > 0 && args[0] == "--finish-update" {
		if err := update.FinishHelper(args); err != nil {
			return err
		}
		return nil
	}
	if len(args) >= 2 && args[0] == "client" && args[1] == "enroll" {
		return runClientEnroll(ctx, args[2:], os.Stdin, stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "client" && args[1] == "run" {
		return runHeadlessClient(ctx, args[2:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "client" && args[1] == "renew" {
		return runClientRenew(ctx, args[2:], stderr)
	}
	if len(args) >= 2 && args[0] == "server" && args[1] == "peering-key" {
		return runServerPeeringKey(ctx, args[2:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "messages" {
		return runMessages(ctx, args[1:], stdout, stderr, getenv)
	}
	if len(args) >= 1 && args[0] == "tasks" {
		return runTasks(ctx, args[1:], stdout, stderr, getenv)
	}
	if len(args) >= 1 && args[0] == "submit-work" {
		return runSubmitWorkAlias(ctx, args[1:], stdout, stderr, getenv)
	}
	if len(args) >= 1 && args[0] == "work-submission" {
		return runWorkSubmissionSettings(args[1:], stdout, stderr, getenv)
	}
	if len(args) >= 1 && args[0] == "mcp" {
		return runMCPIO(ctx, args[1:], os.Stdin, stdout, stderr)
	}
	if len(args) > 0 && (args[0] == "-add-project" || args[0] == "--add-project") {
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("usage: amux -add-project PATH --state PATH --enrolled-profile PATH [--provider NAME] [--slots N]")
		}
		poolArgs := append([]string{"--directory", args[1]}, args[2:]...)
		return runPoolMode(ctx, poolArgs, stdout, stderr, getenv, true)
	}
	if len(args) >= 2 && args[0] == "worker" && args[1] == "pool" {
		return runPool(ctx, args[2:], stdout, stderr, getenv)
	}
	if len(args) >= 2 && args[0] == "worker" && args[1] == "run" {
		return runWorker(ctx, args[2:], stdout, stderr, getenv)
	}
	generateServerConfig := len(args) > 0 && args[0] == "--generate-server-config"
	if generateServerConfig && len(args) != 1 {
		return fmt.Errorf("usage: amux --generate-server-config")
	}
	if !generateServerConfig && (len(args) < 2 || args[0] != "server" || args[1] != "run") {
		return fmt.Errorf("usage: amux --generate-server-config | amux server run (--config PATH | --database PATH | --profile DIR) [--listen 127.0.0.1:7913] | amux server peering-key [--profile DIR] [--copy] | amux worker run --state PATH --server URL --project IDENT --task UUID --directory PATH | amux worker pool --state PATH --server URL --project IDENT --directory PATH | amux tasks --server URL --project IDENT [actor options] OPERATION | amux messages [--gateway-url URL] [--project IDENT] list|get|send | amux submit-work [common task options] --directory PATH TASK MESSAGE PATHS... | amux work-submission show|set")
	}
	flags := commandFlags("amux server run", stderr)
	database := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "database", Value: "", Usage: "durable SQLite database path (required)", Destination: database})
	profile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "profile", Value: "", Usage: "explicit Rust-compatible server profile directory", Destination: profile})
	secureInstance := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "secure-instance-id", Value: "", Usage: "enable enrolled-certificate TLS using this stable local administrator instance UUID", Destination: secureInstance})
	discoveryBind := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "discovery-bind", Value: "", Usage: "public enrollment discovery address (defaults to secure listen host on port 24181)", Destination: discoveryBind})
	var profileTLSNames []string
	flags.Flags = append(flags.Flags, &cli.StringSliceFlag{Name: "profile-tls-name", Usage: "DNS or IP name for the profile-managed TLS certificate (repeatable)", Destination: &profileTLSNames})
	listen := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "listen", Value: "127.0.0.1:7913", Usage: "TCP address to listen on", Destination: listen})
	keyEnv := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "api-key-env", Value: "GATEWAY_API_KEY", Usage: "environment variable containing the server API credential", Destination: keyEnv})
	certPath := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "tls-cert", Value: "", Usage: "TLS server certificate PEM file", Destination: certPath})
	keyPath := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "tls-key", Value: "", Usage: "TLS server private key PEM file", Destination: keyPath})
	retentionDays := new(uint64)
	flags.Flags = append(flags.Flags, &cli.Uint64Flag{Name: "message-retention-days", Value: 30, Usage: "message history retention in days", Destination: retentionDays})
	botRetentionDays := new(uint64)
	flags.Flags = append(flags.Flags, &cli.Uint64Flag{Name: "bot-message-retention-days", Value: 7, Usage: "bot message history retention in days", Destination: botRetentionDays})
	discordTokenEnv := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "discord-token-env", Value: "", Usage: "enable Discord messaging and inbound room ingestion using the token in this environment variable", Destination: discordTokenEnv})
	discordGuildID := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "discord-guild-id", Value: "", Usage: "Discord guild ID (required with --discord-token-env)", Destination: discordGuildID})
	discordCategoryID := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "discord-category-id", Value: "", Usage: "optional Discord category ID for newly created rooms", Destination: discordCategoryID})
	if generateServerConfig {
		return writeServerConfig(stdout)
	}
	configPath := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "config", Value: "", Usage: "YAML server configuration file", Destination: configPath})
	if err := parseCommand(ctx, flags, args[2:]); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 {
		return fmt.Errorf("unexpected server arguments")
	}
	if *configPath == "" {
		dir, err := os.MkdirTemp("", "amux-server-test-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		*configPath = filepath.Join(dir, "server.yml")
		if err := config.SaveServer(*configPath, serverFromFlags(flags)); err != nil {
			return err
		}
	}
	if err := config.LoadServer(*configPath); err != nil {
		return err
	}
	return server.Start(ctx, stdout, stderr)
}
