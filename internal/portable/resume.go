package portable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/postgres"
)

// ResumeOptions controls a portable Codex resume preparation.
type ResumeOptions struct {
	SessionID     string
	SourceMachine string
	RepoPath      string
}

// ResumeResult describes the local restore and command.
type ResumeResult struct {
	SessionID       string
	RawSessionID    string
	SourceMachine   string
	Project         string
	RepoPath        string
	Cwd             string
	DestinationPath string
	Command         string
}

// PrepareCodexResume fetches a native Codex transcript from PG,
// restores it into the local Codex sessions directory, and returns
// the command needed to resume it from the resolved local cwd.
func PrepareCodexResume(
	ctx context.Context, pgURL, schema string,
	cfg config.Config, allowInsecure bool,
	opts ResumeOptions,
) (ResumeResult, error) {
	pg, err := postgres.Open(pgURL, schema, allowInsecure)
	if err != nil {
		return ResumeResult{}, err
	}
	defer pg.Close()

	fullID, rawID := NormalizeCodexSessionID(opts.SessionID)
	blob, err := selectNativeCodexBlob(
		ctx, pg, fullID, opts.SourceMachine,
	)
	if err != nil {
		return ResumeResult{}, err
	}
	if blob == nil {
		return ResumeResult{}, fmt.Errorf(
			"no portable Codex transcript found for %s; enable [portable_resume].upload_native_transcripts on the source machine and run agentsview pg push --full",
			fullID,
		)
	}
	if blob.Agent != string(parser.AgentCodex) {
		return ResumeResult{}, fmt.Errorf(
			"portable resume only supports Codex sessions",
		)
	}

	repoPath, err := ResolveRepoPath(cfg, *blob, opts.RepoPath)
	if err != nil {
		return ResumeResult{}, err
	}
	cwd := resolveLocalCwd(repoPath, *blob)
	if err := validateGitBranch(ctx, repoPath, blob.GitBranch); err != nil {
		return ResumeResult{}, err
	}

	rewritten, err := RewriteCodexSessionCwd(
		blob.Content, cwd, blob.GitBranch,
	)
	if err != nil {
		return ResumeResult{}, err
	}
	dest, err := restoreCodexTranscript(
		cfg, rawID, *blob, rewritten,
	)
	if err != nil {
		return ResumeResult{}, err
	}

	return ResumeResult{
		SessionID:       fullID,
		RawSessionID:    rawID,
		SourceMachine:   blob.SourceMachine,
		Project:         blob.Project,
		RepoPath:        repoPath,
		Cwd:             cwd,
		DestinationPath: dest,
		Command:         commandWithCwd("codex resume "+shellQuote(rawID), cwd),
	}, nil
}

// NormalizeCodexSessionID accepts bare, codex-prefixed, or
// host-prefixed IDs and returns the PG session ID plus raw Codex ID.
func NormalizeCodexSessionID(id string) (fullID, rawID string) {
	_, raw := parser.StripHostPrefix(strings.TrimSpace(id))
	if raw == "" {
		raw = strings.TrimSpace(id)
	}
	rawID = strings.TrimPrefix(raw, string(parser.AgentCodex)+":")
	return string(parser.AgentCodex) + ":" + rawID, rawID
}

func selectNativeCodexBlob(
	ctx context.Context, pg *sql.DB,
	sessionID, sourceMachine string,
) (*postgres.NativeSessionBlob, error) {
	if sourceMachine != "" {
		return postgres.GetNativeSessionBlob(
			ctx, pg, sessionID, sourceMachine,
		)
	}
	candidates, err := postgres.ListNativeSessionBlobs(
		ctx, pg, sessionID,
	)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return nativeCandidateAfter(candidates[i], candidates[j])
	})
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.ContentSHA256 == best.ContentSHA256 {
			continue
		}
		if sameNativeCandidateTime(c, best) {
			return nil, fmt.Errorf(
				"multiple divergent portable transcripts exist for %s; rerun with --source-machine",
				sessionID,
			)
		}
		break
	}
	return postgres.GetNativeSessionBlob(
		ctx, pg, sessionID, best.SourceMachine,
	)
}

func nativeCandidateAfter(
	a, b postgres.NativeSessionBlobSummary,
) bool {
	at := nativeCandidateTime(a)
	bt := nativeCandidateTime(b)
	if !at.Equal(bt) {
		return at.After(bt)
	}
	return a.SourceMachine < b.SourceMachine
}

func sameNativeCandidateTime(
	a, b postgres.NativeSessionBlobSummary,
) bool {
	return nativeCandidateTime(a).Equal(nativeCandidateTime(b))
}

func nativeCandidateTime(
	c postgres.NativeSessionBlobSummary,
) time.Time {
	if c.SourceMtime != nil {
		return c.SourceMtime.UTC()
	}
	return c.UpdatedAt.UTC()
}

// ResolveRepoPath resolves the target machine's local repository path.
func ResolveRepoPath(
	cfg config.Config, blob postgres.NativeSessionBlob, override string,
) (string, error) {
	if override != "" {
		return validateRepoPath(override)
	}
	for _, repo := range cfg.PortableResume.Repos {
		if repo.Project == blob.Project && repo.Path != "" {
			return validateRepoPath(repo.Path)
		}
	}
	if blob.Cwd != "" {
		if root, ok := gitRepoRoot(context.Background(), blob.Cwd); ok {
			return root, nil
		}
	}
	for _, root := range cfg.PortableResume.RepoRoots {
		for _, name := range repoNameCandidates(blob.Project) {
			candidate := filepath.Join(root, name)
			if repo, ok := gitRepoRoot(
				context.Background(), candidate,
			); ok {
				return repo, nil
			}
		}
	}
	return "", fmt.Errorf(
		"could not resolve local repo for project %q; configure [portable_resume] repo_roots or [[portable_resume.repos]]",
		blob.Project,
	)
}

func repoNameCandidates(project string) []string {
	project = strings.TrimSpace(project)
	if project == "" {
		return nil
	}
	candidates := []string{project}
	hyphen := strings.ReplaceAll(project, "_", "-")
	if hyphen != project {
		candidates = append(candidates, hyphen)
	}
	return candidates
}

func validateRepoPath(path string) (string, error) {
	repo, ok := gitRepoRoot(context.Background(), path)
	if !ok {
		return "", fmt.Errorf(
			"%s is not a git repository; pull the project locally or configure the correct path",
			path,
		)
	}
	return repo, nil
}

func gitRepoRoot(ctx context.Context, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		gitCtx, "git", "rev-parse", "--show-toplevel",
	)
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(out))
	return root, root != ""
}

func resolveLocalCwd(
	repoPath string, blob postgres.NativeSessionBlob,
) string {
	if blob.Cwd == "" || blob.SourceRepoRoot == "" {
		return repoPath
	}
	rel, err := filepath.Rel(blob.SourceRepoRoot, blob.Cwd)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return repoPath
	}
	candidate := filepath.Join(repoPath, rel)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return repoPath
}

func validateGitBranch(
	ctx context.Context, repoPath, expected string,
) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		gitCtx, "git", "rev-parse", "--abbrev-ref", "HEAD",
	)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("checking git branch in %s: %w", repoPath, err)
	}
	current := strings.TrimSpace(string(out))
	if current != expected {
		return fmt.Errorf(
			"repo %s is on branch %q; checkout %q before resuming",
			repoPath, current, expected,
		)
	}
	return nil
}

// RewriteCodexSessionCwd updates session_meta cwd and branch in a
// local restored copy of a Codex JSONL transcript.
func RewriteCodexSessionCwd(
	content []byte, cwd, branch string,
) ([]byte, error) {
	if cwd == "" {
		return append([]byte(nil), content...), nil
	}
	parts := bytes.SplitAfter(content, []byte("\n"))
	out := make([]byte, 0, len(content))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		hasNewline := bytes.HasSuffix(part, []byte("\n"))
		line := bytes.TrimSuffix(part, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		next := line
		if gjson.ValidBytes(line) &&
			gjson.GetBytes(line, "type").Str == "session_meta" {
			rewritten, err := rewriteSessionMetaLine(
				line, cwd, branch,
			)
			if err != nil {
				return nil, err
			}
			next = rewritten
		}
		out = append(out, next...)
		if hasNewline {
			out = append(out, '\n')
		}
	}
	return out, nil
}

func rewriteSessionMetaLine(
	line []byte, cwd, branch string,
) ([]byte, error) {
	var entry map[string]any
	if err := json.Unmarshal(line, &entry); err != nil {
		return nil, fmt.Errorf("parsing Codex session_meta: %w", err)
	}
	payload, _ := entry["payload"].(map[string]any)
	if payload == nil {
		payload = map[string]any{}
		entry["payload"] = payload
	}
	payload["cwd"] = cwd
	if branch != "" {
		git, _ := payload["git"].(map[string]any)
		if git == nil {
			git = map[string]any{}
			payload["git"] = git
		}
		git["branch"] = branch
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("encoding Codex session_meta: %w", err)
	}
	return data, nil
}

func restoreCodexTranscript(
	cfg config.Config, rawID string,
	blob postgres.NativeSessionBlob, content []byte,
) (string, error) {
	root := codexRestoreRoot(cfg)
	if root == "" {
		return "", fmt.Errorf("no Codex sessions directory configured")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("creating Codex sessions root: %w", err)
	}
	if existing := parser.FindCodexSourceFile(root, rawID); existing != "" {
		if err := ensureSameOrWrite(existing, content); err != nil {
			return "", err
		}
		return existing, nil
	}

	filename := safeCodexFilename(blob.Filename, rawID, blob.SourceMtime)
	t := time.Now().UTC()
	if blob.SourceMtime != nil {
		t = blob.SourceMtime.UTC()
	}
	dest := filepath.Join(
		root,
		t.Format("2006"),
		t.Format("01"),
		t.Format("02"),
		filename,
	)
	if err := ensureSameOrWrite(dest, content); err != nil {
		return "", err
	}
	return dest, nil
}

func codexRestoreRoot(cfg config.Config) string {
	var fallback string
	for _, dir := range cfg.ResolveDirs(parser.AgentCodex) {
		if dir == "" {
			continue
		}
		if fallback == "" {
			fallback = dir
		}
		if filepath.Base(dir) != "archived_sessions" {
			return dir
		}
	}
	return fallback
}

func safeCodexFilename(
	filename, rawID string, sourceMtime *time.Time,
) string {
	base := filepath.Base(filename)
	if strings.HasPrefix(base, "rollout-") &&
		strings.HasSuffix(base, ".jsonl") {
		return base
	}
	t := time.Now().UTC()
	if sourceMtime != nil {
		t = sourceMtime.UTC()
	}
	rawID = strings.NewReplacer("/", "_", "\\", "_").Replace(rawID)
	return "rollout-" + t.Format("2006-01-02T15-04-05") +
		"-" + rawID + ".jsonl"
}

func ensureSameOrWrite(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil {
		if sha256Hex(existing) != sha256Hex(content) {
			return fmt.Errorf(
				"refusing to overwrite existing Codex transcript with different content: %s",
				path,
			)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading existing Codex transcript: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating Codex session directory: %w", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("writing Codex transcript: %w", err)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RunCodexResume executes the prepared Codex resume command.
func RunCodexResume(ctx context.Context, result ResumeResult) error {
	cmd := exec.CommandContext(
		ctx, "codex", "resume", result.RawSessionID,
	)
	cmd.Dir = result.Cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func shellQuote(s string) string {
	safe := len(s) > 0 && s[0] != '-'
	if safe {
		for _, c := range s {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
				(c < '0' || c > '9') && c != '-' && c != '_' &&
				c != ':' {
				safe = false
				break
			}
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func commandWithCwd(cmd, cwd string) string {
	if cwd == "" {
		return cmd
	}
	return "cd " + shellQuote(cwd) + " && " + cmd
}
