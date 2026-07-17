package store

import (
	"context"
	"testing"
	"time"

	"cicerone/internal/domain"
)

func TestApplyHistoryBatchPublishesRowsBeforeFinalize(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	event := domain.UpdateEvent{ID: "new-event", PackageID: "new", Name: "new", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: "core", Commit: "new", Time: time.Unix(20, 0)}
	batch := HistoryBatch{Repository: "core", Path: "/repo", Head: "new", Since: time.Unix(1, 0).UTC(), Events: []domain.UpdateEvent{event}, Aliases: []HistoryAlias{{Alias: "old-name", PackageID: "new", Commit: "new"}}, Diagnostics: []HistoryDiagnostic{{Commit: "new", Message: "note"}}}
	if err := s.ApplyHistoryBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups=%#v err=%v", groups, err)
	}
	if got, err := s.ResolveHistoryPackageID(ctx, "core", "old-name"); err != nil || got != "new" {
		t.Fatalf("alias=%q err=%v", got, err)
	}
	if diagnostics, err := s.HistoryDiagnostics(ctx, "core"); err != nil || len(diagnostics) != 1 {
		t.Fatalf("diagnostics=%#v err=%v", diagnostics, err)
	}
	if state, ok, err := s.HistoryState(ctx, "core"); err != nil || ok {
		t.Fatalf("state=%#v ok=%v err=%v", state, ok, err)
	}
	if err := s.FinalizeHistory(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if state, ok, err := s.HistoryState(ctx, "core"); err != nil || !ok || state.Head != "new" {
		t.Fatalf("final state=%#v ok=%v err=%v", state, ok, err)
	}
}

func TestFinalizeHistoryDefersRewriteDeletion(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	old := domain.UpdateEvent{ID: "old-event", PackageID: "pkg", Name: "pkg", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: "core", Commit: "old", Time: time.Unix(10, 0)}
	if err := s.ApplyHistory(ctx, HistoryBatch{Repository: "core", Head: "old", Events: []domain.UpdateEvent{old}, Aliases: []HistoryAlias{{Alias: "old-name", PackageID: "pkg", Commit: "old"}}, Diagnostics: []HistoryDiagnostic{{Commit: "old", Message: "old"}}}); err != nil {
		t.Fatal(err)
	}
	replacement := old
	replacement.ID, replacement.Commit, replacement.Time = "new-event", "new", time.Unix(20, 0)
	batch := HistoryBatch{Repository: "core", Head: "new", Events: []domain.UpdateEvent{replacement}, RemoveCommits: []string{"old"}}
	if err := s.ApplyHistoryBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	groups, _ := s.QueryFeed(ctx, domain.FeedFilter{})
	if len(groups) != 2 {
		t.Fatalf("before finalize groups=%#v", groups)
	}
	if err := s.FinalizeHistory(ctx, batch); err != nil {
		t.Fatal(err)
	}
	groups, _ = s.QueryFeed(ctx, domain.FeedFilter{})
	if len(groups) != 1 || groups[0].Events[0].ID != "new-event" {
		t.Fatalf("after finalize groups=%#v", groups)
	}
	if got, _ := s.ResolveHistoryPackageID(ctx, "core", "old-name"); got != "old-name" {
		t.Fatalf("old alias survived as %q", got)
	}
}

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
	state, ok, err := s.HistoryState(ctx, "core")
	if err != nil || !ok || state.Head != "c1" || !state.Since.IsZero() {
		t.Fatalf("atomic state=%#v ok=%v err=%v", state, ok, err)
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
