package portable

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func TestRewriteCodexSessionCwd(t *testing.T) {
	content := []byte(`{"type":"session_meta","payload":{"id":"abc","cwd":"/old","git":{"branch":"main"}}}` + "\n" +
		`{"type":"response_item","payload":{"role":"user"}}` + "\n")

	got, err := RewriteCodexSessionCwd(content, "/new/path", "feature")
	if err != nil {
		t.Fatalf("RewriteCodexSessionCwd: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if cwd := gjson.Get(lines[0], "payload.cwd").Str; cwd != "/new/path" {
		t.Fatalf("cwd = %q", cwd)
	}
	if branch := gjson.Get(lines[0], "payload.git.branch").Str; branch != "feature" {
		t.Fatalf("branch = %q", branch)
	}
	if !strings.Contains(lines[1], "response_item") {
		t.Fatalf("second line was rewritten unexpectedly: %q", lines[1])
	}
}

func TestResolveRepoPathUsesConfiguredProject(t *testing.T) {
	repo := initGitRepo(t)
	cfg := config.Config{
		PortableResume: config.PortableResumeConfig{
			Repos: []config.PortableResumeRepo{{
				Project: "agentsview",
				Path:    repo,
			}},
		},
	}

	got, err := ResolveRepoPath(cfg, postgres.NativeSessionBlob{
		Project: "agentsview",
	}, "")
	if err != nil {
		t.Fatalf("ResolveRepoPath: %v", err)
	}
	if got != repo {
		t.Fatalf("repo = %q, want %q", got, repo)
	}
}

func TestResolveLocalCwdUsesSourceRelativePath(t *testing.T) {
	tmp := t.TempDir()
	sourceRoot := filepath.Join(tmp, "source", "repo")
	sourceCwd := filepath.Join(sourceRoot, "pkg", "api")
	targetRoot := filepath.Join(tmp, "target", "repo")
	targetCwd := filepath.Join(targetRoot, "pkg", "api")
	if err := os.MkdirAll(targetCwd, 0o755); err != nil {
		t.Fatalf("mkdir target cwd: %v", err)
	}

	got := resolveLocalCwd(targetRoot, postgres.NativeSessionBlob{
		Cwd:            sourceCwd,
		SourceRepoRoot: sourceRoot,
	})
	if got != targetCwd {
		t.Fatalf("cwd = %q, want %q", got, targetCwd)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.CommandContext(
		context.Background(), "git", "init", "-q", "-b", "main",
	)
	cmd.Dir = repo
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	return repo
}
