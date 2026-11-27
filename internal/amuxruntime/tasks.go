package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/taskplatform"
	"github.com/urfave/cli/v3"
)

func runTasks(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	global := commandFlags("amux tasks", stderr)
	stopAfterOperation := 1
	global.StopOnNthArg = &stopAfterOperation
	serverURL := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "server", Value: "", Usage: "gateway server URL (required)", Destination: serverURL})
	gatewayURL := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "gateway-url", Value: "", Usage: "alias for --server", Destination: gatewayURL})
	project := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "project", Value: "", Usage: "gateway project identifier (required)", Destination: project})
	keyEnv := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "api-key-env", Value: "GATEWAY_API_KEY", Usage: "credential environment variable (falls back to AMUX_GATEWAY_KEY unless explicitly set)", Destination: keyEnv})
	enrolledProfile := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "enrolled gateway credential manifest", Destination: enrolledProfile})
	workPreferences := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "work-preferences", Value: "", Usage: "work preference JSON path (default: user config directory)", Destination: workPreferences})
	goalFlag := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "goal", Value: "", Usage: "select a task goal", Destination: goalFlag})
	allGoals := new(bool)
	global.Flags = append(global.Flags, &cli.BoolFlag{Name: "all-goals", Value: false, Usage: "list tasks across all goals (list only)", Destination: allGoals})
	session := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "session", Value: "", Usage: "native agent session UUID", Destination: session})
	instance := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "instance", Value: "", Usage: "native agent instance UUID", Destination: instance})
	provider := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "provider", Value: "", Usage: "native provider: codex, claude, or antigravity", Destination: provider})
	hostOS := new(string)
	global.Flags = append(global.Flags, &cli.StringFlag{Name: "os", Value: "", Usage: "native agent OS: linux, windows, or macos", Destination: hostOS})
	human := new(bool)
	global.Flags = append(global.Flags, &cli.BoolFlag{Name: "human", Value: false, Usage: "perform the operation as a human", Destination: human})
	jsonOutput := new(bool)
	global.Flags = append(global.Flags, &cli.BoolFlag{Name: "json", Value: false, Usage: "write JSON output", Destination: jsonOutput})
	if err := parseCommand(ctx, global, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	operation, opArgs := "", []string(nil)
	if global.Args().Len() > 0 {
		operation, opArgs = global.Args().First(), global.Args().Tail()
	}
	if hasTaskHelp(opArgs) {
		return writeTaskHelp(stderr, operation)
	}
	serverWasSet := global.IsSet("server")
	gatewayURLWasSet := global.IsSet("gateway-url")
	keyEnvWasSet := global.IsSet("api-key-env")
	if serverWasSet && gatewayURLWasSet && *serverURL != *gatewayURL {
		return fmt.Errorf("--server and --gateway-url conflict; supply one URL or matching values")
	}
	selectedServerURL := *serverURL
	if gatewayURLWasSet {
		selectedServerURL = *gatewayURL
	}
	apiKey := ""
	if *enrolledProfile == "" {
		apiKey = getenv(*keyEnv)
		if apiKey == "" && !keyEnvWasSet {
			apiKey = getenv("AMUX_GATEWAY_KEY")
		}
	}
	if operation == "report" {
		return runTaskReport(ctx, opArgs, *jsonOutput, selectedServerURL, *project, apiKey, *enrolledProfile, stdout, stderr)
	}
	goalWasSet := global.IsSet("goal")
	if (selectedServerURL == "" && *enrolledProfile == "") || *project == "" {
		return fmt.Errorf("--server and --project are required (--gateway-url is an alias for --server)")
	}
	if global.Args().Len() == 0 {
		return fmt.Errorf("task operation is required")
	}
	goal := strings.TrimSpace(*goalFlag)
	if goal != *goalFlag {
		return fmt.Errorf("--goal must not have surrounding whitespace")
	}
	var selectedGoal *string
	if goalWasSet {
		selectedGoal = &goal
	}
	if *allGoals && goalWasSet {
		return fmt.Errorf("--goal and --all-goals are mutually exclusive")
	}
	if *allGoals && global.Args().First() != "list" {
		return fmt.Errorf("--all-goals is supported for readonly list only")
	}
	providedOrigin := *session != "" || *instance != "" || *provider != "" || *hostOS != ""
	if *human && providedOrigin {
		return fmt.Errorf("--human and native origin flags are mutually exclusive")
	}
	var origin *storage.AgentOrigin
	if providedOrigin {
		if *session == "" || *instance == "" || *provider == "" || *hostOS == "" {
			return fmt.Errorf("native origin requires --session, --instance, --provider, and --os together")
		}
		origin = &storage.AgentOrigin{SessionID: *session, InstanceID: *instance, Provider: *provider, OS: *hostOS}
		canonical, err := storage.NormalizeOrigin(origin)
		if err != nil {
			return err
		}
		origin = canonical
	}
	readonly := operation == "list" || operation == "get" || operation == "cancellation" || operation == "agents"
	if (operation == "claim" || operation == "started") && origin == nil {
		return fmt.Errorf("tasks %s requires a native agent origin", operation)
	}
	if operation == "watch" && origin == nil {
		return fmt.Errorf("tasks watch requires a complete native agent origin")
	}
	if operation == "update-delegation" && origin == nil {
		return fmt.Errorf("tasks update-delegation requires a complete native agent origin")
	}
	if operation == "delegate" && origin == nil {
		return fmt.Errorf("tasks delegate requires a complete native agent origin")
	}
	if operation == "cancel" && (*human || origin == nil) {
		return fmt.Errorf("tasks cancel requires a complete native agent origin and does not support --human")
	}
	if operation == "reassign" && origin == nil {
		return fmt.Errorf("tasks reassign requires a complete native agent origin")
	}
	if !readonly && origin == nil && !*human {
		return fmt.Errorf("task writes require a complete native origin (--session, --instance, --provider, --os) or --human")
	}
	client, _, err := gatewayClientForProfile(selectedServerURL, *enrolledProfile, apiKey)
	if err != nil {
		return err
	}
	if operation == "set-os" && (origin == nil || *human) {
		return fmt.Errorf("tasks set-os requires a complete native agent origin and does not support --human")
	}
	var value any
	goalPtr := selectedGoal
	switch operation {
	case "agents":
		return runTaskAgents(ctx, client, *project, opArgs, stdout, stderr)
	case "list":
		fs := commandFlags("tasks list", stderr)
		status := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "status", Value: "", Usage: "comma-separated task statuses", Destination: status})
		includeStale := new(bool)
		fs.Flags = append(fs.Flags, &cli.BoolFlag{Name: "include-stale", Value: false, Usage: "include older completed tasks", Destination: includeStale})
		pos, err := parseOperation(ctx, fs, opArgs)
		if err != nil {
			return err
		}
		if len(pos) != 0 {
			return fmt.Errorf("tasks list does not accept positional arguments")
		}
		statuses := []string(nil)
		if *status != "" {
			statuses = strings.Split(*status, ",")
		}
		value, err = client.TaskList(ctx, *project, gatewayclient.TaskListOptions{Statuses: statuses, IncludeStale: *includeStale, AllGoals: *allGoals}, origin, goalPtr)
	case "get":
		fs := commandFlags("tasks get", stderr)
		comments := new(uint64)
		fs.Flags = append(fs.Flags, &cli.Uint64Flag{Name: "comments", Value: 50, Usage: "newest comments to include", Destination: comments})
		pos, e := parseOperation(ctx, fs, opArgs)
		if e != nil {
			return e
		}
		if len(pos) != 1 {
			return fmt.Errorf("usage: tasks get ID [--comments N]")
		}
		id, e := canonicalTaskID(pos[0])
		if e != nil {
			return e
		}
		value, err = client.TaskRead(ctx, *project, id, *comments)
	case "cancellation":
		pos, e := parseOperation(ctx, commandFlags("tasks cancellation", stderr), opArgs)
		if e != nil {
			return e
		}
		if len(pos) != 1 {
			return fmt.Errorf("usage: tasks cancellation TASK_ID")
		}
		id, e := canonicalTaskID(pos[0])
		if e != nil {
			return e
		}
		value, err = client.TaskCancellation(ctx, *project, id)
	case "create":
		fs := commandFlags("tasks create", stderr)
		parent := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "parent", Value: "", Usage: "parent task UUID", Destination: parent})
		rootReason := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "root-reason", Value: "", Usage: "user-directed or principle", Destination: rootReason})
		spec := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "specification", Value: "", Usage: "task specification", Destination: spec})
		details := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "details", Value: "", Usage: "alias for --specification", Destination: details})
		taskOS := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "task-os", Value: "", Usage: "required task OS (distinct from global native --os)", Destination: taskOS, Aliases: []string{"os"}})

		reviewFinding := new(bool)
		fs.Flags = append(fs.Flags, &cli.BoolFlag{Name: "review-finding", Value: false, Usage: "create an unassigned review finding", Destination: reviewFinding})
		notifyResults := new(bool)
		fs.Flags = append(fs.Flags, &cli.BoolFlag{Name: "notify-results", Value: false, Usage: "subscribe this execution to task results", Destination: notifyResults})
		pos, err := parseOperation(ctx, fs, opArgs)
		if err != nil {
			return err
		}
		if *reviewFinding {
			return fmt.Errorf("--review-finding is unsupported until server unassigned-routing semantics are ported")
		}
		if *spec != "" && *details != "" {
			return fmt.Errorf("--specification and --details are aliases; supply only one")
		}
		if *spec == "" {
			spec = details
		}
		if len(pos) != 1 {
			return fmt.Errorf("usage: tasks create TITLE (--parent UUID | --root-reason user-directed|principle --specification TEXT)")
		}
		if (*parent == "") == (*rootReason == "") {
			return fmt.Errorf("provide exactly one of --parent or --root-reason")
		}
		if *rootReason != "" && *rootReason != "user-directed" && *rootReason != "principle" {
			return fmt.Errorf("--root-reason must be user-directed or principle")
		}
		if *rootReason != "" && *spec == "" {
			return fmt.Errorf("root creation requires --specification")
		}
		input := gatewayclient.TaskCreateInput{Title: pos[0], Specification: optionalString(*spec)}
		if *notifyResults {
			if origin == nil {
				return fmt.Errorf("--notify-results requires a native agent origin")
			}
			input.Requester, err = client.RequesterTarget(ctx, *project, *origin)
			if err != nil {
				return fmt.Errorf("resolve task requester: %w", err)
			}
		}
		if *parent != "" {
			canonical, err := canonicalTaskID(*parent)
			if err != nil {
				return err
			}
			input.ParentID = &canonical
		}
		if *rootReason != "" {
			input.RootReason = rootReason
		}
		if *taskOS != "" {
			input.Labels, err = taskplatform.LabelsWithOS(nil, *taskOS)
			if err != nil {
				return err
			}
		}
		value, err = client.TaskCreate(ctx, *project, input, origin, goalPtr)
	case "delegate":
		fs := commandFlags("tasks delegate", stderr)
		parent := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "parent", Value: "", Usage: "source parent task UUID", Destination: parent})
		targetProject := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "target-project", Value: "", Usage: "owning target project identifier", Destination: targetProject})
		specification := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "specification", Value: "", Usage: "delegation context and acceptance criteria", Destination: specification})
		notifyResults := new(bool)
		fs.Flags = append(fs.Flags, &cli.BoolFlag{Name: "notify-results", Value: false, Usage: "subscribe this execution to delegation results", Destination: notifyResults})
		pos, parseErr := parseOperation(ctx, fs, opArgs)
		if parseErr != nil {
			return parseErr
		}
		if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" || *parent == "" || *targetProject == "" {
			return fmt.Errorf("usage: tasks delegate TITLE --parent UUID --target-project IDENT [--specification TEXT]")
		}
		parentID, parseErr := canonicalTaskID(*parent)
		if parseErr != nil {
			return fmt.Errorf("--parent: %w", parseErr)
		}
		if err = client.RequireTaskHierarchy(ctx); err != nil {
			return err
		}
		input := gatewayclient.TaskDelegateInput{
			TargetProject: *targetProject, Title: pos[0], ParentID: &parentID,
			Specification: optionalString(*specification),
		}
		if *notifyResults {
			input.Requester, err = client.RequesterTarget(ctx, *project, *origin)
			if err != nil {
				return fmt.Errorf("resolve task requester: %w", err)
			}
		}
		value, err = client.DelegateTask(ctx, *project, input, origin, goalPtr)
	case "watch":
		fs := commandFlags("tasks watch", stderr)
		clear := new(bool)
		fs.Flags = append(fs.Flags, &cli.BoolFlag{Name: "clear", Value: false, Usage: "remove this execution's task result subscription", Destination: clear})
		pos, parseErr := parseOperation(ctx, fs, opArgs)
		if parseErr != nil {
			return parseErr
		}
		if len(pos) != 1 {
			return fmt.Errorf("usage: tasks watch TASK_ID [--clear]")
		}
		id, parseErr := canonicalTaskID(pos[0])
		if parseErr != nil {
			return parseErr
		}
		var requester *storage.RegisteredTargetIdentity
		if !*clear {
			requester, err = client.RequesterTarget(ctx, *project, *origin)
			if err != nil {
				return fmt.Errorf("resolve task requester: %w", err)
			}
		}
		value, err = client.SetTaskRequester(ctx, *project, id, requester, origin)
	case "update-delegation":
		fs := commandFlags("tasks update-delegation", stderr)
		title := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "title", Value: "", Usage: "new shared title (literal)", Destination: title})
		description := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "description", Value: "", Usage: "new shared description (literal)", Destination: description})
		specification := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "specification", Value: "", Usage: "new shared specification (literal; takes precedence over --details)", Destination: specification})
		details := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "details", Value: "", Usage: "alias for --specification (literal)", Destination: details})
		labelsJSON := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "labels", Value: "", Usage: "replacement shared labels as a JSON string array (use [] to clear)", Destination: labelsJSON})
		pos, parseErr := parseOperation(ctx, fs, opArgs)
		if parseErr != nil {
			return parseErr
		}
		if len(pos) != 1 {
			return fmt.Errorf("usage: tasks update-delegation TASK_ID [--title TEXT] [--description TEXT] [--specification TEXT | --details TEXT] [--labels JSON_ARRAY]")
		}
		id, parseErr := canonicalTaskID(pos[0])
		if parseErr != nil {
			return parseErr
		}
		update := storage.TaskDelegationUpdate{}
		if fs.IsSet("title") {
			update.Title = title
		}
		if fs.IsSet("description") {
			update.Description = description
		}
		if fs.IsSet("specification") {
			update.Specification = specification
		} else if fs.IsSet("details") {
			update.Details = details
		}
		if fs.IsSet("labels") {
			if err = json.Unmarshal([]byte(*labelsJSON), &update.Labels); err != nil {
				return fmt.Errorf("--labels must be a JSON string array")
			}
		}
		if update.Title == nil && update.Description == nil && update.Details == nil && update.Specification == nil && update.Labels == nil {
			return fmt.Errorf("tasks update-delegation requires at least one metadata flag")
		}
		value, err = client.UpdateTaskDelegation(ctx, *project, id, update, origin)
	case "reassign":
		fs := commandFlags("tasks reassign", stderr)
		recipient := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "to-agent", Value: "", Usage: "recipient agent session UUID", Destination: recipient, Aliases: []string{"to"}})

		pos, parseErr := parseOperation(ctx, fs, opArgs)
		if parseErr != nil {
			return parseErr
		}
		if len(pos) != 1 || *recipient == "" {
			return fmt.Errorf("usage: tasks reassign TASK_ID --to SESSION_UUID")
		}
		id, parseErr := canonicalTaskID(pos[0])
		if parseErr != nil {
			return parseErr
		}
		if err = validateTaskCheckout(ctx, client, *project); err != nil {
			return err
		}
		value, err = client.ReassignTask(ctx, *project, id, *recipient, *origin)
	case "cancel":
		value, err = runTaskCancel(ctx, client, *project, opArgs, origin, stderr)
	case "submit-work":
		preferencesPath, pathErr := submissionPreferencesPath(*workPreferences, getenv("AMUX_WORK_SUBMISSION_PREFERENCES"))
		if pathErr != nil {
			return taskError(pathErr)
		}
		if err := runTaskSubmitWork(ctx, client, *project, goalPtr, origin, opArgs, preferencesPath, stdout, stderr); err != nil {
			return taskError(err)
		}
		return nil
	case "claim", "started", "rank", "set-os", "comment", "user-review", "review", "done", "release", "confirm-mutation", "set-changeset":
		value, err = runTaskWrite(ctx, client, *project, operation, opArgs, origin, goalPtr, stderr)
	case "report":
		return fmt.Errorf("tasks %s is unsupported in the Go CLI", operation)
	default:
		return fmt.Errorf("unknown tasks operation %q", operation)
	}
	if err != nil {
		return taskError(err)
	}
	return json.NewEncoder(stdout).Encode(value)
}

func hasTaskHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func writeTaskHelp(w io.Writer, operation string) error {
	usage := map[string]string{
		"report":            "usage: amux tasks [--server URL|--enrolled-profile PATH] [--project IDENT] [--json] report FROM..TO [--offline] (or report --range FROM..TO [--format human|json])",
		"list":              "usage: amux tasks [common options] list [--status STATUS,...] [--include-stale] [--all-goals]",
		"agents":            "usage: amux tasks [common options] agents [--os linux|windows|macos]",
		"get":               "usage: amux tasks [common options] get TASK_UUID [--comments N]",
		"cancel":            "usage: amux tasks [--server|--gateway-url URL] [native origin options] cancel TASK_ID --reason TEXT [--changeset JSON]",
		"cancellation":      "usage: amux tasks [common options] cancellation TASK_ID",
		"create":            "usage: amux tasks [common options] create TITLE (--parent UUID | --root-reason user-directed|principle --specification TEXT) [--os linux|windows|macos] [--notify-results]",
		"delegate":          "usage: amux tasks [native origin options] delegate TITLE --parent UUID --target-project IDENT [--specification TEXT] [--notify-results]",
		"reassign":          "usage: amux tasks [native origin options] reassign TASK_ID --to-agent SESSION_UUID (alias: --to)",
		"update-delegation": "usage: amux tasks [native origin options] update-delegation TASK_ID [--title TEXT] [--description TEXT] [--specification TEXT | --details TEXT] [--labels JSON_ARRAY]",
		"watch":             "usage: amux tasks [native origin options] watch TASK_ID [--clear]",
		"claim":             "usage: amux tasks [common options] claim TASK_UUID",
		"started":           "usage: amux tasks [native origin options] started TASK_UUID",
		"rank":              "usage: amux tasks [common options] rank TASK_UUID SIGNED_RANK",
		"set-os":            "usage: amux tasks [common options] set-os TASK_UUID (--os linux|windows|macos | --clear)",
		"comment":           "usage: amux tasks [common options] comment TASK_UUID CONTENT...",
		"user-review":       "usage: amux tasks [common options] user-review TASK_UUID ACTION [--changeset JSON] (alias: review)",
		"review":            "usage: amux tasks [common options] user-review TASK_UUID ACTION [--changeset JSON]",
		"done":              "usage: amux tasks [common options] done TASK_UUID [--changeset JSON]",
		"submit-work":       "usage: amux tasks [common options] submit-work --directory CHECKOUT [--resume-commit SHA] [--paths-file JSON] TASK_UUID MESSAGE [PATHS...]",
		"release":           "usage: amux tasks [common options] release TASK_UUID [--changeset JSON]",
		"confirm-mutation":  "usage: amux tasks [common options] confirm-mutation TASK_UUID",
		"set-changeset":     "usage: amux tasks [common options] set-changeset TASK_UUID JSON_NULL_OR_ARRAY",
	}
	line, ok := usage[operation]
	if !ok {
		line = "usage: amux tasks [--server URL|--gateway-url URL|--enrolled-profile PATH --project IDENT] [common options] OPERATION [arguments]"
	}
	_, err := fmt.Fprintln(w, line)
	if err != nil {
		return err
	}
	if operation == "list" {
		_, err = fmt.Fprintln(w, "common options: --server URL (alias: --gateway-url URL) or --enrolled-profile PATH; --project IDENT [--api-key-env NAME] [--goal GOAL | --all-goals]; list filters include todo, in_progress and stalled by default")
	}
	return err
}

func validateTaskCheckout(ctx context.Context, client *gatewayclient.Client, project string) error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve current checkout: %w", err)
	}
	identity, err := projectidentity.Resolve(ctx, root)
	if err != nil {
		return fmt.Errorf("resolve current checkout identity: %w", err)
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range projects {
		if candidate.Ident == project && checkoutMatchesProject(candidate, identity) {
			return nil
		}
	}
	return fmt.Errorf("current checkout is not bound to gateway project %q", project)
}

func runTaskCancel(ctx context.Context, client *gatewayclient.Client, project string, args []string, origin *storage.AgentOrigin, stderr io.Writer) (*storage.CancelResponse, error) {
	if origin == nil {
		return nil, fmt.Errorf("tasks cancel requires a complete native agent origin and does not support --human")
	}
	fs := commandFlags("tasks cancel", stderr)
	reason := new(string)
	fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "reason", Value: "", Usage: "cancellation reason", Destination: reason})
	changeset := new(string)
	fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "changeset", Value: "", Usage: "JSON array of changed files or null", Destination: changeset})
	pos, err := parseOperation(ctx, fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 || strings.TrimSpace(*reason) == "" || !fs.IsSet("reason") {
		return nil, fmt.Errorf("usage: tasks cancel TASK_ID --reason TEXT [--changeset JSON]")
	}
	id, err := canonicalTaskID(pos[0])
	if err != nil {
		return nil, err
	}
	change, err := parseOptionalChangeset(changeset, fs.IsSet("changeset"))
	if err != nil {
		return nil, err
	}
	return client.TaskCancel(ctx, project, id, *reason, change, origin)
}

func canonicalTaskID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return "", fmt.Errorf("task ID must be a non-zero UUID")
	}
	return id.String(), nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func runTaskWrite(ctx context.Context, client *gatewayclient.Client, project, operation string, args []string, origin *storage.AgentOrigin, goal *string, stderr io.Writer) (json.RawMessage, error) {
	fs := commandFlags("tasks "+operation, stderr)
	switch operation {
	case "claim", "started", "done", "release", "confirm-mutation":
		var changeset *string
		if operation == "done" || operation == "release" {
			changeset = new(string)
			fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "changeset", Value: "", Usage: "JSON array of changed files or null", Destination: changeset})
		}
		pos, err := parseOperation(ctx, fs, args)
		if err != nil {
			return nil, err
		}
		changesetProvided := fs.IsSet("changeset")
		if len(pos) != 1 {
			return nil, fmt.Errorf("tasks %s requires TASK_ID", operation)
		}
		id, err := canonicalTaskID(pos[0])
		if err != nil {
			return nil, err
		}
		switch operation {
		case "started":
			raw, err := client.TaskRead(ctx, project, id, 0)
			if err != nil {
				return nil, err
			}
			var task storage.Task
			if err := json.Unmarshal(raw, &task); err != nil {
				return nil, fmt.Errorf("invalid task response")
			}
			if !storage.IsActiveTaskStatus(task.Status) || task.OwnerOrigin == nil || *task.OwnerOrigin != *origin {
				return nil, fmt.Errorf("started requires an active task owned by this exact native session and instance")
			}
			return client.TaskComment(ctx, project, id, fmt.Sprintf("Agent reports execution started: session=%s instance=%s; recorded as a comment, not an atomic gateway lifecycle transition", origin.SessionID, origin.InstanceID), origin)
		case "claim":
			raw, err := client.TaskRead(ctx, project, id, 0)
			if err != nil {
				return nil, err
			}
			var task storage.Task
			if err := json.Unmarshal(raw, &task); err != nil {
				return nil, fmt.Errorf("invalid task response")
			}
			if !taskplatform.Matches(task.Labels, origin.OS) {
				return nil, fmt.Errorf("task requires another operating system")
			}
			return client.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{Status: "in_progress"}, origin, goal)
		case "done":
			if _, err := client.TaskRead(ctx, project, id, 0); err != nil {
				return nil, err
			}
			change, err := parseOptionalChangeset(changeset, changesetProvided)
			if err != nil {
				return nil, err
			}
			return client.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{Status: "done", Changeset: change}, origin, goal)
		case "release":
			if origin != nil {
				raw, err := client.TaskRead(ctx, project, id, 0)
				if err != nil {
					return nil, err
				}
				var task storage.Task
				if json.Unmarshal(raw, &task) != nil {
					return nil, fmt.Errorf("invalid task response")
				}
				if !storage.IsActiveTaskStatus(task.Status) || task.OwnerOrigin == nil || *task.OwnerOrigin != *origin {
					return nil, fmt.Errorf("only the exact owning execution may release this active task; no status update was sent")
				}
			}
			change, err := parseOptionalChangeset(changeset, changesetProvided)
			if err != nil {
				return nil, err
			}
			return client.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{Status: "todo", Changeset: change}, origin, goal)
		case "confirm-mutation":
			taskType := "software"
			return client.TaskConfirm(ctx, project, id, &taskType, nil, origin)
		}
	case "rank":
		rankArgs := args
		if len(args) == 2 && strings.HasPrefix(args[1], "-") {
			if _, err := strconv.ParseInt(args[1], 10, 64); err == nil {
				rankArgs = []string{args[0], "--", args[1]}
			}
		}
		pos, err := parseOperation(ctx, fs, rankArgs)
		if err != nil {
			return nil, err
		}
		if len(pos) != 2 {
			return nil, fmt.Errorf("usage: tasks rank TASK_ID RANK")
		}
		id, err := canonicalTaskID(pos[0])
		if err != nil {
			return nil, err
		}
		v, err := strconv.ParseInt(pos[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("rank must be a signed 64-bit integer")
		}
		return client.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{TaskMetadataUpdate: storage.TaskMetadataUpdate{Rank: &v}}, origin, goal)
	case "set-os":
		osFlag := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "os", Value: "", Usage: "required task OS: linux, windows, or macos", Destination: osFlag})
		clear := new(bool)
		fs.Flags = append(fs.Flags, &cli.BoolFlag{Name: "clear", Value: false, Usage: "remove task OS requirement", Destination: clear})
		pos, err := parseOperation(ctx, fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) != 1 || (*clear == (*osFlag != "")) {
			return nil, fmt.Errorf("usage: tasks set-os TASK_ID (--os linux|windows|macos | --clear)")
		}
		id, err := canonicalTaskID(pos[0])
		if err != nil {
			return nil, err
		}
		if err := validateTaskCheckout(ctx, client, project); err != nil {
			return nil, err
		}
		var requiredOS *string
		if !*clear {
			requiredOS = osFlag
		}
		return client.SetTaskOS(ctx, project, id, requiredOS, *origin, goal)
	case "comment":
		pos, err := parseOperation(ctx, fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) < 2 {
			return nil, fmt.Errorf("usage: tasks comment TASK_ID CONTENT...")
		}
		id, err := canonicalTaskID(pos[0])
		if err != nil {
			return nil, err
		}
		return client.TaskComment(ctx, project, id, strings.Join(pos[1:], " "), origin)
	case "user-review", "review":
		changeset := new(string)
		fs.Flags = append(fs.Flags, &cli.StringFlag{Name: "changeset", Value: "", Usage: "JSON array of changed files or null", Destination: changeset})
		pos, err := parseOperation(ctx, fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) != 2 {
			return nil, fmt.Errorf("usage: tasks user-review TASK_ID ACTION [--changeset JSON]")
		}
		id, err := canonicalTaskID(pos[0])
		if err != nil {
			return nil, err
		}
		change, err := parseOptionalChangeset(changeset, fs.IsSet("changeset"))
		if err != nil {
			return nil, err
		}
		action := pos[1]
		return client.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{Status: "user_review", ReviewAction: &action, Changeset: change}, origin, goal)
	case "set-changeset":
		pos, err := parseOperation(ctx, fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) != 2 {
			return nil, fmt.Errorf("usage: tasks set-changeset TASK_ID JSON_NULL_OR_ARRAY")
		}
		id, err := canonicalTaskID(pos[0])
		if err != nil {
			return nil, err
		}
		change, err := parseChangeset(pos[1])
		if err != nil {
			return nil, err
		}
		return client.TaskConfirm(ctx, project, id, nil, change, origin)
	}
	return nil, fmt.Errorf("unsupported tasks operation %q", operation)
}

func parseOptionalChangeset(value *string, present bool) (json.RawMessage, error) {
	if !present {
		return nil, nil
	}
	if value == nil {
		return nil, fmt.Errorf("changeset must be a JSON array or null")
	}
	return parseChangeset(*value)
}

func parseChangeset(value string) (json.RawMessage, error) {
	raw := json.RawMessage(value)
	trimmed := strings.TrimSpace(value)
	if !json.Valid(raw) || (trimmed != "null" && !strings.HasPrefix(trimmed, "[")) {
		return nil, fmt.Errorf("changeset must be a JSON array or null")
	}
	if _, err := storage.ValidateChangeset(raw); err != nil {
		return nil, fmt.Errorf("changeset is invalid")
	}
	return append(json.RawMessage(nil), raw...), nil
}

func taskError(err error) error {
	var response *gatewayclient.HTTPError
	if errors.As(err, &response) && response.Status == http.StatusConflict {
		switch response.Code {
		case "task_confirmation_required":
			return fmt.Errorf("task needs a changeset. If no files changed, run `amux tasks --server URL --project IDENT [--goal GOAL] [--human | --session UUID --instance UUID --provider codex --os linux] set-changeset TASK_ID null`. If files changed, run `confirm-mutation TASK_ID` first, then `set-changeset TASK_ID '[\"path/to/file\"]'` with the same common options before each operation")
		case "changeset_confirmation_required":
			return fmt.Errorf("this operation needs a changeset: retry the same operation with `--changeset null` if no files changed, or `--changeset '[\"path/to/file\"]'` if files changed, keeping --server, --project, and actor options before the operation")
		}
	}
	return err
}
