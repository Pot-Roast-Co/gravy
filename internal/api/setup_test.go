package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/store"
)

func setupFixture(t *testing.T, marker string) (*Local, string, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	home := filepath.Join(root, "gravy")
	if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(marker)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, marker), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := host.NewLocal("local", 1)
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "initial"}} {
		_, code, err := runGit(context.Background(), h, repo, args...)
		if err != nil || code != 0 {
			t.Fatalf("git %v: %d %v", args, code, err)
		}
	}
	db, err := store.Open(context.Background(), filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	n := 0
	l := NewLocal(db, nil, []host.Host{h}, func() string { n++; return fmt.Sprint(n) }).WithSettings(home, config.Default(), nil)
	return l, repo, home
}

func TestSetupDetectionProposesWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		marker string
		checks []string
		os     string
	}{
		{"go.mod", []string{"go build ./...", "go test ./...", "go vet ./..."}, ""},
		{"backend/go.mod", []string{"cd 'backend' && go build ./..."}, ""},
		{"Phone.xcodeproj/project.pbxproj", []string{"xcodebuild test"}, "darwin"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			l, repo, home := setupFixture(t, tc.marker)
			p, err := l.PreviewSetup(context.Background(), AddProjectReq{Path: repo})
			if err != nil {
				t.Fatal(err)
			}
			var checks []string
			for _, st := range p.Project.Validation {
				checks = append(checks, st.Cmd)
			}
			for _, want := range tc.checks {
				if !strings.Contains(strings.Join(checks, "\n"), want) {
					t.Fatalf("checks=%v", checks)
				}
			}
			if tc.os != "" && (!reflect.DeepEqual(p.Project.Requirements.OS, []string{tc.os}) || p.Project.Requirements.Tools["xcodebuild"] != "") {
				t.Fatal(p.Project.Requirements)
			}
			if tc.os != "" {
				if _, ok := p.Project.Requirements.Tools["xcodebuild"]; !ok {
					t.Fatal("missing xcode requirement")
				}
			}
			projects, _ := l.ListProjects(context.Background())
			if len(projects) != 0 {
				t.Fatal("preview persisted project")
			}
			if _, err := os.Stat(config.Path(home)); !os.IsNotExist(err) {
				t.Fatal("preview wrote config")
			}
			if len(p.Evidence) == 0 || len(p.Project.Allowlist.Commands) == 0 {
				t.Fatal("missing suggestions")
			}
		})
	}
}

func approvedSetup(t *testing.T, l *Local, repo string) SetupRequest {
	t.Helper()
	p, err := l.PreviewSetup(context.Background(), AddProjectReq{Path: repo})
	if err != nil {
		t.Fatal(err)
	}
	return SetupRequest{Original: l.cfg, Config: l.cfg, Project: &AddProjectReq{Path: repo, TargetBranch: p.Project.TargetBranch, Allowlist: &p.Project.Allowlist, Validation: p.Project.Validation, Requirements: p.Project.Requirements}}
}

func TestSetupApprovalAndRerunPreserveUnrelatedSettings(t *testing.T) {
	l, repo, home := setupFixture(t, "go.mod")
	l.cfg.Hosts = []config.Host{{ID: "air", Target: "air", Workers: 2}}
	l.cfg.Routes[core.Route("custom")] = []string{"codex/default"}
	req := approvedSetup(t, l, repo)
	req.Config.Concurrency.Workers = 3
	if _, err := l.ApplySetup(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Read(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatal(err)
	}
	projects, _ := l.ListProjects(context.Background())
	if len(projects) != 1 || len(projects[0].Validation) != 3 {
		t.Fatal(projects)
	}
	original := l.cfg
	next := original
	next.Concurrency.Workers = 5
	if _, err := l.ApplySetup(context.Background(), SetupRequest{Original: original, Config: next}); err != nil {
		t.Fatal(err)
	}
	after, _ := l.ListProjects(context.Background())
	if !reflect.DeepEqual(projects, after) {
		t.Fatal("rerun changed projects")
	}
	if !reflect.DeepEqual(l.cfg.Routes, original.Routes) || !reflect.DeepEqual(l.cfg.Hosts, original.Hosts) {
		t.Fatal("rerun clobbered settings")
	}
	if _, err := l.ApplySetup(context.Background(), SetupRequest{Original: original, Config: next}); err == nil {
		t.Fatal("accepted stale draft")
	}
}

func TestSetupSaveFailureRollsBackNewProject(t *testing.T) {
	l, repo, home := setupFixture(t, "go.mod")
	req := approvedSetup(t, l, repo)
	if err := os.MkdirAll(config.Path(home), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ApplySetup(context.Background(), req); err == nil {
		t.Fatal("expected config save failure")
	}
	projects, _ := l.ListProjects(context.Background())
	if len(projects) != 0 {
		t.Fatal("left half-created project")
	}
}

func TestSetupRejectsEmptyRepositoryBeforeWrites(t *testing.T) {
	l, _, home := setupFixture(t, "go.mod")
	allow := core.Allowlist{}
	_, err := l.ApplySetup(context.Background(), SetupRequest{Original: l.cfg, Config: l.cfg, Project: &AddProjectReq{Allowlist: &allow}})
	if err == nil {
		t.Fatal("accepted project without repository")
	}
	if _, err := os.Stat(config.Path(home)); !os.IsNotExist(err) {
		t.Fatal("wrote configuration")
	}
}

func TestSetupMethodsCrossRPC(t *testing.T) {
	c, l, _ := served(t)
	l.WithSettings(t.TempDir(), config.Default(), nil)
	info, err := c.SetupInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Projects) != 1 {
		t.Fatal("lost existing projects")
	}
	next := copyConfig(info.Settings.Config)
	next.Concurrency.Workers = 8
	st, err := c.ApplySetup(context.Background(), SetupRequest{Original: info.Settings.Config, Config: next})
	if err != nil {
		t.Fatal(err)
	}
	if st.Config.Concurrency.Workers != 8 {
		t.Fatal("settings did not cross RPC")
	}
	if _, err := c.PreviewSetup(context.Background(), AddProjectReq{Path: "/nonexistent"}); err == nil {
		t.Fatal("preview accepted invalid repository")
	}
}
