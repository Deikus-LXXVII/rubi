package agenttask

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

func repository(t *testing.T) (home, src string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, src = filepath.Join(root, "home"), filepath.Join(root, "project")
	for _, p := range []string{home, src} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, src, "init", "-b", "main")
	runGit(t, src, "config", "user.name", "Fixture")
	runGit(t, src, "config", "user.email", "fixture@localhost")
	if err := os.WriteFile(filepath.Join(src, "tracked"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "tracked")
	runGit(t, src, "commit", "-m", "base")
	return
}
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestWorkspaceCommitHasNoAccessToSourceOrRemotes(t *testing.T) {
	home, src := repository(t)
	before := runGit(t, src, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(src, "tracked"), []byte("dirty source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "untracked-secret-fixture"), []byte("fake-not-real"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Prepare(context.Background(), home, src, "project-a", "role-a")
	if err != nil {
		t.Fatal(err)
	}
	if runGit(t, w.Dir, "remote") != "" {
		t.Fatal("remote was exposed to provider")
	}
	b, err := os.ReadFile(filepath.Join(w.Dir, "tracked"))
	if err != nil || string(b) != "base\n" {
		t.Fatal("dirty/untracked files reached task")
	}
	if _, err := os.Stat(filepath.Join(w.Dir, "untracked-secret-fixture")); !os.IsNotExist(err) {
		t.Fatal("untracked source file copied")
	}
	if runGit(t, w.Dir, "branch", "--show-current") != w.Branch || w.BaseCommit != before {
		t.Fatal("wrong base or task branch")
	}
	common := runGit(t, w.Dir, "rev-parse", "--git-common-dir")
	if common != ".git" {
		t.Fatal("shared gitdir")
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "tracked"), []byte("task edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, w.Dir, "add", "tracked")
	runGit(t, w.Dir, "commit", "-m", "task")
	if runGit(t, src, "rev-parse", "HEAD") != before {
		t.Fatal("source branch changed")
	}
	b, _ = os.ReadFile(filepath.Join(src, "tracked"))
	if string(b) != "dirty source\n" {
		t.Fatal("source dirty state changed")
	}
	if _, err := Resolve(home, w.ID, "project-a", "role-a", roles.DirHash(src)); err != nil {
		t.Fatal(err)
	}
	listed, err := List(home)
	if err != nil || len(listed) != 1 || listed[0].ID != w.ID {
		t.Fatal("trusted workspace not discoverable", err)
	}
}
func TestWorkspaceBindingRefusesCrossProjectAndMetadataTamper(t *testing.T) {
	for _, scenario := range []string{"project", "role", "source", "config", "alternates", "commondir", "symlink-git", "symlink-object", "symlink-workspace", "symlink-record"} {
		t.Run(scenario, func(t *testing.T) {
			home, src := repository(t)
			w, err := Prepare(context.Background(), home, src, "a", "role")
			if err != nil {
				t.Fatal(err)
			}
			project, role, hash := "a", "role", roles.DirHash(src)
			switch scenario {
			case "project":
				project = "b"
			case "role":
				role = "other"
			case "source":
				hash = roles.DirHash(home)
			case "config":
				runGit(t, w.Dir, "config", "alias.injected", "!exit 1")
			case "alternates", "commondir":
				p := filepath.Join(w.Dir, ".git", scenario)
				if scenario == "alternates" {
					p = filepath.Join(w.Dir, ".git/objects/info/alternates")
				}
				if err := os.WriteFile(p, []byte(src+"/.git"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				target := filepath.Join(w.Dir, ".git")
				if scenario == "symlink-object" {
					target = filepath.Join(w.Dir, ".git/objects/link")
				}
				if scenario == "symlink-workspace" {
					target = w.Dir
				}
				if scenario == "symlink-record" {
					target = filepath.Join(home, "t3/tasks", w.ID+".json")
				}
				if scenario != "symlink-object" {
					if err := os.Rename(target, target+"-old"); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(src, target); err != nil {
					t.Skip(err)
				}
			}
			if _, err := Resolve(home, w.ID, project, role, hash); err == nil {
				t.Fatal("unsafe binding reused")
			}
		})
	}
}
func TestWorkspaceAliasAndRefusals(t *testing.T) {
	home, src := repository(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(src, alias); err != nil {
		t.Skip(err)
	}
	w, err := Prepare(context.Background(), home, alias, "a", "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(home, w.ID, "a", "r", roles.DirHash(src)); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(src, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), home, sub, "a", "r"); err == nil {
		t.Fatal("subdirectory accepted")
	}
	if _, err := Resolve(home, "../../escape", "a", "r", roles.DirHash(src)); err == nil {
		t.Fatal("path injection")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, home, src, "a", "r"); err == nil {
		t.Fatal("cancelled prepare succeeded")
	}
}
func TestWorkspaceDoesNotExecuteGlobalTemplatesOrHooks(t *testing.T) {
	home, src := repository(t)
	attack := t.TempDir()
	marker := filepath.Join(attack, "executed")
	hooks := filepath.Join(attack, "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"post-checkout", "post-commit"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(attack, "config")
	if err := os.WriteFile(config, []byte("[core]\n hooksPath = "+hooks+"\n[init]\n templateDir = "+attack+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hooks)
	w, err := Prepare(context.Background(), home, src, "a", "r")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, w.Dir, "commit", "--allow-empty", "-m", "task")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("host hook executed")
	}
}

func TestSourceLocalTemplatesCannotCopyHomeFilesIntoTask(t *testing.T) {
	home, src := repository(t)
	template := t.TempDir()
	if err := os.WriteFile(filepath.Join(template, "private-fixture"), []byte("fake secret"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "config", "init.templateDir", template)
	w, err := Prepare(context.Background(), home, src, "a", "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.Dir, ".git/private-fixture")); !os.IsNotExist(err) {
		t.Fatal("source git template copied into task")
	}
}
func TestSourceSharedDatabaseAndSymlinkAreRefused(t *testing.T) {
	for _, kind := range []string{"alternate", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			home, src := repository(t)
			path := filepath.Join(src, ".git/objects/info/alternates")
			if kind == "alternate" {
				if err := os.WriteFile(path, []byte(home), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(home, filepath.Join(src, ".git/objects/link")); err != nil {
					t.Skip(err)
				}
			}
			if _, err := Prepare(context.Background(), home, src, "a", "r"); err == nil {
				t.Fatal("unsafe source database copied")
			}
		})
	}
}

func TestWorkspaceRefusesMissingOwner(t *testing.T) {
	home, src := repository(t)
	for _, binding := range [][2]string{{"", "role"}, {"project", ""}} {
		if _, err := Prepare(context.Background(), home, src, binding[0], binding[1]); err == nil {
			t.Fatal("prepared an unowned task")
		}
	}
	if records, err := List(home); err != nil || len(records) != 0 {
		t.Fatal("failed prepare left a binding")
	}
}

func TestTaskPlatformRequiresProvedProcessSupervision(t *testing.T) {
	for _, goos := range []string{"linux"} {
		if err := CheckPlatform(goos); err != nil {
			t.Fatal(err)
		}
	}
	for _, goos := range []string{"darwin", "windows", "freebsd", "plan9", ""} {
		if CheckPlatform(goos) == nil {
			t.Fatalf("unsupported %q allowed repository creation", goos)
		}
	}
}
