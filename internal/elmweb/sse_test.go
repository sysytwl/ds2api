package elmweb

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"ds2api/internal/sse"
)

func TestWriteDeepSeekSSEIsReadableByPipeline(t *testing.T) {
	var buf bytes.Buffer
	if err := writeDeepSeekSSE(&buf, "response/content", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := writeDeepSeekSSE(&buf, "response/status", "FINISHED"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(&buf, "data: [DONE]\n\n")

	content := ""
	finished := false
	done := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		chunk, isDone, ok := sse.ParseDeepSeekSSELine([]byte(line))
		if !ok {
			t.Fatalf("unparseable line %q", line)
		}
		if isDone {
			done = true
			continue
		}
		parts, isFinished, _ := sse.ParseSSEChunkForContent(chunk, false, "text")
		finished = finished || isFinished
		for _, p := range parts {
			content += p.Text
		}
	}
	if content != "hello" {
		t.Fatalf("content = %q", content)
	}
	if !finished {
		t.Fatal("expected FINISHED status to stop the stream")
	}
	if !done {
		t.Fatal("expected [DONE] terminator")
	}
}
