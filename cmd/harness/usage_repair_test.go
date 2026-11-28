package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

func TestUsageRepairQuarantinesDamagedJournalAndMatchesRealState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	store, err := session.Open("repair", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(llm.Message{Role: "user", Content: "prior turn"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"FIRSTID", "SECONDID"} {
		if err := store.RecordUsage(session.UsageAttempt{ID: id, Request: llm.RequestTrace{ID: id, TokensComplete: true, Usage: &llm.Usage{TotalTokens: 10}}}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(session.Dir(), store.OutputID(), "usage.jsonl")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("broken\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	args := []string{"--session", "repair", "--root", root, "--quarantine", path}
	var stdout, stderr bytes.Buffer
	if err := usageRepair(args, &stdout, &stderr); err != nil {
		t.Fatalf("repair: %v\n%s", err, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "recovery gap cleared") {
		t.Fatalf("CLI printed recovery gap cleared while strict metering remained blocked: %s", out)
	}
	if !strings.Contains(out, "carried forward 2 attempt(s), 20 tokens") || !strings.Contains(out, "strict metering still blocked") || !strings.Contains(out, "--accept-lower-bound") {
		t.Fatalf("unexpected CLI output: %s", out)
	}

	stdout.Reset()
	stderr.Reset()
	if err := usageRepair(args, &stdout, &stderr); !errors.Is(err, session.ErrUsageHealthy) {
		t.Fatalf("healthy replacement journal was quarantined without --allow-healthy: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := usageRepair(append(args, "--allow-healthy"), &stdout, &stderr); err != nil {
		t.Fatalf("override quarantine failed: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "healthy override audited") || strings.Contains(stdout.String(), "recovery gap cleared") {
		t.Fatalf("unexpected override stdout: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := usageRepair([]string{"--session", "repair", "--root", root, "--accept-lower-bound"}, &stdout, &stderr); err != nil {
		t.Fatalf("accept-lower-bound failed: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "persisted lower-bound usage baseline ready") || strings.Contains(stdout.String(), "strict metering unblocked") || strings.Contains(stdout.String(), "recovery gap cleared") {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestUsageRepairBackupQuarantineIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	store, err := session.Open("repair-bak", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.01
	if err := store.RecordUsage(session.UsageAttempt{
		ID:               "FIRSTID",
		EstimateUSD:      &cost,
		ExactEstimateUSD: "1/100",
		Request:          llm.RequestTrace{ID: "FIRSTID", TokensComplete: true, PriceCountsComplete: true, Usage: &llm.Usage{TotalTokens: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(session.Dir(), store.OutputID(), "usage.jsonl")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":1,"id":"torn`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	store, err = session.Open("repair-bak", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := store.UsageRecovery()
	if err != nil || len(recovery.Artifacts) != 1 {
		t.Fatalf("%+v %v", recovery, err)
	}
	bakPath := recovery.Artifacts[0].Path
	store.Close()

	args := []string{"--session", "repair-bak", "--root", root, "--quarantine", bakPath}
	var stdout, stderr bytes.Buffer
	if err := usageRepair(args, &stdout, &stderr); err != nil {
		t.Fatalf("repair: %v\n%s", err, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := usageRepair(args, &stdout, &stderr); err != nil {
		t.Fatalf("second repair: %v\n%s", err, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("recovery gap cleared")) {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestUsageRepairDoesNotClearWhenUnpricedAttemptsBlockCostCaps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	store, err := session.Open("repair-unpriced", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUsage(session.UsageAttempt{ID: "FIRSTID", Request: llm.RequestTrace{ID: "FIRSTID", TokensComplete: true, Usage: &llm.Usage{TotalTokens: 1}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(session.Dir(), store.OutputID(), "usage.jsonl")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":1,"id":"torn`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	store, err = session.Open("repair-unpriced", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := store.UsageRecovery()
	if err != nil || len(recovery.Artifacts) != 1 {
		t.Fatalf("%+v %v", recovery, err)
	}
	bakPath := recovery.Artifacts[0].Path
	store.Close()

	var stdout, stderr bytes.Buffer
	if err := usageRepair([]string{"--session", "repair-unpriced", "--root", root, "--quarantine", bakPath}, &stdout, &stderr); err != nil {
		t.Fatalf("repair: %v\n%s", err, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "recovery gap cleared") || !strings.Contains(out, "strict metering still blocked for cost caps") {
		t.Fatalf("CLI claimed cleared while unpriced attempt remained: %s", out)
	}

	stdout.Reset()
	stderr.Reset()
	if err := usageRepair([]string{"--session", "repair-unpriced", "--root", root, "--accept-lower-bound"}, &stdout, &stderr); err != nil {
		t.Fatalf("accept-lower-bound: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "persisted lower-bound usage baseline ready") || strings.Contains(stdout.String(), "strict metering unblocked") || strings.Contains(stdout.String(), "recovery gap cleared") {
		t.Fatalf("stdout=%s", stdout.String())
	}
}
