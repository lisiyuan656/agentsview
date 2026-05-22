package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/portable"
)

type PGResumeConfig struct {
	SourceMachine string
	RepoPath      string
	PrintCommand  bool
}

func newPGResumeCommand() *cobra.Command {
	var cfg PGResumeConfig
	cmd := &cobra.Command{
		Use:          "resume <session-id>",
		Short:        "Restore and resume a Codex session from PostgreSQL",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			runPGResume(cmd.Context(), args[0], cfg)
		},
	}
	cmd.Flags().StringVar(
		&cfg.SourceMachine,
		"source-machine",
		"",
		"Source machine to use when multiple transcripts exist",
	)
	cmd.Flags().StringVar(
		&cfg.RepoPath,
		"repo",
		"",
		"Local repository path to use for this resume",
	)
	cmd.Flags().BoolVar(
		&cfg.PrintCommand,
		"print-command",
		false,
		"Restore the transcript and print the resume command without executing Codex",
	)
	return cmd
}

func runPGResume(
	parent context.Context, sessionID string, resumeCfg PGResumeConfig,
) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		fatal("pg resume: loading config: %v", err)
	}
	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		fatal("pg resume: %v", err)
	}
	if pgCfg.URL == "" {
		fatal("pg resume: url not configured")
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()

	result, err := portable.PrepareCodexResume(
		ctx, pgCfg.URL, pgCfg.Schema, appCfg,
		pgCfg.AllowInsecure,
		portable.ResumeOptions{
			SessionID:     sessionID,
			SourceMachine: resumeCfg.SourceMachine,
			RepoPath:      resumeCfg.RepoPath,
		},
	)
	if err != nil {
		fatal("pg resume: %v", err)
	}

	fmt.Printf("Restored transcript: %s\n", result.DestinationPath)
	fmt.Printf("Source machine:      %s\n", result.SourceMachine)
	fmt.Printf("Working directory:   %s\n", result.Cwd)
	fmt.Printf("Command:             %s\n", result.Command)
	if resumeCfg.PrintCommand {
		return
	}
	if err := portable.RunCodexResume(ctx, result); err != nil {
		fatal("pg resume: codex resume failed: %v", err)
	}
}
