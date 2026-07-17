package store

import (
	"cicerone/internal/domain"
	"context"
	"testing"
	"time"
)

func TestHistoryCoverageAndAliasesAreRepositoryScoped(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	at := time.Unix(10, 0).UTC()
	e := domain.UpdateEvent{ID: "a", PackageID: "new", Name: "new", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: "one", Commit: "c1", Time: at}
	if err := s.ApplyHistory(ctx, HistoryBatch{Repository: "one", Head: "c1", Events: []domain.UpdateEvent{e}, Aliases: []HistoryAlias{{Alias: "old", PackageID: "new", Repository: "one", Commit: "c1"}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		repo string
		want bool
	}{{"one", true}, {"two", false}} {
		got, err := s.HasHistoryEvent(ctx, tc.repo, "new", domain.EventVersion)
		if err != nil || got != tc.want {
			t.Fatalf("coverage %s=%v err=%v", tc.repo, got, err)
		}
	}
	if got, err := s.ResolveHistoryPackageID(ctx, "one", "old"); err != nil || got != "new" {
		t.Fatalf("resolved=%q err=%v", got, err)
	}
	if got, err := s.ResolveHistoryPackageID(ctx, "two", "old"); err != nil || got != "old" {
		t.Fatalf("other source resolved=%q err=%v", got, err)
	}
}

func TestHistoryDiagnosticsPersistWithoutEventsAndRollbackAtomically(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	d := HistoryDiagnostic{Repository: "core", Commit: "c1", Path: "Formula/foo.rb", Message: "ambiguous"}
	bad := domain.UpdateEvent{ID: "bad", PackageID: "bad", Name: "bad", Type: "invalid", Kind: domain.EventVersion, Repository: "core", Commit: "c1", Time: time.Now()}
	err := s.ApplyHistory(ctx, HistoryBatch{Repository: "core", Head: "c1", Diagnostics: []HistoryDiagnostic{d}, Events: []domain.UpdateEvent{bad}})
	if err == nil {
		t.Fatal("expected constraint failure")
	}
	got, err := s.HistoryDiagnostics(ctx, "core")
	if err != nil || len(got) != 0 {
		t.Fatalf("diagnostics after rollback=%#v err=%v", got, err)
	}
	if _, ok, err := s.HistoryState(ctx, "core"); err != nil || ok {
		t.Fatalf("cursor visible after rollback ok=%v err=%v", ok, err)
	}
	if err := s.ApplyHistory(ctx, HistoryBatch{Repository: "core", Head: "c1", Diagnostics: []HistoryDiagnostic{d}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.HistoryDiagnostics(ctx, "core")
	if err != nil || len(got) != 1 {
		t.Fatalf("diagnostics=%#v err=%v", got, err)
	}
}

func TestRewriteRemovesOnlyAffectedSourceAliasAndDiagnostic(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	at := time.Unix(10, 0)
	for n, repo := range []string{"one", "two"} {
		e := domain.UpdateEvent{ID: domain.EventID(repo), PackageID: domain.PackageID("new" + repo), Name: "new", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: repo, Commit: "old", Time: at}
		err := s.ApplyHistory(ctx, HistoryBatch{Repository: repo, Head: "old", Events: []domain.UpdateEvent{e}, Aliases: []HistoryAlias{{Alias: "old", PackageID: e.PackageID, Commit: "old"}}, Diagnostics: []HistoryDiagnostic{{Commit: "old", Message: "d"}}})
		if err != nil {
			t.Fatalf("source %d: %v", n, err)
		}
	}
	if err := s.ApplyHistory(ctx, HistoryBatch{Repository: "one", Head: "new", RemoveCommits: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ResolveHistoryPackageID(ctx, "one", "old"); got != "old" {
		t.Fatalf("rewritten alias survived: %q", got)
	}
	if got, _ := s.ResolveHistoryPackageID(ctx, "two", "old"); got != "newtwo" {
		t.Fatalf("other source alias removed: %q", got)
	}
	if got, _ := s.HistoryDiagnostics(ctx, "one"); len(got) != 0 {
		t.Fatalf("rewritten diagnostics=%#v", got)
	}
	if got, _ := s.HistoryDiagnostics(ctx, "two"); len(got) != 1 {
		t.Fatalf("other diagnostics=%#v", got)
	}
}
