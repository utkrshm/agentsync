package main

import (
	"errors"
	"testing"
)

func TestAliasLabel(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		want     string
	}{
		{"no previous alias omits default portion", "", "Enter device alias (Optional):"},
		{"previous alias shown as default", "laptop", "Enter device alias (Optional, Default - laptop):"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := aliasLabel(tt.existing); got != tt.want {
				t.Errorf("aliasLabel(%q) = %q, want %q", tt.existing, got, tt.want)
			}
		})
	}
}

func TestResolveAlias(t *testing.T) {
	newAsk := func(keep bool, answer string, keepCalls *int, inputCalls *int) (func() (bool, error), func(string) (string, error)) {
		return func() (bool, error) {
				*keepCalls++
				return keep, nil
			}, func(label string) (string, error) {
				*inputCalls++
				return answer, nil
			}
	}

	t.Run("flag overrides stored alias without prompting", func(t *testing.T) {
		keepCalls, inputCalls := 0, 0
		askKeep, askInput := newAsk(true, "", &keepCalls, &inputCalls)
		got, err := resolveAlias("desktop", "laptop", askKeep, askInput)
		if err != nil {
			t.Fatal(err)
		}
		if got != "desktop" {
			t.Errorf("resolveAlias = %q, want %q", got, "desktop")
		}
		if keepCalls != 0 || inputCalls != 0 {
			t.Errorf("flag path must not prompt: keep=%d input=%d", keepCalls, inputCalls)
		}
	})

	t.Run("stored alias kept on confirmation", func(t *testing.T) {
		keepCalls, inputCalls := 0, 0
		askKeep, askInput := newAsk(true, "", &keepCalls, &inputCalls)
		got, err := resolveAlias("", "laptop", askKeep, askInput)
		if err != nil {
			t.Fatal(err)
		}
		if got != "laptop" {
			t.Errorf("resolveAlias = %q, want %q", got, "laptop")
		}
		if keepCalls != 1 || inputCalls != 0 {
			t.Errorf("keep path should confirm once, not prompt for input: keep=%d input=%d", keepCalls, inputCalls)
		}
	})

	t.Run("refusing stored alias falls through to prompt", func(t *testing.T) {
		keepCalls, inputCalls := 0, 0
		askKeep, askInput := newAsk(false, "workstation", &keepCalls, &inputCalls)
		got, err := resolveAlias("", "laptop", askKeep, askInput)
		if err != nil {
			t.Fatal(err)
		}
		if got != "workstation" {
			t.Errorf("resolveAlias = %q, want %q", got, "workstation")
		}
		if keepCalls != 1 || inputCalls != 1 {
			t.Errorf("refusal should lead to one prompt: keep=%d input=%d", keepCalls, inputCalls)
		}
	})

	t.Run("blank input keeps stored alias", func(t *testing.T) {
		keepCalls, inputCalls := 0, 0
		askKeep, askInput := newAsk(false, "", &keepCalls, &inputCalls)
		got, err := resolveAlias("", "laptop", askKeep, askInput)
		if err != nil {
			t.Fatal(err)
		}
		if got != "laptop" {
			t.Errorf("blank input after refusal = %q, want stored %q", got, "laptop")
		}
	})

	t.Run("no stored alias with blank input stays empty", func(t *testing.T) {
		keepCalls, inputCalls := 0, 0
		askKeep, askInput := newAsk(false, "", &keepCalls, &inputCalls)
		got, err := resolveAlias("", "", askKeep, askInput)
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("resolveAlias = %q, want empty", got)
		}
		if keepCalls != 0 || inputCalls != 1 {
			t.Errorf("fresh setup should only prompt for input: keep=%d input=%d", keepCalls, inputCalls)
		}
	})

	t.Run("confirmation error propagates", func(t *testing.T) {
		sentinel := errors.New("stdin closed")
		got, err := resolveAlias("", "laptop",
			func() (bool, error) { return false, sentinel },
			func(string) (string, error) { return "", nil })
		if !errors.Is(err, sentinel) {
			t.Errorf("expected sentinel error, got %v", err)
		}
		if got != "" {
			t.Errorf("on error alias must be empty, got %q", got)
		}
	})
}
