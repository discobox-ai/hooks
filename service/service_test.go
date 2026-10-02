package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	hooks "github.com/discobox-ai/hooks"
	"github.com/discobox-ai/hooks/api/model"
	"github.com/discobox-ai/hooks/models"
	"github.com/discobox-ai/hooks/store"
	"github.com/discobox-ai/hooks/watcher"
)

type testHookSet map[string]hooks.Hook

func (s testHookSet) HookByID(id string) (hooks.Hook, bool) {
	h, ok := s[id]
	return h, ok
}

func TestRunHookSkipsAfterSuccessUnlessForced(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(ctx, t)

	resp, err := svc.RunHook(ctx, "lint", model.RunRequest{})
	if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	if !resp.Enqueued || resp.HookID != "lint" {
		t.Fatalf("unexpected run response: %#v", resp)
	}
	if err := st.Enqueue(ctx, []string{"lint"}, []models.ChangedFile{{Path: "old.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("seed run inputs: %v", err)
	}
	run, err := st.MarkRunning(ctx, "lint", nil)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := st.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusSuccess}); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	resp, err = svc.RunHook(ctx, "lint", model.RunRequest{})
	if err != nil {
		t.Fatalf("run hook after success: %v", err)
	}
	if !resp.Skipped || resp.Reason != "already_succeeded" {
		t.Fatalf("expected success skip, got %#v", resp)
	}

	resp, err = svc.RunHook(ctx, "lint", model.RunRequest{Force: true})
	if err != nil {
		t.Fatalf("forced run hook: %v", err)
	}
	if !resp.Enqueued || resp.Skipped {
		t.Fatalf("expected forced enqueue, got %#v", resp)
	}
	pending, err := st.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if pending == nil || len(pending.ChangedFiles) != 1 || pending.ChangedFiles[0].Path != "old.go" {
		t.Fatalf("expected forced run to copy previous inputs, got %#v", pending)
	}
}

func TestRunHookReplaysFailedRunInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  model.RunRequest
	}{
		{name: "explicit", req: model.RunRequest{}},
		{name: "expanded", req: model.RunRequest{Expanded: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, st := newTestServiceWithHooks(ctx, t, fileHook("vet"))
			if err := st.EnqueueWithChangeIDs(ctx, []string{"vet"}, []models.ChangedFile{{Path: "bad.go", Kind: watcher.Modified}}, []string{"c1"}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			failRun(ctx, t, st, "vet")

			resp, err := svc.RunHook(ctx, "vet", tc.req)
			if err != nil {
				t.Fatalf("run hook: %v", err)
			}
			if !resp.Enqueued || resp.Skipped {
				t.Fatalf("expected failed hook to be re-enqueued, got %#v", resp)
			}
			requirePending(ctx, t, st, "vet", []string{"bad.go"}, []string{"c1"})
		})
	}
}

func TestRunHookMergesChangesRecordedSinceFailedRun(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestServiceWithHooks(ctx, t, fileHook("vet"), fileHook("fmt"))

	// vet: a change lands while the failing run is in flight.
	if err := st.EnqueueWithChangeIDs(ctx, []string{"vet"}, []models.ChangedFile{{Path: "a.go", Kind: watcher.Modified}}, []string{"c1"}); err != nil {
		t.Fatalf("enqueue vet: %v", err)
	}
	run, err := st.MarkRunning(ctx, "vet", nil)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := st.EnqueueWithChangeIDs(ctx, []string{"vet"}, []models.ChangedFile{{Path: "b.go", Kind: watcher.Created}}, []string{"c2"}); err != nil {
		t.Fatalf("enqueue vet during run: %v", err)
	}
	if err := st.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusFailure, ExitCode: 1}); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	resp, err := svc.RunHook(ctx, "vet", model.RunRequest{Expanded: true})
	if err != nil {
		t.Fatalf("run vet: %v", err)
	}
	if !resp.Enqueued {
		t.Fatalf("expected vet enqueue, got %#v", resp)
	}
	requirePending(ctx, t, st, "vet", []string{"a.go", "b.go"}, []string{"c1", "c2"})

	// fmt: a change lands after the failed run, which queues the hook again.
	if err := st.EnqueueWithChangeIDs(ctx, []string{"fmt"}, []models.ChangedFile{{Path: "a.go", Kind: watcher.Modified}}, []string{"c1"}); err != nil {
		t.Fatalf("enqueue fmt: %v", err)
	}
	failRun(ctx, t, st, "fmt")
	if err := st.EnqueueWithChangeIDs(ctx, []string{"fmt"}, []models.ChangedFile{{Path: "c.go", Kind: watcher.Modified}}, []string{"c3"}); err != nil {
		t.Fatalf("enqueue fmt after failure: %v", err)
	}
	resp, err = svc.RunHook(ctx, "fmt", model.RunRequest{Expanded: true})
	if err != nil {
		t.Fatalf("run fmt: %v", err)
	}
	if !resp.Skipped || resp.Reason != "already_queued" {
		t.Fatalf("expected fmt already queued, got %#v", resp)
	}
	requirePending(ctx, t, st, "fmt", []string{"a.go", "c.go"}, []string{"c1", "c3"})
}

func TestRunHookSkipsNeverRunFileHookWithNothingChanged(t *testing.T) {
	ctx := context.Background()
	review := hooks.Hook{ID: "review", Name: "Review", Type: hooks.HookTypeSession, Engine: hooks.HookEngineScript}
	svc, st := newTestServiceWithHooks(ctx, t, fileHook("vet"), fileHook("fmt"), fileHook("lint"), review)

	resp, err := svc.RunHook(ctx, "vet", model.RunRequest{Expanded: true})
	if err != nil {
		t.Fatalf("expanded run: %v", err)
	}
	if !resp.Skipped || resp.Enqueued || resp.Reason != "nothing_changed" {
		t.Fatalf("expected nothing_changed skip, got %#v", resp)
	}
	if rows, err := st.ListPending(ctx, 0); err != nil || len(rows) != 0 {
		t.Fatalf("expected nothing queued, got %#v err=%v", rows, err)
	}

	// Explicit IDs and --force still run a never-run file hook.
	if resp, err := svc.RunHook(ctx, "vet", model.RunRequest{}); err != nil || !resp.Enqueued {
		t.Fatalf("explicit run: resp=%#v err=%v", resp, err)
	}
	if resp, err := svc.RunHook(ctx, "fmt", model.RunRequest{Expanded: true, Force: true}); err != nil || !resp.Enqueued {
		t.Fatalf("forced expanded run: resp=%#v err=%v", resp, err)
	}

	// Pending changes are already queued, not "nothing changed".
	if err := st.Enqueue(ctx, []string{"lint"}, []models.ChangedFile{{Path: "a.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue lint: %v", err)
	}
	if resp, err := svc.RunHook(ctx, "lint", model.RunRequest{Expanded: true}); err != nil || resp.Reason != "already_queued" {
		t.Fatalf("expanded run with pending changes: resp=%#v err=%v", resp, err)
	}

	// Session hooks have no change list to scope to and still run.
	if resp, err := svc.RunHook(ctx, "review", model.RunRequest{Expanded: true}); err != nil || !resp.Enqueued {
		t.Fatalf("expanded session run: resp=%#v err=%v", resp, err)
	}
}

func TestOutputAssemblesLatestRunLogs(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(ctx, t)

	run, err := st.MarkRunning(ctx, "lint", nil)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := st.AppendHookLog(ctx, models.HookLog{HookID: "lint", RunID: run.ID, Line: "first"}); err != nil {
		t.Fatalf("append first log: %v", err)
	}
	if _, err := st.AppendHookLog(ctx, models.HookLog{HookID: "lint", RunID: run.ID, Line: "second"}); err != nil {
		t.Fatalf("append second log: %v", err)
	}

	out, err := svc.Output(ctx, "lint")
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if out.HookID != "lint" || out.Output != "first\nsecond\n" {
		t.Fatalf("unexpected output: %#v", out)
	}
}

func TestRunHookEnqueuesPhaseHooksDirectly(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	review := hooks.Hook{ID: "review", Name: "Review", Type: hooks.HookTypeSession, Engine: hooks.HookEngineScript, Phase: "review"}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{review}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}
	svc, err := New(Config{Store: st, HookSet: testHookSet{"review": review}})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	resp, err := svc.RunHook(ctx, "review", model.RunRequest{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Skipped || !resp.Enqueued {
		t.Fatalf("expected enqueue, got %#v", resp)
	}
}

func TestHookOperationsReturnNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(ctx, t)
	if _, err := svc.SetHookExecution(ctx, "missing", model.ExecutionPatchRequest{Paused: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set execution error = %v, want ErrNotFound", err)
	}
	if _, err := svc.RunHook(ctx, "missing", model.RunRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run error = %v, want ErrNotFound", err)
	}
	if _, err := svc.Output(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("output error = %v, want ErrNotFound", err)
	}
}

func newTestService(ctx context.Context, t *testing.T) (*Service, *store.Store) {
	t.Helper()
	return newTestServiceWithHooks(ctx, t, hooks.Hook{ID: "lint", Name: "Lint", Type: hooks.HookTypeSession, Engine: hooks.HookEngineScript})
}

func newTestServiceWithHooks(ctx context.Context, t *testing.T, defs ...hooks.Hook) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.RefreshDefinitions(ctx, defs); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}
	set := testHookSet{}
	for _, def := range defs {
		set[def.ID] = def
	}
	svc, err := New(Config{Store: st, HookSet: set})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, st
}

func fileHook(id string) hooks.Hook {
	return hooks.Hook{ID: id, Name: id, Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Pattern: "**/*.go"}
}

// failRun runs hookID from its queued inputs and records a failure.
func failRun(ctx context.Context, t *testing.T, st *store.Store, hookID string) {
	t.Helper()
	run, err := st.MarkRunning(ctx, hookID, nil)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := st.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusFailure, ExitCode: 1}); err != nil {
		t.Fatalf("finish run: %v", err)
	}
}

func requirePending(ctx context.Context, t *testing.T, st *store.Store, hookID string, wantPaths []string, wantIDs []string) {
	t.Helper()
	rows, err := st.ListPending(ctx, 0)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	for _, row := range rows {
		if row.HookID != hookID {
			continue
		}
		paths := make([]string, 0, len(row.ChangedFiles))
		for _, f := range row.ChangedFiles {
			paths = append(paths, f.Path)
		}
		if strings.Join(paths, ",") != strings.Join(wantPaths, ",") || strings.Join(row.ChangeIDs, ",") != strings.Join(wantIDs, ",") {
			t.Fatalf("queued %s inputs = %v %v, want %v %v", hookID, paths, row.ChangeIDs, wantPaths, wantIDs)
		}
		return
	}
	t.Fatalf("expected %s to be queued, got %#v", hookID, rows)
}
