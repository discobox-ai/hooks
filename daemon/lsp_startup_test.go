package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hooks "github.com/discobox-ai/hooks"
	"github.com/discobox-ai/hooks/manager"
	"github.com/discobox-ai/hooks/models"
	"github.com/discobox-ai/hooks/parser"
	"github.com/discobox-ai/hooks/processhelper"
	"github.com/discobox-ai/hooks/store"
	"github.com/discobox-ai/hooks/watcher"
)

func TestMain(m *testing.M) {
	if handled, code := processhelper.HandleEntry(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// A minimal real stdio server, launched by the repaired hook executable.
func TestLSPStartupServer(t *testing.T) {
	if os.Getenv("HOOKS_TEST_LSP_SERVER") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		var size int
		if _, err := fmt.Fscanf(reader, "Content-Length: %d\r\n", &size); err != nil {
			os.Exit(0)
		}
		if _, err := reader.ReadString('\n'); err != nil {
			os.Exit(1)
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(reader, body); err != nil {
			os.Exit(1)
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			os.Exit(1)
		}
		if msg.Method == "exit" {
			os.Exit(0)
		}
		if len(msg.ID) > 0 {
			reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"capabilities":{}}}`, msg.ID)
			fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(reply), reply)
		}
	}
}

func TestLSPStartupFailureSettlesAndExplicitRetryRecovers(t *testing.T) {
	requireExecutableHooks(t)
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			repo := t.TempDir()
			st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			// A nonexistent command exercises failure before initialization can block.
			hook := hooks.Hook{ID: "lsp", Name: "LSP", Engine: hooks.HookEngineLSP, Type: hooks.HookTypeFile, Pattern: "**/*.go", RelPath: ".discobox/hooks/server", AbsPath: filepath.Join(repo, "server"), LanguageID: "go"}
			shell := hooks.Hook{ID: "shell", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Pattern: "**/*.go"}
			if err := st.RefreshDefinitions(ctx, []hooks.Hook{hook, shell}); err != nil {
				t.Fatal(err)
			}
			mgr, err := manager.New(manager.Config{SessionID: "startup-test", Store: st, Hooks: []hooks.Hook{hook, shell}, RepoRoot: repo})
			if err != nil {
				t.Fatal(err)
			}
			rt := &lspRuntime{hook: hook, open: map[string]struct{}{}}
			r := &runtimeState{ctx: ctx, cfg: Config{RepoRoot: repo}, store: st, manager: mgr, discovery: &parser.Discovery{Hooks: []hooks.Hook{hook, shell}}, lspClients: map[string]*lspRuntime{hook.ID: rt}}
			defer r.closeLSPClients()
			changes := []watcher.Change{{Path: "main.go", Kind: watcher.Modified}}
			if explicit {
				r.activateLSPHook(hook, rt, nil)
			} else {
				r.handleLSPHookChanges(hook, changes)
			}
			await := func(want models.Status) string {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					rows, err := st.ListStatus(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						if row.Hook.ID == hook.ID && row.Status == want {
							rt.mu.Lock()
							starting := rt.starting
							rt.mu.Unlock()
							if !starting {
								return row.LastError
							}
						}
					}
					time.Sleep(time.Millisecond)
				}
				t.Fatalf("LSP never settled as %s", want)
				return ""
			}
			startupError := await(models.StatusFailure)
			if startupError == "" {
				t.Fatal("missing startup error")
			}
			hook.Name = "Renamed LSP"
			if err := st.RefreshDefinitions(ctx, []hooks.Hook{hook, shell}); err != nil {
				t.Fatal(err)
			}
			r.syncLSPHooks()
			if got := await(models.StatusFailure); got != startupError {
				t.Fatalf("definition refresh erased startup error: %q", got)
			}
			for range 20 {
				r.handleLSPHookChanges(hook, changes)
			}
			if got := await(models.StatusFailure); got != startupError {
				t.Fatalf("startup error changed: %q -> %q", startupError, got)
			}
			resp, err := r.waitSnapshot(ctx)
			if err != nil || !resp.Settled || resp.PendingLSP {
				t.Fatalf("failed startup must settle: %+v, %v", resp, err)
			}
			rt.mu.Lock()
			pending := len(rt.pending)
			rt.mu.Unlock()
			if pending != 0 {
				t.Fatalf("failed runtime retained %d changes", pending)
			}
			// A lifecycle failure blocks scripts but is terminal for wait/check.
			if err := st.Enqueue(ctx, []string{shell.ID}, nil); err != nil {
				t.Fatal(err)
			}
			queued, err := st.NextPending(ctx)
			if err != nil || queued != nil {
				t.Fatalf("startup failure did not block script queue: %+v, %v", queued, err)
			}
			resp, err = r.waitSnapshot(ctx)
			if err != nil || !resp.Settled || resp.Queued != 1 {
				t.Fatalf("blocked queue did not settle: %+v, %v", resp, err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\nHOOKS_TEST_LSP_SERVER=1 exec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run '^TestLSPStartupServer$'\n"
			if err := os.WriteFile(hook.AbsPath, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			if ok, err := r.requestLSPRun(hook.ID, false); err != nil || !ok {
				t.Fatalf("retry: %t, %v", ok, err)
			}
			if got := await(models.StatusSuccess); got != "" {
				t.Fatalf("retry retained error: %q", got)
			}
			queued, err = st.NextPending(ctx)
			if err != nil || queued == nil || queued.HookID != shell.ID {
				t.Fatalf("recovery did not release script queue: %+v, %v", queued, err)
			}
		})
	}
}
