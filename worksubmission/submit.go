package worksubmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/storage"
)

const maxTaskCommentBytes = 65536
const maxSubmitWorkPathsBytes = 4 << 20

type Input struct {
	Project, ConversationID, Root, TaskID, Message string
	Paths                                          []string
	PathsFile                                      string
	ResumeCommit                                   string
	Origin                                         *agent.Origin
	Goal                                           *string
}

type Receipt struct {
	TaskID    string   `json:"task_id"`
	Reference string   `json:"reference"`
	Files     []string `json:"files"`
	CommentID string   `json:"comment_id,omitempty"`
}

func SubmitTask(ctx context.Context, client *gatewayclient.Client, input Input, preferencesPath string) (Receipt, error) {
	var receipt Receipt
	if client == nil || input.Origin == nil {
		return receipt, fmt.Errorf("work submission requires a native agent execution")
	}
	if input.Root == "" {
		return receipt, fmt.Errorf("work submission requires an explicit checkout directory")
	}
	taskID, err := storage.CanonicalTaskID(input.TaskID)
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
		if projectid.NormalizeIdent(candidate.Project.Ident) != projectid.NormalizeIdent(input.Project) || candidate.Project.ArchivedAt != nil {
			continue
		}
		if CheckoutMatchesProject(candidate, identity) {
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
		if conversation.ID != input.ConversationID || projectid.NormalizeIdent(conversation.ProjectIdent) != projectid.NormalizeIdent(input.Project) || conversation.Owner != *input.Origin {
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
	if task.ID != taskID || projectid.NormalizeIdent(task.ProjectIdent) != projectid.NormalizeIdent(input.Project) || !storage.IsActiveTaskStatus(task.Status) || task.OwnerOrigin == nil || *task.OwnerOrigin != *input.Origin {
		return receipt, fmt.Errorf("work was not submitted: task is not in progress under this exact execution in the selected project")
	}
	selectedGoal, err := storage.NormalizeTaskGoal(input.Goal)
	if err != nil {
		return receipt, fmt.Errorf("work was not submitted: invalid selected task goal: %w", err)
	}
	if !submissionGoalMatches(selectedGoal, task.Goal) {
		return receipt, fmt.Errorf("work was not submitted: selected task goal does not match the task")
	}
	preferences := DefaultPreferences()
	if preferencesPath != "" {
		preferences, err = LoadPreferences(preferencesPath)
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
	request := Request{Root: root, TaskID: taskID, Message: input.Message, Paths: paths}
	var outcome *Outcome
	if input.ResumeCommit != "" {
		outcome, err = Resume(ctx, request, preferences, input.ResumeCommit, OpenGitHubPullRequest)
	} else {
		outcome, err = Submit(ctx, request, preferences, OpenGitHubPullRequest)
	}
	if err != nil {
		return receipt, err
	}
	receipt = Receipt{TaskID: taskID, Reference: outcome.Reference, Files: outcome.Files}
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

func validateSubmissionComment(raw []byte, taskID, content string, origin agent.Origin) (*storage.TaskComment, error) {
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

func submissionComment(outcome *Outcome) string {
	comment := outcome.Comment()
	if len(comment) <= maxTaskCommentBytes {
		return comment
	}
	return fmt.Sprintf("Work submitted: %s\n\n%d files submitted. The complete file list is in the submission receipt.", outcome.Reference, len(outcome.Files))
}

func canonicalSubmissionRoot(ctx context.Context, inputRoot string) (string, projectid.Identity, error) {
	if inputRoot == "" {
		return "", projectid.Identity{}, fmt.Errorf("work submission requires an explicit checkout directory")
	}
	root, err := filepath.Abs(inputRoot)
	if err != nil {
		return "", projectid.Identity{}, fmt.Errorf("resolve explicit checkout: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", projectid.Identity{}, fmt.Errorf("resolve explicit checkout: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", projectid.Identity{}, fmt.Errorf("explicit checkout directory is unavailable")
	}
	identity, err := projectid.Resolve(ctx, root)
	if err != nil {
		return "", projectid.Identity{}, fmt.Errorf("resolve explicit checkout identity: %w", err)
	}
	if identity.Directory == "" {
		return "", projectid.Identity{}, fmt.Errorf("resolve explicit checkout identity: unavailable")
	}
	return root, identity, nil
}

func CheckoutMatchesProject(candidate storage.ProjectOverview, identity projectid.Identity) bool {
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
	paths, err := DecodePaths(raw)
	if err != nil {
		return nil, fmt.Errorf("path list must be a JSON array of file paths")
	}
	return paths, nil
}

func DecodePaths(raw json.RawMessage) ([]string, error) {
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

func ValidateRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return fmt.Errorf("--directory must be an absolute checkout path")
	}
	return nil
}
