package chat

import (
	"strings"

	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// shellLexer tokenizes shell source; shellStyle/shellFormatter render ANSI.
// Resolved once at init so per-line highlighting is allocation-light.
var (
	shellLexer     = lexers.Get("bash")
	shellStyle     = styles.Get("monokai")
	shellFormatter = formatters.Get("terminal256")
)

// highlightShellLine returns the input with ANSI shell syntax highlighting.
// On any failure (or missing lexer/formatter) it returns the input unchanged so
// the command text is always displayed.
func highlightShellLine(line string) string {
	if line == "" || shellLexer == nil || shellStyle == nil || shellFormatter == nil {
		return line
	}
	it, err := shellLexer.Tokenise(nil, line)
	if err != nil {
		return line
	}
	var b strings.Builder
	if err := shellFormatter.Format(&b, shellStyle, it); err != nil {
		return line
	}
	return strings.TrimRight(b.String(), "\n")
}
