package worksubmission

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
)

func TestBuildLocalReportGroupsAmuxTrailersFilesAndUntraileredCommitsReadOnly(t *testing.T) {
	f := newFixture(t)
	f.git(t, f.root, "tag", "v1.2.3")
	write(t, filepath.Join(f.root, "report file.txt"), "work\n")
	f.git(t, f.root, "add", "report file.txt")
	f.git(t, f.root, "commit", "-m", "linked work\n\nAmuxID: "+taskID()+"\nAmuxID: "+taskID())
	linked := f.head(t)
	f.git(t, f.root, "commit", "--allow-empty", "-m", "unlinked work")
	f.git(t, f.root, "tag", "v1.2.4")
	before := f.git(t, f.root, "status", "--porcelain=v1")
	r, err := BuildLocalReport(context.Background(), f.root, ReportOptions{Range: "v1.2.3..v1.2.4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Commits) != 2 || len(r.Tasks) != 1 || len(r.Tasks[uuidParse(t, taskID())].Commits) != 1 {
		t.Fatalf("unexpected report: %+v", r)
	}
	if len(r.UntraileredCommits) != 1 || r.UntraileredCommits[0] != f.head(t) {
		t.Fatalf("untrailered = %v", r.UntraileredCommits)
	}
	if len(r.Files) != 1 || r.Files[0] != "report file.txt" {
		t.Fatalf("files = %q", r.Files)
	}
	if r.Commits[0].SHA != linked || !strings.Contains(r.Render(), "Files touched") {
		t.Fatalf("unexpected report rendering: %s", r.Render())
	}
	if got := f.git(t, f.root, "status", "--porcelain=v1"); got != before {
		t.Fatalf("report changed working tree: before %q after %q", before, got)
	}
}

func TestBuildLocalReportDateBoundsAndRejectsUnsafeOrInvalidSelections(t *testing.T) {
	f := newFixture(t)
	t.Setenv("GIT_AUTHOR_DATE", "2024-01-02T12:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2024-01-02T12:00:00Z")
	f.git(t, f.root, "commit", "--allow-empty", "-m", "inside")
	inside := f.head(t)
	cmd := f.git(t, f.root, "rev-parse", inside)
	if cmd == "" {
		t.Fatal("missing fixture commit")
	}
	r, err := BuildLocalReport(context.Background(), f.root, ReportOptions{Since: "2024-01-02", Until: "2024-01-03T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Commits) != 1 || r.Commits[0].SHA != inside {
		t.Fatalf("date window commits = %+v", r.Commits)
	}
	for _, opts := range []ReportOptions{{Range: "--no-such-ref..HEAD"}, {Range: "HEAD...HEAD"}, {Since: "2024-02-02", Until: "2024-02-01"}, {Since: "yesterday", Until: "2024-02-01"}, {Range: "HEAD..HEAD", Since: "2024-01-01", Until: "2024-01-02"}} {
		if _, err := BuildLocalReport(context.Background(), f.root, opts); err == nil {
			t.Fatalf("invalid selection accepted: %+v", opts)
		}
	}
}

func TestBuildLocalReportHandlesRustLargeRangeContract(t *testing.T) {
	f := newFixture(t)
	f.git(t, f.root, "tag", "v1.2.3")
	for i := 0; i < 310; i++ {
		write(t, filepath.Join(f.root, fmt.Sprintf("file %d", i)), "one\n")
	}
	f.git(t, f.root, "add", ".")
	f.git(t, f.root, "commit", "-m", "bulk\n\nAmuxID: "+taskID())
	for i := 0; i < 251; i++ {
		f.git(t, f.root, "commit", "--allow-empty", "-m", fmt.Sprintf("extra %d", i))
	}
	write(t, filepath.Join(f.root, "file 0"), "two\n")
	f.git(t, f.root, "commit", "-am", "two tasks\n\nAmuxID: "+taskID()+"\nAmuxID: "+taskID()+"\nAmuxID: "+otherReportTaskID)
	f.git(t, f.root, "commit", "--allow-empty", "-m", "invalid\n\nAmuxID: not-an-id")
	f.git(t, f.root, "tag", "v1.2.4")
	r, err := BuildLocalReport(context.Background(), f.root, ReportOptions{Range: "v1.2.3..v1.2.4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Commits) != 254 || len(r.Files) != 310 || len(r.Tasks[uuidParse(t, taskID())].Commits) != 2 || len(r.Tasks[uuidParse(t, otherReportTaskID)].Commits) != 1 || len(r.UntraileredCommits) != 252 {
		t.Fatalf("large range mismatch: commits=%d files=%d tasks=%v untrailered=%d", len(r.Commits), len(r.Files), r.Tasks, len(r.UntraileredCommits))
	}
	if len(r.Partial) != 1 || !strings.Contains(r.Partial[0], "Invalid AmuxID") {
		t.Fatalf("partial markers = %v", r.Partial)
	}
}

const otherReportTaskID = "01a1986d-8c70-778a-ad46-316ec310c1b6"
const unlinkedReportTaskID = "01a1986d-8c70-778a-ad46-316ec310c1b8"
const olderReportReviewID = "01a1986d-8c70-778a-ad46-316ec310c1b9"

func TestEnrichGatewayUsesGETForTaskAndReviewFacts(t *testing.T) {
	task, review := taskID(), otherReportTaskID
	var methods []string
	detailComments := map[string]string{}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method != "GET" {
			t.Errorf("unexpected gateway method %s", r.Method)
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			if r.URL.Query().Get("include_stale") != "true" || r.URL.Query().Get("status") != "todo,in_progress,user_review,done,cancelled" {
				t.Errorf("task list query omitted stale/status coverage: %s", r.URL.RawQuery)
			}
			_, _ = fmt.Fprintf(w, `[{"id":%q,"status":"done","title":"Work","created_at":1,"updated_at":100,"labels":[]},{"id":%q,"status":"done","created_at":100,"updated_at":200,"labels":["task-review","review-of:%s"]},{"id":%q,"status":"done","created_at":40,"updated_at":300,"labels":["task-review","review-of:%s"]},{"id":%q,"status":"done","title":"Unlinked completion","created_at":1,"updated_at":100,"labels":[]},{"id":"01a1986d-8c70-778a-ad46-316ec310c1b7","status":"done","created_at":101,"updated_at":101,"labels":[]}]`, task, review, task, olderReportReviewID, task, unlinkedReportTaskID)
		case strings.HasSuffix(r.URL.Path, "/"+task):
			detailComments[task] = r.URL.Query().Get("comments")
			_, _ = fmt.Fprintf(w, `{"id":%q,"project_ident":"demo","title":"Work","status":"done","done_at":50,"labels":[]}`, task)
		case strings.HasSuffix(r.URL.Path, "/"+review):
			detailComments[review] = r.URL.Query().Get("comments")
			_, _ = fmt.Fprintf(w, `{"id":%q,"project_ident":"demo","title":"Review","status":"done","created_at":100,"updated_at":200,"labels":["task-review","review-of:%s"],"comments":[{"author_type":"agent","content":"No findings","created_at":150},{"author_type":"system","content":"Later reminder","created_at":250}]}`, review, task)
		case strings.HasSuffix(r.URL.Path, "/"+olderReportReviewID):
			detailComments[olderReportReviewID] = r.URL.Query().Get("comments")
			_, _ = fmt.Fprintf(w, `{"id":%q,"project_ident":"demo","title":"Old review","status":"done","created_at":40,"updated_at":300,"labels":["task-review","review-of:%s"],"comments":[{"author_type":"agent","content":"Old result","created_at":55}]}`, olderReportReviewID, task)
		case strings.HasSuffix(r.URL.Path, "/"+unlinkedReportTaskID):
			detailComments[unlinkedReportTaskID] = r.URL.Query().Get("comments")
			_, _ = fmt.Fprintf(w, `{"id":%q,"project_ident":"demo","title":"Unlinked completion","status":"done","done_at":50,"labels":[]}`, unlinkedReportTaskID)
		default:
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "contract-key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	r := &WorkReport{SinceMS: 0, UntilMS: 100, FromSHA: ptrString("from"), Tasks: map[uuid.UUID]TaskWork{uuidParse(t, task): {ID: uuidParse(t, task), Commits: []string{"sha"}}}, CompletedNotReleased: []TaskWork{}, Partial: []string{}}
	if err := r.EnrichGateway(context.Background(), client, "demo"); err != nil {
		t.Fatal(err)
	}
	work := r.Tasks[uuidParse(t, task)]
	if work.Title == nil || *work.Title != "Work" || work.Status == nil || *work.Status != "done" || !strings.Contains(string(work.LatestReview), review) || !strings.Contains(string(work.LatestReview), "No findings") || strings.Contains(string(work.LatestReview), "Later reminder") || strings.Contains(string(work.LatestReview), "Old result") {
		t.Fatalf("gateway task/review facts: %+v", work)
	}
	if detailComments[task] != "0" || detailComments[review] != "100" || detailComments[olderReportReviewID] != "100" || detailComments[unlinkedReportTaskID] != "0" {
		t.Fatalf("detail comment limits = %v", detailComments)
	}
	if len(r.CompletedNotReleased) != 1 || r.CompletedNotReleased[0].ID.String() != unlinkedReportTaskID {
		t.Fatalf("unlinked completion facts = %+v", r.CompletedNotReleased)
	}
	if _, fetched := detailComments["01a1986d-8c70-778a-ad46-316ec310c1b7"]; fetched {
		t.Fatal("out-of-window task detail was fetched")
	}
	rendered := r.Render()
	for _, want := range []string{"Work", "[done]", "Review task " + review + " [done]", "No findings", "No matching AmuxID commit in this range; untrailered commits may contain this work."} {
		if !strings.Contains(rendered, want) {
			t.Errorf("human report missing %q:\n%s", want, rendered)
		}
	}
	for _, method := range methods {
		if method != "GET" {
			t.Fatalf("non-GET method used: %v", methods)
		}
	}
}

func ptrString(s string) *string { return &s }

func uuidParse(t *testing.T, value string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
