package worksubmission

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
)

// ReportOptions selects a local Git history window. Set Range or both Since
// and Until; date windows use inclusive Since and exclusive Until bounds.
type ReportOptions struct {
	Range string
	Since string
	Until string
}

type WorkCommit struct {
	SHA         string      `json:"sha"`
	CommittedAt int64       `json:"committed_at"`
	Subject     string      `json:"subject"`
	TaskIDs     []uuid.UUID `json:"task_ids"`
}

type TaskWork struct {
	ID           uuid.UUID       `json:"id"`
	Title        *string         `json:"title"`
	Status       *string         `json:"status"`
	Commits      []string        `json:"commits"`
	LatestReview json.RawMessage `json:"latest_review"`
}

// WorkReport contains local Git facts only. It performs no gateway or CI reads.
type WorkReport struct {
	AsOf                 string                 `json:"as_of"`
	Selection            string                 `json:"selection"`
	FromSHA              *string                `json:"from_sha"`
	ToSHA                string                 `json:"to_sha"`
	SinceMS              int64                  `json:"since_ms"`
	UntilMS              int64                  `json:"until_ms"`
	Commits              []WorkCommit           `json:"commits"`
	Files                []string               `json:"files"`
	Tasks                map[uuid.UUID]TaskWork `json:"tasks"`
	UntraileredCommits   []string               `json:"untrailered_commits"`
	CompletedNotReleased []TaskWork             `json:"completed_not_released"`
	CIRuns               []CIRun                `json:"ci_runs"`
	Partial              []string               `json:"partial"`
}

func reportGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	all := []string{"-C", root, "--no-optional-locks", "-c", "core.hooksPath=/dev/null"}
	all = append(all, args...)
	c := exec.CommandContext(ctx, "git", all...)
	c.Env = append(cleanEnv(), "GIT_TERMINAL_PROMPT=0")
	out, err := c.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read-only Git report command failed: %w", err)
	}
	return out, nil
}

func reportDate(s string) (int64, error) {
	if d, err := time.Parse("2006-01-02", s); err == nil {
		return d.UTC().UnixMilli(), nil
	}
	d, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("dates must be YYYY-MM-DD or RFC3339: %w", err)
	}
	return d.UnixMilli(), nil
}

func resolveReportRef(ctx context.Context, root, ref string) (string, int64, error) {
	if ref == "" {
		return "", 0, fmt.Errorf("range needs two nonempty refs")
	}
	out, err := reportGit(ctx, root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", 0, fmt.Errorf("cannot resolve ref %q to a commit: %w", ref, err)
	}
	sha := strings.TrimSpace(string(out))
	out, err = reportGit(ctx, root, "show", "-s", "--format=%ct", sha, "--")
	if err != nil {
		return "", 0, err
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return sha, sec * 1000, err
}

// EnrichGateway adds current task and linked review facts through GET-only
// gatewayclient methods. Exact-SHA CI lookup remains an independent concern.
func (r *WorkReport) EnrichGateway(ctx context.Context, client *gatewayclient.Client, project string) error {
	rows, err := client.TaskList(ctx, project, gatewayclient.TaskListOptions{Statuses: []string{"todo", "in_progress", "user_review", "done", "cancelled"}, IncludeStale: true, AllGoals: true}, nil, nil)
	if err != nil {
		return err
	}
	ids := map[uuid.UUID]bool{}
	for id := range r.Tasks {
		ids[id] = true
	}
	for _, row := range rows {
		id, parseErr := uuid.Parse(row.ID)
		if parseErr != nil || id == uuid.Nil {
			return fmt.Errorf("invalid task ID in report list")
		}
		if row.Status == "done" && (row.CreatedAt == 0 || row.CreatedAt <= r.UntilMS) && (row.UpdatedAt == 0 || row.UpdatedAt >= r.SinceMS) {
			ids[id] = true
		}
	}
	reviews := map[uuid.UUID][]uuid.UUID{}
	for _, row := range rows {
		isReview := containsReportLabel(row.Labels, "task-review")
		if !isReview {
			continue
		}
		reviewID, _ := uuid.Parse(row.ID)
		for _, label := range row.Labels {
			if strings.HasPrefix(label, "review-of:") {
				target, e := uuid.Parse(strings.TrimPrefix(label, "review-of:"))
				if e == nil && ids[target] {
					reviews[target] = append(reviews[target], reviewID)
				}
			}
		}
	}
	for _, linked := range reviews {
		for _, id := range linked {
			ids[id] = true
		}
	}
	details := map[uuid.UUID]reportTaskDetail{}
	ordered := make([]uuid.UUID, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].String() < ordered[j].String() })
	for _, id := range ordered {
		comments := uint64(0)
		for _, linked := range reviews {
			for _, reviewID := range linked {
				if reviewID == id {
					comments = 100
				}
			}
		}
		raw, readErr := client.TaskRead(ctx, project, id.String(), comments)
		if readErr != nil {
			r.Partial = append(r.Partial, fmt.Sprintf("Task %s: gateway detail unavailable", id))
			continue
		}
		var detail reportTaskDetail
		if jsonErr := json.Unmarshal(raw, &detail); jsonErr != nil {
			r.Partial = append(r.Partial, fmt.Sprintf("Task %s: invalid gateway detail", id))
			continue
		}
		details[id] = detail
	}
	for id, detail := range details {
		if containsReportLabel(detail.Labels, "task-review") {
			continue
		}
		work, committed := r.Tasks[id]
		work.ID = id
		work.Title = detail.Title
		status := detail.Status
		work.Status = &status
		if !committed && detail.Status == "done" && detail.DoneAt != nil && ((r.FromSHA != nil && *detail.DoneAt > r.SinceMS && *detail.DoneAt <= r.UntilMS) || (r.FromSHA == nil && *detail.DoneAt >= r.SinceMS && *detail.DoneAt < r.UntilMS)) {
			r.CompletedNotReleased = append(r.CompletedNotReleased, work)
		}
		if committed {
			r.Tasks[id] = work
		}
	}
	for taskID, reviewIDs := range reviews {
		var latest *reportTaskDetail
		var latestID uuid.UUID
		for _, id := range reviewIDs {
			detail, ok := details[id]
			if !ok {
				continue
			}
			if latest == nil || detail.CreatedAt > latest.CreatedAt {
				copy := detail
				latest = &copy
				latestID = id
			}
		}
		if latest == nil {
			continue
		}
		var comment *reportTaskComment
		for i := range latest.Comments {
			candidate := &latest.Comments[i]
			if candidate.AuthorType == "system" {
				continue
			}
			if comment == nil || candidate.CreatedAt > comment.CreatedAt {
				comment = candidate
			}
		}
		fact := map[string]any{"task_id": latestID.String(), "status": latest.Status}
		if comment != nil {
			fact["latest_comment"] = map[string]any{"content": comment.Content, "created_at": comment.CreatedAt}
		}
		encoded, _ := json.Marshal(fact)
		if work, ok := r.Tasks[taskID]; ok {
			work.LatestReview = encoded
			r.Tasks[taskID] = work
		}
		for i := range r.CompletedNotReleased {
			if r.CompletedNotReleased[i].ID == taskID {
				r.CompletedNotReleased[i].LatestReview = encoded
			}
		}
	}
	return nil
}

type reportTaskDetail struct {
	Title     *string             `json:"title"`
	Status    string              `json:"status"`
	Labels    []string            `json:"labels"`
	DoneAt    *int64              `json:"done_at"`
	CreatedAt int64               `json:"created_at"`
	UpdatedAt int64               `json:"updated_at"`
	Comments  []reportTaskComment `json:"comments"`
}
type reportTaskComment struct {
	AuthorType string `json:"author_type"`
	Content    string `json:"content"`
	CreatedAt  int64  `json:"created_at"`
}

func containsReportLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

// BuildLocalReport builds a read-only work inventory from local Git history.
func BuildLocalReport(ctx context.Context, root string, options ReportOptions) (*WorkReport, error) {
	var from *string
	var to string
	var since, until int64
	var selection string
	var revisions []string
	if options.Range != "" {
		if options.Since != "" || options.Until != "" {
			return nil, fmt.Errorf("range conflicts with --since and --until")
		}
		parts := strings.Split(options.Range, "..")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.HasPrefix(parts[1], ".") {
			return nil, fmt.Errorf("use a two-dot FROM..TO range")
		}
		f, ft, err := resolveReportRef(ctx, root, parts[0])
		if err != nil {
			return nil, err
		}
		t, tt, err := resolveReportRef(ctx, root, parts[1])
		if err != nil {
			return nil, err
		}
		if ft > tt {
			return nil, fmt.Errorf("range endpoint commit dates are reversed")
		}
		from, to, since, until = &f, t, ft, tt
		selection, revisions = options.Range, []string{f + ".." + t}
	} else {
		if options.Since == "" || options.Until == "" {
			return nil, fmt.Errorf("provide Range or both Since and Until")
		}
		var err error
		since, err = reportDate(options.Since)
		if err != nil {
			return nil, err
		}
		until, err = reportDate(options.Until)
		if err != nil {
			return nil, err
		}
		if since >= until {
			return nil, fmt.Errorf("Since must precede Until")
		}
		to, _, err = resolveReportRef(ctx, root, "HEAD")
		if err != nil {
			return nil, err
		}
		selection = fmt.Sprintf("%s <= commit time < %s (HEAD)", options.Since, options.Until)
		revisions = []string{to, fmt.Sprintf("--since-as-filter=@%d", since/1000), fmt.Sprintf("--until=@%d", until/1000)}
	}
	args := []string{"log", "-z", "--reverse", "--format=%H%x00%ct%x00%s%x00%(trailers:key=AmuxID,valueonly,separator=%x1f)"}
	args = append(args, revisions...)
	args = append(args, "--")
	log, err := reportGit(ctx, root, args...)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(string(log), "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%4 != 0 {
		return nil, fmt.Errorf("invalid Git commit records")
	}
	r := &WorkReport{AsOf: time.Now().UTC().Format(time.RFC3339), Selection: selection, FromSHA: from, ToSHA: to, SinceMS: since, UntilMS: until, Commits: []WorkCommit{}, Files: []string{}, Tasks: map[uuid.UUID]TaskWork{}, UntraileredCommits: []string{}, CompletedNotReleased: []TaskWork{}, CIRuns: []CIRun{}, Partial: []string{}}
	shaInput := strings.Builder{}
	for i := 0; i < len(fields); i += 4 {
		sec, e := strconv.ParseInt(fields[i+1], 10, 64)
		if e != nil {
			return nil, e
		}
		stamp := sec * 1000
		if from == nil && (stamp < since || stamp >= until) {
			continue
		}
		ids := map[uuid.UUID]bool{}
		for _, trailer := range strings.FieldsFunc(fields[i+3], func(c rune) bool { return c == '\x1f' || c == '\n' }) {
			u, e := uuid.Parse(strings.TrimSpace(trailer))
			if e != nil || u == uuid.Nil {
				r.Partial = append(r.Partial, "Invalid AmuxID trailer in "+fields[i])
				continue
			}
			ids[u] = true
		}
		ordered := make([]uuid.UUID, 0, len(ids))
		for id := range ids {
			ordered = append(ordered, id)
		}
		sort.Slice(ordered, func(a, b int) bool { return ordered[a].String() < ordered[b].String() })
		commit := WorkCommit{SHA: fields[i], CommittedAt: stamp, Subject: fields[i+2], TaskIDs: ordered}
		r.Commits = append(r.Commits, commit)
		shaInput.WriteString(commit.SHA + "\n")
		if len(ordered) == 0 {
			r.UntraileredCommits = append(r.UntraileredCommits, commit.SHA)
		}
		for _, id := range ordered {
			task := r.Tasks[id]
			task.ID = id
			task.Commits = append(task.Commits, commit.SHA)
			r.Tasks[id] = task
		}
	}
	if len(r.Commits) > 0 {
		cmd := exec.CommandContext(ctx, "git", "-C", root, "--no-optional-locks", "-c", "core.hooksPath=/dev/null", "diff-tree", "--stdin", "--root", "-m", "-r", "--no-commit-id", "--no-renames", "--name-only", "-z")
		cmd.Env = append(cleanEnv(), "GIT_TERMINAL_PROMPT=0")
		cmd.Stdin = strings.NewReader(shaInput.String())
		out, e := cmd.Output()
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("read-only Git report file listing failed: %w", e)
		}
		set := map[string]bool{}
		for _, name := range strings.Split(string(out), "\x00") {
			if name != "" {
				set[name] = true
			}
		}
		for name := range set {
			r.Files = append(r.Files, name)
		}
		sort.Strings(r.Files)
	}
	shallow, err := reportGit(ctx, root, "rev-parse", "--is-shallow-repository")
	if err == nil && strings.TrimSpace(string(shallow)) == "true" {
		r.Partial = append(r.Partial, "Shallow checkout: local history may be incomplete")
	}
	return r, nil
}

// Render returns a concise human-readable rendering of the local report.
func (r *WorkReport) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Work report\n\nSelection: %s\nAs of: %s\nRange: %s..%s\n", r.Selection, r.AsOf, valueOrNone(r.FromSHA), r.ToSHA)
	if len(r.Partial) > 0 {
		b.WriteString("\n## Partial data\n")
		for _, p := range r.Partial {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	b.WriteString("\n## Commits\n")
	for _, c := range r.Commits {
		fmt.Fprintf(&b, "- %s %s\n", c.SHA, c.Subject)
	}
	b.WriteString("\nTask status and review comments are current at retrieval.\n\n## Tasks\n")
	ids := make([]uuid.UUID, 0, len(r.Tasks))
	for id := range r.Tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		work := r.Tasks[id]
		fmt.Fprintf(&b, "\n### %s — %s [%s]\n\n", id, reportString(work.Title, "title unavailable"), reportString(work.Status, "status unavailable"))
		fmt.Fprintf(&b, "Commits: %d\n", len(work.Commits))
		renderReportReview(&b, work)
	}
	b.WriteString("\n## Completed, not released\n")
	b.WriteString("\nNo matching AmuxID commit in this range; untrailered commits may contain this work.\n")
	for _, work := range r.CompletedNotReleased {
		fmt.Fprintf(&b, "\n- %s — %s [%s]\n", work.ID, reportString(work.Title, "title unavailable"), reportString(work.Status, "done"))
		renderReportReview(&b, work)
	}
	b.WriteString("\n## CI observations\n")
	if len(r.CIRuns) == 0 {
		b.WriteString("\nNo CI observations available.\n")
	} else {
		for _, run := range r.CIRuns {
			var name, conclusion, url *string
			_ = json.Unmarshal(run.Name, &name)
			_ = json.Unmarshal(run.Conclusion, &conclusion)
			_ = json.Unmarshal(run.URL, &url)
			fmt.Fprintf(&b, "\n- %s: status=%s, conclusion=%s, attempt=%d, endpoint SHA=%s", reportString(name, "(name unavailable)"), run.Status, reportString(conclusion, "(none)"), run.Attempt, run.HeadSHA)
			if url != nil && *url != "" {
				fmt.Fprintf(&b, " (%s)", *url)
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nCI observations describe runs for the reported endpoint SHA; they do not imply approval or release readiness.\n\n## Untrailered commits\n")
	for _, sha := range r.UntraileredCommits {
		fmt.Fprintf(&b, "- %s\n", sha)
	}
	b.WriteString("\n## Files touched\n")
	for _, f := range r.Files {
		fmt.Fprintf(&b, "- %q\n", f)
	}
	return b.String()
}

func renderReportReview(b *strings.Builder, work TaskWork) {
	if len(work.LatestReview) == 0 || string(work.LatestReview) == "null" {
		b.WriteString("\nReview result: unavailable (no linked review retrieved).\n")
		return
	}
	var review struct {
		TaskID        string `json:"task_id"`
		Status        string `json:"status"`
		LatestComment struct {
			Content string `json:"content"`
		} `json:"latest_comment"`
	}
	if json.Unmarshal(work.LatestReview, &review) != nil {
		b.WriteString("\nReview result: unavailable (invalid review facts).\n")
		return
	}
	fmt.Fprintf(b, "\nReview task %s [%s]; latest non-system comment (verbatim):\n\n%s\n", review.TaskID, reportString(&review.Status, "unknown"), reportString(&review.LatestComment.Content, "No result comment available"))
}

func valueOrNone(s *string) string {
	if s == nil {
		return "(date window)"
	}
	return *s
}

func reportString(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}
