package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
)

const maxTaskCommentBytes = 65536
const maxSubmitWorkPathsBytes = 4 << 20

func submissionPreferencesPath(override, configuredEnv string) (string, error) {
	return worksubmission.OptionalPreferencesPath(override, configuredEnv)
}

func runWorkSubmissionSettings(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	flags := flag.NewFlagSet("amux work-submission", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pathOverride := flags.String("work-preferences", "", "work preference JSON path (default: user config directory)")
	mode := flags.String("mode", "", "submission mode: commit_and_push or pull_request")
	branchPrefix := flags.String("branch-prefix", "", "prefix used for pull request branches")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: amux work-submission [--work-preferences PATH] show | set [--mode commit_and_push|pull_request] [--branch-prefix PREFIX]")
		flags.PrintDefaults()
	}
	if hasTaskHelp(args) {
		flags.Usage()
		return nil
	}
	pos, err := parseTaskFlagsArgs(flags, args, nil)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
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
		if !flagWasSet(flags, "mode") && !flagWasSet(flags, "branch-prefix") {
			return fmt.Errorf("set requires --mode or --branch-prefix")
		}
		var change worksubmission.PreferencesChange
		if flagWasSet(flags, "mode") {
			selected := worksubmission.Mode(*mode)
			change.Mode = &selected
		}
		if flagWasSet(flags, "branch-prefix") {
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

type submitWorkInput struct {
	Project, ConversationID, Root, TaskID, Message string
	Paths                                          []string
	PathsFile                                      string
	ResumeCommit                                   string
	Origin                                         *storage.AgentOrigin
	Goal                                           *string
}

type submitWorkReceipt struct {
	TaskID    string   `json:"task_id"`
	Reference string   `json:"reference"`
	Files     []string `json:"files"`
	CommentID string   `json:"comment_id,omitempty"`
}

func submitWork(ctx context.Context, client *gatewayclient.Client, input submitWorkInput, preferencesPath string) (submitWorkReceipt, error) {
	var receipt submitWorkReceipt
	if client == nil || input.Origin == nil {
		return receipt, fmt.Errorf("work submission requires a native agent execution")
	}
	if input.Root == "" {
		return receipt, fmt.Errorf("work submission requires an explicit checkout directory")
	}
	taskID, err := canonicalTaskID(input.TaskID)
	if err != nil {
		return receipt, err
	}
	input.TaskID = taskID
	root, identity, err := canonicalSubmissionRoot(ctx, input.Root)
	if err != nil {
		return receipt, err
	}
	projectList, err := client.Projects(ctx)
	if err != nil {
		return receipt, fmt.Errorf("verify checkout project: %w", err)
	}
	projectFound := false
	for _, candidate := range projectList {
		if projects.NormalizeIdent(candidate.Project.Ident) != projects.NormalizeIdent(input.Project) || candidate.Project.ArchivedAt != nil {
			continue
		}
		if checkoutMatchesProject(candidate, identity) {
			projectFound = true
		}
		break
	}
	if !projectFound {
		return receipt, fmt.Errorf("explicit checkout is not bound to active gateway project %q", input.Project)
	}
	if input.ConversationID != "" {
		conversation, err := client.Conversation(ctx, input.Project, input.ConversationID)
		if err != nil {
			return receipt, fmt.Errorf("verify bound conversation: %w", err)
		}
		if conversation.ID != input.ConversationID || projects.NormalizeIdent(conversation.ProjectIdent) != projects.NormalizeIdent(input.Project) || conversation.Owner != *input.Origin {
			return receipt, fmt.Errorf("bound conversation does not match this execution and project")
		}
	}
	rawTask, err := client.TaskRead(ctx, input.Project, taskID, 0)
	if err != nil {
		return receipt, fmt.Errorf("work was not submitted: verify task ownership before reading paths: %w", err)
	}
	var task storage.Task
	if err := json.Unmarshal(rawTask, &task); err != nil {
		return receipt, fmt.Errorf("work was not submitted: decode task ownership response: %w", err)
	}
	if task.ID != taskID || projects.NormalizeIdent(task.ProjectIdent) != projects.NormalizeIdent(input.Project) || !storage.IsActiveTaskStatus(task.Status) || task.OwnerOrigin == nil || *task.OwnerOrigin != *input.Origin {
		return receipt, fmt.Errorf("work was not submitted: task is not in progress under this exact execution in the selected project")
	}
	selectedGoal, err := storage.NormalizeTaskGoal(input.Goal)
	if err != nil {
		return receipt, fmt.Errorf("work was not submitted: invalid selected task goal: %w", err)
	}
	if !submissionGoalMatches(selectedGoal, task.Goal) {
		return receipt, fmt.Errorf("work was not submitted: selected task goal does not match the task")
	}
	preferences := worksubmission.DefaultPreferences()
	if preferencesPath != "" {
		preferences, err = worksubmission.LoadPreferences(preferencesPath)
		if err != nil {
			return receipt, err
		}
	}
	paths := append([]string(nil), input.Paths...)
	if input.PathsFile != "" {
		if len(paths) != 0 {
			return receipt, fmt.Errorf("provide paths or --paths-file, not both")
		}
		paths, err = readSubmitWorkPaths(input.PathsFile)
		if err != nil {
			return receipt, err
		}
	}
	if len(paths) == 0 {
		return receipt, fmt.Errorf("list at least one changed path to submit")
	}
	request := worksubmission.Request{Root: root, TaskID: taskID, Message: input.Message, Paths: paths}
	var outcome *worksubmission.Outcome
	if input.ResumeCommit != "" {
		outcome, err = worksubmission.Resume(ctx, request, preferences, input.ResumeCommit, worksubmission.OpenGitHubPullRequest)
	} else {
		outcome, err = worksubmission.Submit(ctx, request, preferences, worksubmission.OpenGitHubPullRequest)
	}
	if err != nil {
		return receipt, err
	}
	receipt = submitWorkReceipt{TaskID: taskID, Reference: outcome.Reference, Files: outcome.Files}
	comment := submissionComment(outcome)
	if input.ConversationID != "" {
		arguments, _ := json.Marshal(map[string]string{"id": taskID, "body": comment})
		result, commentErr := client.CallTaskToolWithGoal(ctx, input.Project, input.ConversationID, *input.Origin, input.Goal, "task_comment", arguments)
		if commentErr == nil && result.IsError {
			message := "agent comment was rejected"
			if len(result.Content) > 0 {
				message = result.Content[0].Text
			}
			commentErr = errors.New(message)
		}
		if commentErr == nil {
			if len(result.Content) != 1 || result.Content[0].Type != "text" {
				commentErr = fmt.Errorf("gateway returned an invalid agent comment acknowledgement")
			} else {
				var saved *storage.TaskComment
				saved, commentErr = validateSubmissionComment([]byte(result.Content[0].Text), taskID, comment, *input.Origin)
				if commentErr == nil {
					receipt.CommentID = saved.ID
				}
			}
		}
		if commentErr != nil {
			return receipt, fmt.Errorf("work was submitted as %s, but posting the result comment failed: %w", outcome.Reference, commentErr)
		}
	} else {
		raw, commentErr := client.TaskComment(ctx, input.Project, taskID, comment, input.Origin)
		if commentErr != nil {
			return receipt, fmt.Errorf("work was submitted as %s, but posting the result comment failed: %w", outcome.Reference, commentErr)
		}
		saved, commentErr := validateSubmissionComment(raw, taskID, comment, *input.Origin)
		if commentErr != nil {
			return receipt, fmt.Errorf("work was submitted as %s, but posting the result comment failed: %w", outcome.Reference, commentErr)
		}
		receipt.CommentID = saved.ID
	}
	action := "Review the submitted work at " + outcome.Reference + " and accept it if complete."
	expectedStatus := "in_progress"
	update := storage.TaskWorkflowUpdate{
		Status: "user_review", ExpectedStatus: &expectedStatus, ReviewAction: &action, SubmissionCommentID: receipt.CommentID,
	}
	changeset, err := json.Marshal(outcome.Files)
	if err != nil {
		return receipt, err
	}
	if _, err := storage.ValidateChangeset(changeset); err == nil {
		update.Changeset = changeset
	}
	if _, err := client.TaskUpdate(ctx, input.Project, taskID, update, input.Origin, input.Goal); err != nil {
		return receipt, fmt.Errorf("work was submitted as %s and the result was posted, but moving the task to User Review failed: %w", outcome.Reference, err)
	}
	return receipt, nil
}

func validateSubmissionComment(raw []byte, taskID, content string, origin storage.AgentOrigin) (*storage.TaskComment, error) {
	var comment storage.TaskComment
	if err := json.Unmarshal(raw, &comment); err != nil {
		return nil, fmt.Errorf("gateway returned an invalid agent comment acknowledgement: %w", err)
	}
	id, err := uuid.Parse(comment.ID)
	if err != nil || id == uuid.Nil || comment.TaskID != taskID || comment.Author != origin.SessionID || comment.AuthorType != "agent" || comment.Content != content || comment.Origin == nil || *comment.Origin != origin {
		return nil, fmt.Errorf("gateway agent comment acknowledgement did not match the task, content, or provenance")
	}
	return &comment, nil
}

func submissionComment(outcome *worksubmission.Outcome) string {
	comment := outcome.Comment()
	if len(comment) <= maxTaskCommentBytes {
		return comment
	}
	return fmt.Sprintf("Work submitted: %s\n\n%d files submitted. The complete file list is in the submission receipt.", outcome.Reference, len(outcome.Files))
}

func canonicalSubmissionRoot(ctx context.Context, inputRoot string) (string, projectidentity.Identity, error) {
	if inputRoot == "" {
		return "", projectidentity.Identity{}, fmt.Errorf("work submission requires an explicit checkout directory")
	}
	root, err := filepath.Abs(inputRoot)
	if err != nil {
		return "", projectidentity.Identity{}, fmt.Errorf("resolve explicit checkout: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", projectidentity.Identity{}, fmt.Errorf("resolve explicit checkout: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", projectidentity.Identity{}, fmt.Errorf("explicit checkout directory is unavailable")
	}
	identity, err := projectidentity.Resolve(ctx, root)
	if err != nil {
		return "", projectidentity.Identity{}, fmt.Errorf("resolve explicit checkout identity: %w", err)
	}
	if identity.Directory == "" {
		return "", projectidentity.Identity{}, fmt.Errorf("resolve explicit checkout identity: unavailable")
	}
	return root, identity, nil
}

func checkoutMatchesProject(candidate storage.ProjectOverview, identity projectidentity.Identity) bool {
	if candidate.Project.CanonicalRemote != nil {
		return identity.Upstream != "" && *candidate.Project.CanonicalRemote == identity.Upstream
	}
	return candidate.Project.Kind == "adhoc" && identity.Upstream == ""
}

func submissionGoalMatches(selected, task *string) bool {
	selectedGoal, err := storage.NormalizeTaskGoal(selected)
	if err != nil {
		return false
	}
	taskGoal, err := storage.NormalizeTaskGoal(task)
	if err != nil {
		return false
	}
	return taskGoal == nil || selectedGoal != nil && *selectedGoal == *taskGoal
}

func readSubmitWorkPaths(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read path list %s: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSubmitWorkPathsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read path list %s: %w", path, err)
	}
	if len(data) > maxSubmitWorkPathsBytes {
		return nil, fmt.Errorf("path list exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("path list must be a JSON array of file paths")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("path list must contain exactly one JSON array")
	}
	paths, err := decodeSubmitWorkPaths(raw)
	if err != nil {
		return nil, fmt.Errorf("path list must be a JSON array of file paths")
	}
	return paths, nil
}

func decodeSubmitWorkPaths(raw json.RawMessage) ([]string, error) {
	var items []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &items) != nil {
		return nil, fmt.Errorf("paths must be an array of strings")
	}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		if len(item) == 0 || item[0] != '"' {
			return nil, fmt.Errorf("paths must be an array of strings")
		}
		var path string
		if err := json.Unmarshal(item, &path); err != nil {
			return nil, fmt.Errorf("paths must be an array of strings")
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func submitWorkMCPTool() mcpstdio.Tool {
	return mcpstdio.Tool{Name: "submit_work", Description: "Submit the listed changed files for the currently owned task. Commit and publish them, or open a pull request, according to local work submission settings, then post an agent comment with the result. For a recovered task commit, supply resume_commit and the exact original path list.", InputSchema: json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string"},"message":{"type":"string"},"paths":{"type":"array","description":"Changed files to submit (modified, added or deleted), relative to the project or absolute","items":{"type":"string"}},"resume_commit":{"type":"string"}},"required":["task_id","message","paths"],"additionalProperties":false}`)}
}

func callSubmitWorkMCP(ctx context.Context, client *gatewayclient.Client, project, conversationID, root string, origin storage.AgentOrigin, goal *string, preferencesPath string, args json.RawMessage) (mcpstdio.Result, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil {
		return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: "invalid submit_work arguments"}}, IsError: true}, nil
	}
	for name := range fields {
		if name != "task_id" && name != "message" && name != "paths" && name != "resume_commit" {
			return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: "unknown submit_work argument: " + name}}, IsError: true}, nil
		}
	}
	var request struct {
		TaskID       string   `json:"task_id"`
		Message      string   `json:"message"`
		Paths        []string `json:"paths"`
		ResumeCommit string   `json:"resume_commit"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: "invalid submit_work arguments"}}, IsError: true}, nil
	}
	pathsRaw, pathsPresent := fields["paths"]
	if pathsPresent && string(pathsRaw) != "null" {
		paths, err := decodeSubmitWorkPaths(pathsRaw)
		if err != nil {
			return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: err.Error()}}, IsError: true}, nil
		}
		request.Paths = paths[:0]
		for _, path := range paths {
			if strings.TrimSpace(path) != "" {
				request.Paths = append(request.Paths, path)
			}
		}
	}
	if request.TaskID == "" || request.Message == "" || len(request.Paths) == 0 {
		return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: "task_id, message, and paths are required"}}, IsError: true}, nil
	}
	if err := validateWorkSubmissionRoot(root); err != nil {
		return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: err.Error()}}, IsError: true}, nil
	}
	receipt, err := submitWork(ctx, client, submitWorkInput{Project: project, ConversationID: conversationID, Root: root, TaskID: request.TaskID, Message: request.Message, Paths: request.Paths, ResumeCommit: request.ResumeCommit, Origin: &origin, Goal: goal}, preferencesPath)
	if err != nil {
		return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: err.Error()}}, IsError: true}, nil
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: string(encoded)}}}, nil
}

func validateWorkSubmissionRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return fmt.Errorf("--directory must be an absolute checkout path")
	}
	return nil
}

func runTaskSubmitWork(ctx context.Context, client *gatewayclient.Client, project string, goal *string, origin *storage.AgentOrigin, args []string, preferencesPath string, stdout, stderr io.Writer) error {
	flags := taskFlags("tasks submit-work", stderr)
	directory := flags.String("directory", "", "explicit checkout directory to submit from (required)")
	resume := flags.String("resume-commit", "", "publish this existing task commit without committing files again")
	pathsFile := flags.String("paths-file", "", "JSON array of exact changed paths, for lists too large for command-line arguments")
	pos, err := parseTaskFlagsArgs(flags, args, nil)
	if err != nil {
		return err
	}
	if len(pos) < 2 || *directory == "" {
		return fmt.Errorf("usage: tasks submit-work --directory CHECKOUT [--resume-commit SHA] [--paths-file JSON] TASK_UUID MESSAGE [PATHS...]")
	}
	if origin == nil {
		return fmt.Errorf("tasks submit-work requires a complete native agent origin")
	}
	if err := validateWorkSubmissionRoot(*directory); err != nil {
		return err
	}
	receipt, err := submitWork(ctx, client, submitWorkInput{
		Project: project, Root: *directory, TaskID: pos[0], Message: pos[1], Paths: pos[2:], PathsFile: *pathsFile,
		ResumeCommit: *resume, Origin: origin, Goal: goal,
	}, preferencesPath)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(receipt)
}

func runSubmitWorkAlias(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	flags := taskFlags("amux submit-work", stderr)
	serverURL := flags.String("server", "", "gateway server URL (required unless --enrolled-profile is used)")
	enrolledProfile := flags.String("enrolled-profile", "", "enrolled gateway credential manifest")
	project := flags.String("project", "", "gateway project identifier (required)")
	keyEnv := flags.String("api-key-env", "GATEWAY_API_KEY", "environment variable containing the gateway API credential")
	goalFlag := flags.String("goal", "", "select a task goal")
	session := flags.String("session", "", "native agent session UUID")
	instance := flags.String("instance", "", "native agent instance UUID")
	provider := flags.String("provider", "", "native provider")
	hostOS := flags.String("os", "", "native agent OS")
	directory := flags.String("directory", "", "explicit checkout directory to submit from (required)")
	resume := flags.String("resume-commit", "", "publish this existing task commit without committing files again")
	pathsFile := flags.String("paths-file", "", "JSON array of exact changed paths, for lists too large for command-line arguments")
	preferencesOverride := flags.String("work-preferences", "", "work preference JSON path (default: user config directory)")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: amux submit-work (--server URL | --enrolled-profile PATH) --project IDENT --directory CHECKOUT --session UUID --instance UUID --provider NAME --os OS TASK_UUID MESSAGE [PATHS...]")
		flags.PrintDefaults()
	}
	if hasTaskHelp(args) {
		flags.Usage()
		return nil
	}
	pos, err := parseTaskFlagsArgs(flags, args, nil)
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
	if err := validateWorkSubmissionRoot(*directory); err != nil {
		return err
	}
	origin, err := storage.NormalizeOrigin(&storage.AgentOrigin{SessionID: *session, InstanceID: *instance, Provider: *provider, OS: *hostOS})
	if err != nil {
		return err
	}
	apiKey := ""
	if *enrolledProfile == "" {
		apiKey = getenv(*keyEnv)
	}
	client, _, err := gatewayClientForProfile(*serverURL, *enrolledProfile, apiKey)
	if err != nil {
		return err
	}
	preferencesPath, err := worksubmission.OptionalPreferencesPath(*preferencesOverride, getenv("AMUX_WORK_SUBMISSION_PREFERENCES"))
	if err != nil {
		return err
	}
	var selectedGoal *string
	if flagWasSet(flags, "goal") {
		selectedGoal = goalFlag
	}
	receipt, err := submitWork(ctx, client, submitWorkInput{
		Project: *project, Root: *directory, TaskID: pos[0], Message: pos[1], Paths: pos[2:], PathsFile: *pathsFile,
		ResumeCommit: *resume, Origin: origin, Goal: selectedGoal,
	}, preferencesPath)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(receipt)
}
