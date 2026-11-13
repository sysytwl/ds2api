package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitPromptShort(t *testing.T) {
	got := splitPrompt("hello", 100)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("unexpected %q", got)
	}
}

func TestSplitPromptLines(t *testing.T) {
	line := strings.Repeat("a", 99) + "\n"
	prompt := strings.Repeat(line, 400) // 40000 bytes
	chunks := splitPrompt(prompt, 1000)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 1000 {
			t.Fatalf("chunk too big: %d", len(c))
		}
	}
	if strings.Join(chunks, "") != prompt {
		t.Fatal("chunks do not reassemble the prompt")
	}
}

func TestSplitPromptCJK(t *testing.T) {
	prompt := strings.Repeat("中文提示词", 3000) // no newlines, multi-byte
	chunks := splitPrompt(prompt, 1000)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatal("chunk is not valid utf-8")
		}
		if len(c) > 1000 {
			t.Fatalf("chunk too big: %d", len(c))
		}
	}
	if strings.Join(chunks, "") != prompt {
		t.Fatal("chunks do not reassemble the prompt")
	}
}
