package worksubmission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

// CIRun records one GitHub Actions attempt for the report's endpoint commit.
type CIRun struct {
	ID         uint64          `json:"id"`
	Attempt    uint64          `json:"attempt"`
	Name       json.RawMessage `json:"name"`
	Event      json.RawMessage `json:"event"`
	HeadSHA    string          `json:"head_sha"`
	Status     string          `json:"status"`
	Conclusion json.RawMessage `json:"conclusion"`
	URL        json.RawMessage `json:"url"`
}

var reportSHA = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
var githubRepoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ReadReportCI reads paginated Actions runs for the requested endpoint SHA.
// It invokes only gh's repository read and explicit API GET operations.
func ReadReportCI(ctx context.Context, root, sha string) ([]CIRun, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("GitHub CI report requires a checkout root")
	}
	if !reportSHA.MatchString(sha) {
		return nil, fmt.Errorf("GitHub CI report requires a full 40- or 64-digit commit SHA")
	}
	repoCommand := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "nameWithOwner")
	repoCommand.Dir = root
	repoCommand.Env = cleanEnv()
	repoOutput, err := repoCommand.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read GitHub repository identity: %w", err)
	}
	var repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if json.Unmarshal(repoOutput, &repository) != nil || !githubRepoName.MatchString(repository.NameWithOwner) {
		return nil, fmt.Errorf("GitHub repository identity is missing or invalid")
	}
	endpoint := fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", repository.NameWithOwner, sha)
	command := exec.CommandContext(ctx, "gh", "api", "--paginate", "--method", "GET", endpoint)
	command.Dir = root
	command.Env = cleanEnv()
	command.Stderr = io.Discard
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("read GitHub Actions response: %w", err)
	}
	if err := command.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("start GitHub Actions read: %w", err)
	}

	var runs []CIRun
	decoder := json.NewDecoder(stdout)
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("invalid GitHub Actions page: %w", err)
		}
		pageRuns, err := parseCIRuns(raw, sha)
		if err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return nil, err
		}
		runs = append(runs, pageRuns...)
	}
	if err := command.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("GitHub Actions read failed: %w", err)
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("no CI runs found for the exact endpoint SHA")
	}
	return runs, nil
}

func parseCIRuns(raw json.RawMessage, sha string) ([]CIRun, error) {
	var page map[string]json.RawMessage
	if json.Unmarshal(raw, &page) != nil || page == nil {
		return nil, fmt.Errorf("invalid GitHub CI response")
	}
	list, ok := page["workflow_runs"]
	if !ok {
		return nil, fmt.Errorf("invalid GitHub CI response: workflow_runs is missing")
	}
	var records []map[string]json.RawMessage
	if json.Unmarshal(list, &records) != nil || records == nil {
		return nil, fmt.Errorf("invalid GitHub CI response: workflow_runs must be an array")
	}
	out := make([]CIRun, 0, len(records))
	for _, record := range records {
		var run CIRun
		if json.Unmarshal(record["id"], &run.ID) != nil || run.ID <= 0 ||
			json.Unmarshal(record["run_attempt"], &run.Attempt) != nil || run.Attempt <= 0 ||
			json.Unmarshal(record["head_sha"], &run.HeadSHA) != nil || run.HeadSHA != sha ||
			json.Unmarshal(record["status"], &run.Status) != nil || run.Status == "" ||
			(run.Status == "completed" && !isJSONText(record["conclusion"])) {
			return nil, fmt.Errorf("GitHub CI run is missing a positive ID, attempt, exact SHA, status or completed conclusion")
		}
		run.Name = record["name"]
		run.Event = record["event"]
		run.Conclusion = record["conclusion"]
		run.URL = record["html_url"]
		out = append(out, run)
	}
	return out, nil
}

func isJSONText(raw json.RawMessage) bool {
	var value string
	return len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &value) == nil
}
