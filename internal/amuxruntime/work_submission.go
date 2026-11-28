package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/worksubmission"
	"github.com/urfave/cli/v3"
)

func submissionPreferencesPath(override, configuredEnv string) (string, error) {
	return worksubmission.OptionalPreferencesPath(override, configuredEnv)
}

func runWorkSubmissionSettings(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	flags := commandFlags("amux work-submission", stderr)
	pathOverride := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "work-preferences", Value: "", Usage: "work preference JSON path (default: user config directory)", Destination: pathOverride})
	mode := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "mode", Value: "", Usage: "submission mode: commit_and_push or pull_request", Destination: mode})
	branchPrefix := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "branch-prefix", Value: "", Usage: "prefix used for pull request branches", Destination: branchPrefix})
	if hasTaskHelp(args) {
		_, _ = fmt.Fprintln(stderr, "usage: amux work-submission [--work-preferences PATH] show | set [--mode commit_and_push|pull_request] [--branch-prefix PREFIX]")
		return nil
	}
	pos, err := parseOperation(context.Background(), flags, args)
	if err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if len(pos) != 1 || (pos[0] != "show" && pos[0] != "set") {
		return fmt.Errorf("usage: amux work-submission [--work-preferences PATH] show | set [--mode commit_and_push|pull_request] [--branch-prefix PREFIX]")
	}
	path, err := worksubmission.PreferencesPath(*pathOverride, getenv("AMUX_WORK_SUBMISSION_PREFERENCES"))
	if err != nil {
		return err
	}
	switch pos[0] {
	case "show":
		preferences, err := worksubmission.LoadPreferences(path)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(worksubmission.PreferencesDocument{WorkSubmission: &preferences})
	case "set":
		if !flags.IsSet("mode") && !flags.IsSet("branch-prefix") {
			return fmt.Errorf("set requires --mode or --branch-prefix")
		}
		var change worksubmission.PreferencesChange
		if flags.IsSet("mode") {
			selected := worksubmission.Mode(*mode)
			change.Mode = &selected
		}
		if flags.IsSet("branch-prefix") {
			change.BranchPrefix = branchPrefix
		}
		preferences, err := worksubmission.UpdatePreferences(path, change)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(worksubmission.PreferencesDocument{WorkSubmission: &preferences})
	default:
		return fmt.Errorf("unsupported work submission settings operation")
	}
}

func runTaskSubmitWork(ctx context.Context, client *gatewayclient.Client, project string, goal *string, origin *agent.Origin, args []string, preferencesPath string, stdout, stderr io.Writer) error {
	flags := commandFlags("tasks submit-work", stderr)
	directory := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "directory", Value: "", Usage: "explicit checkout directory to submit from (required)", Destination: directory})
	resume := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "resume-commit", Value: "", Usage: "publish this existing task commit without committing files again", Destination: resume})
	pathsFile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "paths-file", Value: "", Usage: "JSON array of exact changed paths, for lists too large for command-line arguments", Destination: pathsFile})
	pos, err := parseOperation(ctx, flags, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 || *directory == "" {
		return fmt.Errorf("usage: tasks submit-work --directory CHECKOUT [--resume-commit SHA] [--paths-file JSON] TASK_UUID MESSAGE [PATHS...]")
	}
	if origin == nil {
		return fmt.Errorf("tasks submit-work requires a complete native agent origin")
	}
	if err := worksubmission.ValidateRoot(*directory); err != nil {
		return err
	}
	receipt, err := worksubmission.SubmitTask(ctx, client, worksubmission.Input{
		Project: project, Root: *directory, TaskID: pos[0], Message: pos[1], Paths: pos[2:], PathsFile: *pathsFile,
		ResumeCommit: *resume, Origin: origin, Goal: goal,
	}, preferencesPath)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(receipt)
}

func runSubmitWorkAlias(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	flags := commandFlags("amux submit-work", stderr)
	serverURL := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "server", Value: "", Usage: "gateway server URL (required unless --enrolled-profile is used)", Destination: serverURL})
	enrolledProfile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "enrolled gateway credential manifest", Destination: enrolledProfile})
	project := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "project", Value: "", Usage: "gateway project identifier (required)", Destination: project})
	keyEnv := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "api-key-env", Value: "GATEWAY_API_KEY", Usage: "environment variable containing the gateway API credential", Destination: keyEnv})
	goalFlag := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "goal", Value: "", Usage: "select a task goal", Destination: goalFlag})
	session := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "session", Value: "", Usage: "native agent session UUID", Destination: session})
	instance := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "instance", Value: "", Usage: "native agent instance UUID", Destination: instance})
	provider := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "provider", Value: "", Usage: "native provider", Destination: provider})
	hostOS := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "os", Usage: "native agent OS", Destination: hostOS})
	directory := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "directory", Value: "", Usage: "explicit checkout directory to submit from (required)", Destination: directory})
	resume := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "resume-commit", Value: "", Usage: "publish this existing task commit without committing files again", Destination: resume})
	pathsFile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "paths-file", Value: "", Usage: "JSON array of exact changed paths, for lists too large for command-line arguments", Destination: pathsFile})
	preferencesOverride := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "work-preferences", Value: "", Usage: "work preference JSON path (default: user config directory)", Destination: preferencesOverride})
	if hasTaskHelp(args) {
		_, _ = fmt.Fprintln(stderr, "usage: amux submit-work (--server URL | --enrolled-profile PATH) --project IDENT --directory CHECKOUT --session UUID --instance UUID --provider NAME --os OS [--paths-file JSON] [--resume-commit SHA] TASK_UUID MESSAGE [PATHS...]")
		return nil
	}
	pos, err := parseOperation(ctx, flags, args)
	if err != nil {
		return err
	}
	if (*serverURL == "" && *enrolledProfile == "") || *project == "" {
		return fmt.Errorf("--server or --enrolled-profile and --project are required")
	}
	if strings.TrimSpace(*goalFlag) != *goalFlag {
		return fmt.Errorf("--goal must not have surrounding whitespace")
	}
	providedOrigin := *session != "" || *instance != "" || *provider != "" || *hostOS != ""
	if !providedOrigin || *session == "" || *instance == "" || *provider == "" || *hostOS == "" {
		return fmt.Errorf("submit-work requires --session, --instance, --provider, and --os")
	}
	if len(pos) < 2 || *directory == "" {
		return fmt.Errorf("usage: amux submit-work --server URL --project IDENT --directory CHECKOUT --session UUID --instance UUID --provider NAME --os OS TASK_UUID MESSAGE [PATHS...]")
	}
	if err := worksubmission.ValidateRoot(*directory); err != nil {
		return err
	}
	origin, err := agent.NormalizeOrigin(&agent.Origin{SessionID: *session, InstanceID: *instance, Provider: *provider, OS: *hostOS})
	if err != nil {
		return err
	}
	apiKey := ""
	if *enrolledProfile == "" {
		apiKey = getenv(*keyEnv)
	}
	client, _, err := gatewayclient.NewForProfile(*serverURL, *enrolledProfile, apiKey)
	if err != nil {
		return err
	}
	preferencesPath, err := worksubmission.OptionalPreferencesPath(*preferencesOverride, getenv("AMUX_WORK_SUBMISSION_PREFERENCES"))
	if err != nil {
		return err
	}
	var selectedGoal *string
	if flags.IsSet("goal") {
		selectedGoal = goalFlag
	}
	receipt, err := worksubmission.SubmitTask(ctx, client, worksubmission.Input{
		Project: *project, Root: *directory, TaskID: pos[0], Message: pos[1], Paths: pos[2:], PathsFile: *pathsFile,
		ResumeCommit: *resume, Origin: origin, Goal: selectedGoal,
	}, preferencesPath)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(receipt)
}
