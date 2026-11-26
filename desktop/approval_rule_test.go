package desktop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/chat"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/worksubmission"
)

func TestAlwaysAllowEditsSavesDirectoryRuleAndApprovesPendingRequest(t *testing.T) {
	fynetest.NewApp()
	gateway := newFakeGateway(t)
	client, err := gatewayclient.New(gateway.server.URL, "test-key", gateway.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	directory := t.TempDir()
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	ui, err := New(Config{Client: client, Project: testProject, ConversationID: testConv, Dispatch: callbacks.dispatch, ToolApprovalsPath: path, ToolApprovalDirectory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close(); ui.Wait(); callbacks.pump() })
	ui.Start(context.Background())
	permission := chat.Permission{EventID: "approval-event", Details: json.RawMessage(`{"command":"git log --oneline"}`), Options: []conversation.PermissionOption{{ID: "accept", Name: "Allow", Kind: "allow_once"}}}
	controls := ui.renderApprovalRule(permission)
	if len(controls) != 1 {
		t.Fatalf("missing Always allow: %v", controls)
	}
	fynetest.Tap(controls[0].(*widget.Button))
	callbacks.pump()
	controls = ui.renderApprovalRule(permission)
	form := controls[1].(*widget.Form)
	directoryEntry := form.Items[0].Widget.(*widget.Entry)
	pattern := form.Items[1].Widget.(*widget.Entry)
	if directoryEntry.Text != directory || pattern.Text != "git log --oneline" {
		t.Fatalf("incorrect initial rule: %q / %q", directoryEntry.Text, pattern.Text)
	}
	pattern.SetText("git log *")
	// Stream refreshes must preserve the user's edits.
	controls = ui.renderApprovalRule(permission)
	form = controls[1].(*widget.Form)
	if got := form.Items[1].Widget.(*widget.Entry).Text; got != "git log *" {
		t.Fatalf("draft lost: %q", got)
	}
	fynetest.Tap(findSettingsButton(container.NewVBox(controls...), "Save rule and allow"))
	waitForUI(t, callbacks, 3*time.Second, func() bool {
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		return len(gateway.commands) > 0
	})
	rules, err := worksubmission.LoadDirectoryToolApprovals(path, directory)
	if err != nil || !worksubmission.MatchToolApproval(rules, "git log --stat") {
		t.Fatalf("rule not saved: %v, %v", rules, err)
	}
	other, err := worksubmission.LoadDirectoryToolApprovals(path, t.TempDir())
	if err != nil || worksubmission.MatchToolApproval(other, "git log --stat") {
		t.Fatalf("rule escaped directory: %v, %v", other, err)
	}
	gateway.mu.Lock()
	command := gateway.commands[0].Command
	gateway.mu.Unlock()
	if command.Kind != "permission" || command.PermissionEventID != permission.EventID || command.OptionID != "accept" {
		t.Fatalf("wrong approval response: %#v", command)
	}
}
