package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMachineCheckoutReportsReplaceByHostAndRemainDisplayOnly(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "checkouts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ident := range []string{"amux", "other"} {
		if err := s.InsertProject(ctx, Project{Ident: ident, ChannelName: ident, Kind: "repo"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.UnixMilli(123456)
	origin := "git@github.com:nitecon/amux.git"
	if err := s.ReportMachineCheckouts(ctx, MachineCheckoutReport{Hostname: " Defcon2 ", Checkouts: []CheckoutReportItem{{Project: "AMUX", Origin: &origin}}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportMachineCheckouts(ctx, MachineCheckoutReport{Hostname: "Defcon1", Checkouts: []CheckoutReportItem{{Project: "amux"}, {Project: "other"}}}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	projects, err := s.CheckoutOverviews(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var amux CheckoutOverview
	for _, project := range projects {
		if project.Ident == "amux" {
			amux = project
		}
	}
	if len(amux.Machines) != 2 || amux.Machines[0].Hostname != "Defcon1" || amux.Machines[1].Hostname != "Defcon2" || amux.Machines[1].Origin == nil || *amux.Machines[1].Origin != origin || amux.Agents != 0 || amux.Active != 0 {
		t.Fatalf("overview=%+v", amux)
	}
	if err := s.ReportMachineCheckouts(ctx, MachineCheckoutReport{Hostname: "Defcon1", Checkouts: []CheckoutReportItem{{Project: "other"}}}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	projects, err = s.CheckoutOverviews(ctx, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		for _, machine := range project.Machines {
			if machine.Hostname == "Defcon1" && project.Ident == "amux" {
				t.Fatalf("old host report was retained: %+v", project)
			}
		}
	}
	if err := s.ReportMachineCheckouts(ctx, MachineCheckoutReport{Hostname: "Defcon1"}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	projects, err = s.CheckoutOverviews(ctx, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		for _, machine := range project.Machines {
			if machine.Hostname == "Defcon1" {
				t.Fatalf("empty report did not clear host: %+v", project)
			}
		}
	}
	if err := s.ReportMachineCheckouts(ctx, MachineCheckoutReport{Hostname: " \t "}, now); err == nil {
		t.Fatal("blank hostname accepted")
	}
}

func TestCheckoutAvailabilityClearsWhenHostReportsRecovery(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "checkouts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, Project{Ident: "amux", ChannelName: "amux", Kind: "repo"}); err != nil {
		t.Fatal(err)
	}
	report := MachineCheckoutReport{Hostname: "worker", Checkouts: []CheckoutReportItem{{Project: "amux", Unavailable: true}}}
	now := time.Now()
	if err := s.ReportMachineCheckouts(ctx, report, now); err != nil {
		t.Fatal(err)
	}
	items, err := s.CheckoutOverviews(ctx, now)
	if err != nil || len(items) != 1 || len(items[0].UnavailableHosts) != 1 || items[0].UnavailableHosts[0] != "worker" || len(items[0].Machines) != 0 {
		t.Fatalf("unavailable checkout overview: %+v, %v", items, err)
	}
	report.Checkouts[0].Unavailable = false
	now = now.Add(time.Second)
	if err := s.ReportMachineCheckouts(ctx, report, now); err != nil {
		t.Fatal(err)
	}
	items, err = s.CheckoutOverviews(ctx, now)
	if err != nil || len(items) != 1 || len(items[0].UnavailableHosts) != 0 || len(items[0].Machines) != 1 || items[0].Machines[0].Hostname != "worker" {
		t.Fatalf("recovered checkout overview: %+v, %v", items, err)
	}
}

func TestMachineCheckoutMigrationPreservesRustSchemaAndRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rust.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE machine_checkouts(hostname TEXT NOT NULL,project_ident TEXT NOT NULL,origin TEXT,seen_at INTEGER NOT NULL,PRIMARY KEY(hostname,project_ident));INSERT INTO machine_checkouts VALUES('Old Host','amux','git@github.com:team/amux.git',10);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var hostname, ident, origin string
	var seen int64
	if err := s.db.QueryRow("SELECT hostname,project_ident,origin,seen_at FROM machine_checkouts").Scan(&hostname, &ident, &origin, &seen); err != nil {
		t.Fatal(err)
	}
	if hostname != "Old Host" || ident != "amux" || origin != "git@github.com:team/amux.git" || seen != 10 {
		t.Fatalf("legacy checkout row changed: %q %q %q %d", hostname, ident, origin, seen)
	}
}

func TestMachineCheckoutMigrationPreservesLegacyGoAvailability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-go.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE machine_checkouts(hostname TEXT NOT NULL,project_ident TEXT NOT NULL,origin TEXT,seen_at INTEGER NOT NULL,unavailable INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(hostname,project_ident));INSERT INTO machine_checkouts VALUES('Offline Host','amux','git@github.com:team/amux.git',10,1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var legacyUnavailable, goUnavailable int
	if err := s.db.QueryRow("SELECT unavailable FROM machine_checkouts WHERE hostname='Offline Host'").Scan(&legacyUnavailable); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT unavailable FROM amux_go_machine_checkouts WHERE hostname='Offline Host'").Scan(&goUnavailable); err != nil {
		t.Fatal(err)
	}
	if legacyUnavailable != 1 || goUnavailable != 1 {
		t.Fatalf("unavailable values: source=%d Go-owned=%d", legacyUnavailable, goUnavailable)
	}
}

func TestSafeCheckoutOriginKeepsUsableSourceAndDropsCredentialedOrigins(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  *string
	}{
		{"git@github.com:team/repo.git", ptr("git@github.com:team/repo.git")},
		{"ssh://git@github.com/team/repo.git", ptr("ssh://git@github.com/team/repo.git")},
		{"https://github.com/team/repo.git", ptr("https://github.com/team/repo.git")},
		{"https://token@github.com/team/repo.git", nil},
		{"https://user:secret@github.com/team/repo.git", nil},
		{"https://github.com/team/repo.git?token=secret", nil},
		{"ssh://deploy@github.com/team/repo.git", nil},
	} {
		got := safeCheckoutOrigin(&tc.input)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("safeCheckoutOrigin(%q)=%v want %v", tc.input, got, tc.want)
		}
		if got != nil && (strings.Contains(*got, "secret") || strings.Contains(*got, "token")) {
			t.Errorf("credential leaked in %q", *got)
		}
	}
}
