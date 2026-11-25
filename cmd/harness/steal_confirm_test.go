package main

import (
	"context"
	"strings"
	"testing"

	"minimalharness/internal/input"
)

func TestREPLStealSharesScanner(t *testing.T) {
	scanner := input.New(strings.NewReader("/steal held\nn\n/quit\n"))
	defer scanner.Close()
	line, err := scanner.Read(context.Background())
	if err != nil || line != "/steal held" {
		t.Fatalf("command = %q %v", line, err)
	}
	var out strings.Builder
	ok, err := confirmFrom(scanner, &out, context.Background(), "held")
	if err != nil || ok {
		t.Fatalf("n confirmed a steal: %v", err)
	}
	line, err = scanner.Read(context.Background())
	if err != nil || line != "/quit" {
		t.Fatalf("next line = %q %v", line, err)
	}
}

func TestConfirmStealRequiresExplicitYes(t *testing.T) {
	var out strings.Builder
	if confirmSteal(strings.NewReader("\n"), &out, "held") {
		t.Fatal("empty answer confirmed a steal")
	}
	if !strings.Contains(out.String(), "Steal session held") || !strings.Contains(out.String(), "refuses when it is not") {
		t.Fatalf("prompt = %q", out.String())
	}
	out.Reset()
	if confirmSteal(strings.NewReader("no\n"), &out, "held") {
		t.Fatal("no confirmed a steal")
	}
	if !confirmSteal(strings.NewReader("y\n"), &out, "held") {
		t.Fatal("y did not confirm a steal")
	}
	if confirmSteal(strings.NewReader(""), &out, "held") {
		t.Fatal("EOF confirmed a steal")
	}
}
