package main

import (
	"strings"
	"testing"
)

func TestCompletionScript(t *testing.T) {
	bash, err := completionScript("bash")
	if err != nil || !strings.Contains(bash, "complete -F _yagent yagent") {
		t.Errorf("bash completion: %v", err)
	}
	zsh, err := completionScript("zsh")
	if err != nil || !strings.Contains(zsh, "#compdef yagent") {
		t.Errorf("zsh completion: %v", err)
	}
	if _, err := completionScript("fish"); err == nil {
		t.Error("unknown shell should error")
	}
}
