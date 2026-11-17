package cel_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/policy"
	"github.com/NimbleEngineer21/tokenwarden/policy/cel"
)

const (
	allowAll = "version: 1\nrules:\n- name: all\n  effect: allow\n"
	denyAll  = "version: 1\nrules:\n- name: none\n  effect: deny\n  reason: closed\n"
)

func decideRule(t *testing.T, e *cel.Engine) string {
	t.Helper()
	d, err := e.Decide(t.Context(), policy.Input{Team: "red"})
	if err != nil {
		t.Fatal(err)
	}
	return d.Rule
}

func TestReload(t *testing.T) {
	e, path, _ := load(t, allowAll)
	before := e.Revision()

	writeFile(t, path, "version: 1\nrules:\n- name: bad\n  effect: allow\n  expr: nope\n")
	if err := e.Reload(); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Reload() error = %v, want a compile error", err)
	}
	if e.Revision() != before || decideRule(t, e) != "all" {
		t.Fatal("a failed reload replaced the rules")
	}

	writeFile(t, path, denyAll)
	if err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	if e.Revision() == before || decideRule(t, e) != "none" {
		t.Fatal("Reload() did not swap in the new rules")
	}
}

func TestReloadConcurrentWithDecide(t *testing.T) {
	e, path, _ := load(t, allowAll)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				if r := decideRule(t, e); r != "all" && r != "none" {
					t.Errorf("rule = %q", r)
				}
			}
		})
	}
	for i := range 20 {
		writeFile(t, path, []string{allowAll, denyAll}[i%2])
		if err := e.Reload(); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
}

func TestWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, path, logs := load(t, allowAll)
		go e.Watch(t.Context(), time.Second)

		tick := func() {
			time.Sleep(time.Second)
			synctest.Wait()
		}

		writeFile(t, path, denyAll)
		tick()
		if got := decideRule(t, e); got != "none" {
			t.Fatalf("after a change: rule = %q, want none", got)
		}

		writeFile(t, path, "version: 1\nmode: loud\n")
		tick()
		tick()
		tick()
		if got := decideRule(t, e); got != "none" {
			t.Fatalf("after a bad file: rule = %q, want the old rules (none)", got)
		}
		if n := strings.Count(logs.String(), "policy reload failed"); n != 1 {
			t.Fatalf("a bad file was logged %d times, want 1:\n%s", n, logs)
		}

		writeFile(t, path, allowAll)
		tick()
		if got := decideRule(t, e); got != "all" {
			t.Fatalf("after a fix: rule = %q, want all", got)
		}
		if n := strings.Count(logs.String(), "policy loaded"); n != 3 {
			t.Fatalf("policy loaded %d times, want 3 (start, change, fix):\n%s", n, logs)
		}
	})
}

// TestWatchSymlinkSwap copies how a Kubernetes ConfigMap volume updates a
// file: the file is a symlink into a data directory, and the update points
// the symlink at a new directory. The modification time of the new file
// can be older than the last poll, so only a content check sees it.
func TestWatchSymlinkSwap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		for name, data := range map[string]string{"v1": allowAll, "v2": denyAll} {
			if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, name, "policy.yaml"), data)
		}
		link := func(target string) {
			tmp := filepath.Join(dir, "..data_tmp")
			if err := os.Symlink(target, tmp); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
				t.Fatal(err)
			}
		}
		link("v1")
		if err := os.Symlink(filepath.Join("..data", "policy.yaml"), filepath.Join(dir, "policy.yaml")); err != nil {
			t.Fatal(err)
		}

		e, err := cel.Load(filepath.Join(dir, "policy.yaml"), cel.WithLogger(slog.New(slog.DiscardHandler)))
		if err != nil {
			t.Fatal(err)
		}
		go e.Watch(t.Context(), time.Second)
		link("v2")
		time.Sleep(time.Second)
		synctest.Wait()
		if got := decideRule(t, e); got != "none" {
			t.Fatalf("after the swap: rule = %q, want none", got)
		}
	})
}
