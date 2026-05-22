package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// NativeSessionBlob is the original agent-native transcript
// stored in PostgreSQL for portable resume.
type NativeSessionBlob struct {
	SessionID      string
	Agent          string
	SourceMachine  string
	Project        string
	SourcePath     string
	SourceRepoRoot string
	Filename       string
	Cwd            string
	GitBranch      string
	Content        []byte
	ContentSHA256  string
	SizeBytes      int64
	SourceMtime    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NativeSessionBlobSummary omits Content for status and candidate
// listing APIs.
type NativeSessionBlobSummary struct {
	SessionID      string     `json:"session_id"`
	Agent          string     `json:"agent"`
	SourceMachine  string     `json:"source_machine"`
	Project        string     `json:"project"`
	SourcePath     string     `json:"source_path,omitempty"`
	SourceRepoRoot string     `json:"source_repo_root,omitempty"`
	Filename       string     `json:"filename,omitempty"`
	Cwd            string     `json:"cwd,omitempty"`
	GitBranch      string     `json:"git_branch,omitempty"`
	ContentSHA256  string     `json:"content_sha256"`
	SizeBytes      int64      `json:"size_bytes"`
	SourceMtime    *time.Time `json:"source_mtime,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// UpsertNativeSessionBlob stores a source-machine-specific native
// transcript. Divergent copies from different machines are kept as
// separate rows instead of overwriting each other.
func UpsertNativeSessionBlob(
	ctx context.Context, tx *sql.Tx, blob NativeSessionBlob,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO native_session_blobs (
			session_id, agent, source_machine, project,
			source_path, source_repo_root, filename, cwd, git_branch,
			content, content_sha256, size_bytes, source_mtime,
			updated_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8, $9,
			$10, $11, $12, $13,
			NOW()
		)
		ON CONFLICT (session_id, agent, source_machine) DO UPDATE SET
			project = EXCLUDED.project,
			source_path = EXCLUDED.source_path,
			source_repo_root = EXCLUDED.source_repo_root,
			filename = EXCLUDED.filename,
			cwd = EXCLUDED.cwd,
			git_branch = EXCLUDED.git_branch,
			content = EXCLUDED.content,
			content_sha256 = EXCLUDED.content_sha256,
			size_bytes = EXCLUDED.size_bytes,
			source_mtime = EXCLUDED.source_mtime,
			updated_at = NOW()
		WHERE native_session_blobs.project IS DISTINCT FROM EXCLUDED.project
			OR native_session_blobs.source_path IS DISTINCT FROM EXCLUDED.source_path
			OR native_session_blobs.source_repo_root IS DISTINCT FROM EXCLUDED.source_repo_root
			OR native_session_blobs.filename IS DISTINCT FROM EXCLUDED.filename
			OR native_session_blobs.cwd IS DISTINCT FROM EXCLUDED.cwd
			OR native_session_blobs.git_branch IS DISTINCT FROM EXCLUDED.git_branch
			OR native_session_blobs.content_sha256 IS DISTINCT FROM EXCLUDED.content_sha256
			OR native_session_blobs.size_bytes IS DISTINCT FROM EXCLUDED.size_bytes
			OR native_session_blobs.source_mtime IS DISTINCT FROM EXCLUDED.source_mtime`,
		blob.SessionID, blob.Agent, blob.SourceMachine,
		sanitizePG(blob.Project), sanitizePG(blob.SourcePath),
		sanitizePG(blob.SourceRepoRoot), sanitizePG(blob.Filename),
		sanitizePG(blob.Cwd), sanitizePG(blob.GitBranch),
		blob.Content, blob.ContentSHA256, blob.SizeBytes,
		blob.SourceMtime,
	)
	if err != nil {
		return fmt.Errorf(
			"upserting native session blob %s/%s: %w",
			blob.SessionID, blob.SourceMachine, err,
		)
	}
	return nil
}

// ListNativeSessionBlobs returns candidate native transcripts for a
// session without loading the blob content.
func (s *Store) ListNativeSessionBlobs(
	ctx context.Context, sessionID string,
) ([]NativeSessionBlobSummary, error) {
	return ListNativeSessionBlobs(ctx, s.pg, sessionID)
}

// ListNativeSessionBlobs returns candidate native transcripts for a
// session without loading the blob content.
func ListNativeSessionBlobs(
	ctx context.Context, pg *sql.DB, sessionID string,
) ([]NativeSessionBlobSummary, error) {
	rows, err := pg.QueryContext(ctx, `
		SELECT session_id, agent, source_machine, project,
			source_path, source_repo_root, filename, cwd, git_branch,
			content_sha256, size_bytes, source_mtime,
			created_at, updated_at
		FROM native_session_blobs
		WHERE session_id = $1
		ORDER BY source_mtime DESC NULLS LAST,
			updated_at DESC, source_machine ASC`,
		sessionID,
	)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"listing native session blobs: %w", err,
		)
	}
	defer rows.Close()

	var out []NativeSessionBlobSummary
	for rows.Next() {
		var b NativeSessionBlobSummary
		if err := rows.Scan(
			&b.SessionID, &b.Agent, &b.SourceMachine, &b.Project,
			&b.SourcePath, &b.SourceRepoRoot,
			&b.Filename, &b.Cwd, &b.GitBranch,
			&b.ContentSHA256, &b.SizeBytes, &b.SourceMtime,
			&b.CreatedAt, &b.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning native session blob: %w", err,
			)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating native session blobs: %w", err,
		)
	}
	return out, nil
}

// GetNativeSessionBlob loads a specific source-machine transcript.
func GetNativeSessionBlob(
	ctx context.Context, pg *sql.DB,
	sessionID, sourceMachine string,
) (*NativeSessionBlob, error) {
	query := `
		SELECT session_id, agent, source_machine, project,
			source_path, source_repo_root, filename, cwd, git_branch,
			content, content_sha256, size_bytes, source_mtime,
			created_at, updated_at
		FROM native_session_blobs
		WHERE session_id = $1`
	args := []any{sessionID}
	if sourceMachine != "" {
		query += " AND source_machine = $2"
		args = append(args, sourceMachine)
	}
	query += `
		ORDER BY source_mtime DESC NULLS LAST,
			updated_at DESC, source_machine ASC
		LIMIT 1`

	var b NativeSessionBlob
	err := pg.QueryRowContext(ctx, query, args...).Scan(
		&b.SessionID, &b.Agent, &b.SourceMachine, &b.Project,
		&b.SourcePath, &b.SourceRepoRoot,
		&b.Filename, &b.Cwd, &b.GitBranch,
		&b.Content, &b.ContentSHA256, &b.SizeBytes, &b.SourceMtime,
		&b.CreatedAt, &b.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"getting native session blob: %w", err,
		)
	}
	return &b, nil
}
