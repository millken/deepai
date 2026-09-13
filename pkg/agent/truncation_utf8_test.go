package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/models"
)

// This project's session database holds 15 messages that are not decodable
// UTF-8. Ten came from an assistant-text cut that has since been made
// rune-safe; the rest came from toolMessageContent's byte slice, which this
// pins. The fragments reach SQLite, the next provider request and the
// terminal alike — a strict consumer errors out on them.
func TestToolMessageContent_StaysValidUTF8AtTheCut(t *testing.T) {
	// Land the cap inside a 3-byte rune: pad so byte maxToolContentBytes is
	// the middle of a CJK character.
	for pad := 0; pad < 3; pad++ {
		content := strings.Repeat("a", pad) + strings.Repeat("测", maxToolContentBytes)
		got := toolMessageContent(models.ToolResult{Content: content})
		if !utf8.ValidString(got) {
			t.Fatalf("pad=%d: truncated tool content is not valid UTF-8", pad)
		}
		if !strings.Contains(got, "[truncated:") {
			t.Fatalf("pad=%d: truncation marker missing", pad)
		}
	}
}

func TestToolResultPreviewAndHint_StayValidUTF8(t *testing.T) {
	long := strings.Repeat("测", 500)
	if got := toolResultPreview(models.ToolResult{Content: long}); !utf8.ValidString(got) {
		t.Errorf("tool result preview is not valid UTF-8: %q", got)
	}
	if got := firstLine(long); !utf8.ValidString(got) {
		t.Errorf("breaker hint line is not valid UTF-8: %q", got)
	}
}
