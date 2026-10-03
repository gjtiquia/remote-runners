package main_test

import (
	"strings"
	"testing"
)

func TestHelpDocumentsCommandsFormattingDefaultsAndExitCodes(t *testing.T) {
	binary := buildUtils(t)
	out, errOut, code := runUtils(t, binary, "-h")
	if code != 0 || out != "" {
		t.Fatalf("help: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, text := range []string{"usage:", "runners", "jobs", "job ID", "output ID", "cancel ID", "JSON", "raw", "2461", "127.0.0.1", "Options must precede", "0", "1", "2"} {
		if !strings.Contains(errOut, text) {
			t.Fatalf("help missing %q: %s", text, errOut)
		}
	}
}
