package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

// PrepareRun creates a gateway conversation and saves the local recovery record
// before a worker is started.
func PrepareRun(ctx context.Context, store *storage.Store, client *gatewayclient.Client, project, taskID, directory, provider string, command []string, goal *string) (*storage.WorkerRun, *storage.Conversation, error) {
	if store == nil || client == nil || project == "" || directory == "" {
		return nil, nil, fmt.Errorf("worker preparation requires store, client, project and directory")
	}
	if provider != "antigravity" && provider != "codex" && provider != "claude" {
		return nil, nil, fmt.Errorf("unsupported provider %q", provider)
	}
	selectedGoal, err := storage.NormalizeTaskGoal(goal)
	if err != nil {
		return nil, nil, err
	}
	if taskID == "" && selectedGoal != nil {
		return nil, nil, fmt.Errorf("a task goal requires a task")
	}
	commandJSON, _ := json.Marshal(command)
	if len(command) > 64 || len(commandJSON) > 65536 || len(command) > 0 && command[0] == "" {
		return nil, nil, fmt.Errorf("worker command must be a bounded nonempty command array")
	}
	absPath, err := filepath.Abs(directory)
	if err != nil {
		return nil, nil, err
	}
	absPath, err = filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, nil, fmt.Errorf("worker checkout must exist")
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		return nil, nil, fmt.Errorf("worker checkout must be a directory")
	}
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	owner := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: provider, OS: platform}
	conversation, err := client.CreateConversationWithGoal(ctx, project, taskID, owner, selectedGoal)
	if err != nil {
		return nil, nil, err
	}
	run := &storage.WorkerRun{
		ID: uuid.NewString(), Server: client.Endpoint(), Project: project, ConversationID: conversation.ID,
		Directory: absPath, Command: append([]string(nil), command...), Owner: owner, Goal: selectedGoal,
	}
	if err = store.SaveWorkerRun(ctx, *run); err != nil {
		return nil, nil, err
	}
	return run, conversation, nil
}
