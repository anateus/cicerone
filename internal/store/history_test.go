package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/domain"
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

func TestHistoryScanCursorMigrationPreservesExistingProgress(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := createMigrationFixture(t, path, 17)
	if _, err := db.Exec(`INSERT INTO history_scan_progress(repository,scan_key,commit_hash,event_count,diagnostic_count) VALUES('core','initial','commit',2,1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	progress, err := s.HistoryScanProgress(ctx, "core", "initial")
	if err != nil || len(progress) != 1 || progress[0].Commit != "commit" || progress[0].Events != 2 || progress[0].Diagnostics != 1 {
		t.Fatalf("migrated progress=%#v err=%v", progress, err)
	}
	if _, ok, err := s.HistoryScanCursor(ctx, "core", "initial"); err != nil || ok {
		t.Fatalf("legacy progress unexpectedly has a frontier: ok=%v err=%v", ok, err)
	}
}

func TestHistoryScanCursorAndRowsCommitAtomically(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	since := time.Unix(100, 0).UTC()
	cursor := &HistoryScanCursor{Head: "head", Frontier: "first", Floor: "base", Since: since}
	first := HistoryBatch{Repository: "core", ScanKey: "recent", Cursor: cursor, Processed: []HistoryProgress{{Commit: "first", Events: 1}}}
	if err := s.ApplyHistoryBatch(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_history_cursor BEFORE UPDATE ON history_scan_cursors WHEN NEW.frontier_commit='failed' BEGIN SELECT RAISE(ABORT, 'cursor failure'); END`); err != nil {
		t.Fatal(err)
	}
	failedCursor := *cursor
	failedCursor.Frontier = "failed"
	event := domain.UpdateEvent{ID: "failed", PackageID: "failed", Name: "failed", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: "core", Commit: "failed", Time: since}
	if err := s.ApplyHistoryBatch(ctx, HistoryBatch{Repository: "core", ScanKey: "recent", Cursor: &failedCursor, Processed: []HistoryProgress{{Commit: "failed"}}, Events: []domain.UpdateEvent{event}}); err == nil {
		t.Fatal("expected cursor write to fail")
	}
	got, ok, err := s.HistoryScanCursor(ctx, "core", "recent")
	if err != nil || !ok || got.Frontier != "first" || got.Head != "head" || got.Floor != "base" || !got.Since.Equal(since) {
		t.Fatalf("cursor=%#v ok=%v err=%v", got, ok, err)
	}
	progress, err := s.HistoryScanProgress(ctx, "core", "recent")
	if err != nil || len(progress) != 1 || progress[0].Commit != "first" {
		t.Fatalf("progress after rollback=%#v err=%v", progress, err)
	}
	if found, err := s.HasHistoryEvent(ctx, "core", "failed", domain.EventVersion); err != nil || found {
		t.Fatalf("failed event visible=%v err=%v", found, err)
	}

	other := *cursor
	other.Frontier = "other"
	if err := s.ApplyHistoryBatch(ctx, HistoryBatch{Repository: "core", ScanKey: "fallback", Cursor: &other, Processed: []HistoryProgress{{Commit: "other"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeHistory(ctx, HistoryBatch{Repository: "core", Head: "head", CompletedScanKeys: []string{"recent"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.HistoryScanCursor(ctx, "core", "recent"); err != nil || ok {
		t.Fatalf("completed cursor ok=%v err=%v", ok, err)
	}
	if got, ok, err := s.HistoryScanCursor(ctx, "core", "fallback"); err != nil || !ok || got.Frontier != "other" {
		t.Fatalf("uncompleted cursor=%#v ok=%v err=%v", got, ok, err)
	}
	if err := s.FinalizeHistory(ctx, HistoryBatch{Repository: "core", Head: "head"}); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.HistoryScanCursor(ctx, "core", "fallback"); err != nil || !ok || got.Frontier != "other" {
		t.Fatalf("unrelated cursor after empty completion=%#v ok=%v err=%v", got, ok, err)
	}
	if progress, err := s.HistoryScanProgress(ctx, "core", "fallback"); err != nil || len(progress) != 1 || progress[0].Commit != "other" {
		t.Fatalf("unrelated progress after empty completion=%#v err=%v", progress, err)
	}
	if err := s.FinalizeHistory(ctx, HistoryBatch{Repository: "core", Head: "head", CompletedScanKeys: []string{"fallback"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.HistoryScanCursor(ctx, "core", "fallback"); err != nil || ok {
		t.Fatalf("finalized cursor ok=%v err=%v", ok, err)
	}
}

func TestFinalizeRewriteInvalidatesOnlyAffectedPendingScans(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	old := domain.UpdateEvent{ID: "rewritten", PackageID: "pkg", Name: "pkg", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: "core", Commit: "rewritten", Time: time.Unix(10, 0)}
	affected := HistoryBatch{
		Repository: "core", ScanKey: "fallback",
		Cursor:    &HistoryScanCursor{Head: "old", Frontier: "remaining"},
		Processed: []HistoryProgress{{Commit: "rewritten"}, {Commit: "remaining"}},
		Events:    []domain.UpdateEvent{old},
	}
	if err := s.ApplyHistoryBatch(ctx, affected); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"core", "other"} {
		batch := HistoryBatch{Repository: repo, ScanKey: "unrelated", Cursor: &HistoryScanCursor{Head: "unrelated", Frontier: "unrelated"}, Processed: []HistoryProgress{{Commit: "unrelated"}}}
		if err := s.ApplyHistoryBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ApplyHistoryBatch(ctx, HistoryBatch{Repository: "other", ScanKey: "fallback", Cursor: &HistoryScanCursor{Head: "old", Frontier: "rewritten"}, Processed: []HistoryProgress{{Commit: "rewritten"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeHistory(ctx, HistoryBatch{Repository: "core", Head: "new", RemoveCommits: []string{"rewritten"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.HistoryScanCursor(ctx, "core", "fallback"); err != nil || ok {
		t.Fatalf("affected frontier survived rewrite: ok=%v err=%v", ok, err)
	}
	if progress, err := s.HistoryScanProgress(ctx, "core", "fallback"); err != nil || len(progress) != 1 || progress[0].Commit != "remaining" {
		t.Fatalf("affected progress=%#v err=%v", progress, err)
	}
	for _, repo := range []string{"core", "other"} {
		cursor, ok, err := s.HistoryScanCursor(ctx, repo, "unrelated")
		if err != nil || !ok || cursor.Frontier != "unrelated" {
			t.Fatalf("%s unrelated cursor=%#v ok=%v err=%v", repo, cursor, ok, err)
		}
	}
	if cursor, ok, err := s.HistoryScanCursor(ctx, "other", "fallback"); err != nil || !ok || cursor.Frontier != "rewritten" {
		t.Fatalf("other repository cursor=%#v ok=%v err=%v", cursor, ok, err)
	}
	if found, err := s.HasHistoryEvent(ctx, "core", "pkg", domain.EventVersion); err != nil || found {
		t.Fatalf("rewritten event survived: found=%v err=%v", found, err)
	}
}

func TestFinalizeRewriteRollsBackPendingScanInvalidation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	old := domain.UpdateEvent{ID: "old", PackageID: "pkg", Name: "pkg", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: "core", Commit: "old", Time: time.Unix(10, 0)}
	if err := s.ApplyHistory(ctx, HistoryBatch{Repository: "core", Head: "old", Events: []domain.UpdateEvent{old}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyHistoryBatch(ctx, HistoryBatch{Repository: "core", ScanKey: "fallback", Cursor: &HistoryScanCursor{Head: "old", Frontier: "old"}, Processed: []HistoryProgress{{Commit: "old"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_history_finalize BEFORE UPDATE ON repositories WHEN NEW.head_commit='new' BEGIN SELECT RAISE(ABORT, 'finalize failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeHistory(ctx, HistoryBatch{Repository: "core", Head: "new", RemoveCommits: []string{"old"}}); err == nil {
		t.Fatal("expected finalization failure")
	}
	if found, err := s.HasHistoryEvent(ctx, "core", "pkg", domain.EventVersion); err != nil || !found {
		t.Fatalf("old event after rollback: found=%v err=%v", found, err)
	}
	if cursor, ok, err := s.HistoryScanCursor(ctx, "core", "fallback"); err != nil || !ok || cursor.Frontier != "old" {
		t.Fatalf("cursor after rollback=%#v ok=%v err=%v", cursor, ok, err)
	}
	if progress, err := s.HistoryScanProgress(ctx, "core", "fallback"); err != nil || len(progress) != 1 || progress[0].Commit != "old" {
		t.Fatalf("progress after rollback=%#v err=%v", progress, err)
	}
	if state, ok, err := s.HistoryState(ctx, "core"); err != nil || !ok || state.Head != "old" {
		t.Fatalf("state after rollback=%#v ok=%v err=%v", state, ok, err)
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
