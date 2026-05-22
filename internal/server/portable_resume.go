package server

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/postgres"
)

type nativeBlobLister interface {
	ListNativeSessionBlobs(
		ctx context.Context,
		sessionID string,
	) ([]postgres.NativeSessionBlobSummary, error)
}

type portableResumeResponse struct {
	Supported  bool                                `json:"supported"`
	SessionID  string                              `json:"session_id"`
	Agent      string                              `json:"agent"`
	Command    string                              `json:"command,omitempty"`
	Project    string                              `json:"project,omitempty"`
	Cwd        string                              `json:"cwd,omitempty"`
	GitBranch  string                              `json:"git_branch,omitempty"`
	Candidates []postgres.NativeSessionBlobSummary `json:"candidates,omitempty"`
	Error      string                              `json:"error,omitempty"`
}

func (s *Server) handlePortableResumeSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	session, err := s.db.GetSessionFull(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("portable resume: session lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if session == nil || session.DeletedAt != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if session.Agent != string(parser.AgentCodex) {
		writeJSON(w, http.StatusOK, portableResumeResponse{
			Supported: false,
			SessionID: session.ID,
			Agent:     session.Agent,
			Error:     "portable resume currently supports Codex sessions only",
		})
		return
	}

	var candidates []postgres.NativeSessionBlobSummary
	if lister, ok := s.db.(nativeBlobLister); ok {
		candidates, err = lister.ListNativeSessionBlobs(
			r.Context(), session.ID,
		)
		if err != nil {
			if handleContextError(w, err) {
				return
			}
			log.Printf("portable resume: listing blobs failed: %v", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if len(candidates) == 0 {
		writeJSON(w, http.StatusOK, portableResumeResponse{
			Supported: false,
			SessionID: session.ID,
			Agent:     session.Agent,
			Project:   session.Project,
			Cwd:       session.Cwd,
			GitBranch: session.GitBranch,
			Error:     "no portable transcript found; enable [portable_resume].upload_native_transcripts on the source machine and run agentsview pg push --full",
		})
		return
	}

	cmd := fmt.Sprintf(
		"agentsview pg resume %s",
		shellQuote(session.ID),
	)
	writeJSON(w, http.StatusOK, portableResumeResponse{
		Supported:  true,
		SessionID:  session.ID,
		Agent:      session.Agent,
		Command:    cmd,
		Project:    session.Project,
		Cwd:        session.Cwd,
		GitBranch:  session.GitBranch,
		Candidates: candidates,
	})
}
