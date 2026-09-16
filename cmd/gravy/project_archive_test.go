package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// projectLister answers ListProjects and nothing else. The embedded interface is nil on purpose:
// a call to any other method is a test asking for something these two functions have no business
// doing, and a nil panic says so louder than a stub returning zero values.
type projectLister struct {
	api.Service
	projects []core.Project
}

func (l projectLister) ListProjects(_ context.Context, f api.ProjectFilter) ([]core.Project, error) {
	if f.IncludeArchived {
		return l.projects, nil
	}
	var out []core.Project
	for _, p := range l.projects {
		if !p.Archived {
			out = append(out, p)
		}
	}
	return out, nil
}

// TestResolveProjectPrefersTheWorkingSet covers the rule that decides where a ticket typed with
// no -project goes.
//
// The adversarial case is the one that quietly does nothing useful: a human archives the
// repository they finished, `gravy ticket add` finds exactly one project left, and files the
// ticket into the archived one — where it sits Ready forever because the scheduler skips it.
// Archived projects are nameable but never the implicit default.
func TestResolveProjectPrefersTheWorkingSet(t *testing.T) {
	tests := []struct {
		name     string
		projects []core.Project
		slug     string
		want     string
		wantErr  string
	}{
		{
			name: "the only working project is the default",
			projects: []core.Project{
				{ID: "p1", Slug: "gravy"},
				{ID: "p2", Slug: "acorn", Archived: true},
			},
			want: "gravy",
		},
		{
			name: "an archived project still resolves when it is named",
			projects: []core.Project{
				{ID: "p1", Slug: "gravy"},
				{ID: "p2", Slug: "acorn", Archived: true},
			},
			slug: "acorn",
			want: "acorn",
		},
		{
			name: "two working projects still need naming",
			projects: []core.Project{
				{ID: "p1", Slug: "gravy"},
				{ID: "p2", Slug: "mojo"},
				{ID: "p3", Slug: "acorn", Archived: true},
			},
			wantErr: "several projects registered",
		},
		{
			name: "everything archived says how to come back",
			projects: []core.Project{
				{ID: "p1", Slug: "gravy", Archived: true},
			},
			wantErr: "unarchive",
		},
		{
			name:    "nothing registered says how to start",
			wantErr: "add one with",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProject(context.Background(), projectLister{projects: tc.projects}, tc.slug)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolved %q, want an error mentioning %q", got.Slug, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveProject: %v", err)
			}
			if got.Slug != tc.want {
				t.Errorf("resolved %q, want %q", got.Slug, tc.want)
			}
		})
	}
}

// TestProjectSubcommandDefaultsToListing covers the dispatch, whose failure mode is not a bad
// error message but a panic.
//
// `gravy project` on its own has always listed the projects. Once archive and unarchive were
// added beside it the default branch still had to hand the remaining arguments to the listing,
// and there are no remaining arguments — taking args[1:] of an empty list is an out-of-range
// slice, so the command that used to list projects crashes instead.
func TestProjectSubcommandDefaultsToListing(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantSub  string
		wantRest []string
	}{
		{name: "no arguments at all lists", args: nil, wantSub: ""},
		{name: "empty but non-nil lists", args: []string{}, wantSub: ""},
		{name: "a leading flag is the listing's", args: []string{"--all"}, wantSub: "",
			wantRest: []string{"--all"}},
		{name: "listing named explicitly", args: []string{"list"}, wantSub: "list"},
		{name: "listing with its flag", args: []string{"list", "--all"}, wantSub: "list",
			wantRest: []string{"--all"}},
		{name: "archive takes its project", args: []string{"archive", "gravy"},
			wantSub: "archive", wantRest: []string{"gravy"}},
		{name: "unarchive takes its project", args: []string{"unarchive", "gravy"},
			wantSub: "unarchive", wantRest: []string{"gravy"}},
		{name: "an unknown word stays the subcommand", args: []string{"frobnicate"},
			wantSub: "frobnicate"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub, rest := projectSubcommand(tc.args)
			if sub != tc.wantSub {
				t.Errorf("sub = %q, want %q", sub, tc.wantSub)
			}
			if strings.Join(rest, " ") != strings.Join(tc.wantRest, " ") {
				t.Errorf("rest = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// TestFindProjectSeesArchivedProjects, because `gravy project unarchive <slug>` is the way back
// and it can only name a project the lookup can find.
func TestFindProjectSeesArchivedProjects(t *testing.T) {
	svc := projectLister{projects: []core.Project{
		{ID: "p1", Slug: "gravy"},
		{ID: "p2", Slug: "acorn", Archived: true},
	}}

	for _, ref := range []string{"acorn", "p2"} {
		got, err := findProject(context.Background(), svc, ref)
		if err != nil {
			t.Fatalf("findProject(%q): %v", ref, err)
		}
		if got.ID != "p2" {
			t.Errorf("findProject(%q) = %+v, want the archived project", ref, got)
		}
	}

	if _, err := findProject(context.Background(), svc, "nope"); err == nil {
		t.Error("an unregistered name resolved")
	}
}
