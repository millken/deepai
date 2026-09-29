package hook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tgControlScriptLine is pinned to the exact output format of the TG control
// reference script in docs/HOOKS.md. Changing the doc's script without
// changing the parser (or this pin) turns the pinned tests red.
const tgControlScriptLine = "reply hello from telegram"

func TestCommandSource_PollsCommandOutput(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' 'reply hello' 'interrupt'\n"
	path := filepath.Join(dir, "ctl.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	src, err := NewCommandSource([]string{path})
	if err != nil {
		t.Fatalf("NewCommandSource: %v", err)
	}
	cmds, err := src.Poll(context.Background(), ControlState{})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("cmds = %d, want 2", len(cmds))
	}
	if cmds[0].Kind != CommandReply || cmds[0].Arg != "hello" {
		t.Errorf("cmds[0] = %+v, want reply hello", cmds[0])
	}
	if cmds[1].Kind != CommandInterrupt {
		t.Errorf("cmds[1] = %+v, want interrupt", cmds[1])
	}
}

func TestCommandSource_PassesEnvAndExpandsHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	out := filepath.Join(dir, "env.txt")
	script := "#!/bin/sh\nprintf '%s' \"$DEEPAI_STATE $DEEPAI_ASKING $DEEPAI_QUESTION\" > " + out + "\nprintf 'noop\\n'\n"
	path := filepath.Join(dir, "ctl.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	src, err := NewCommandSource([]string{"~/ctl.sh"})
	if err != nil {
		t.Fatalf("NewCommandSource: %v", err)
	}
	if _, err := src.Poll(context.Background(), ControlState{Asking: true, Question: "Pick one?"}); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "asking true Pick one?" {
		t.Fatalf("env = %q, want %q", string(data), "asking true Pick one?")
	}
}

func TestCommandSource_CommandFailureReturnsEmpty(t *testing.T) {
	src, err := NewCommandSource([]string{"/nonexistent/definitely-missing-ctl"})
	if err != nil {
		t.Fatalf("NewCommandSource: %v", err)
	}
	cmds, err := src.Poll(context.Background(), ControlState{})
	if err != nil {
		t.Fatalf("a failing control command must not surface an error to the poll loop: %v", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("cmds = %+v, want none", cmds)
	}
}

func TestURLSource_PollsBodyLines(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		_, _ = w.Write([]byte("cancel-task abc123\n"))
	}))
	defer srv.Close()

	src := NewURLSource(srv.URL)
	cmds, err := src.Poll(context.Background(), ControlState{})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(cmds) != 1 || cmds[0].Kind != CommandCancelTask || cmds[0].Arg != "abc123" {
		t.Fatalf("cmds = %+v, want one cancel-task abc123", cmds)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

func TestURLSource_ServerErrorReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	src := NewURLSource(srv.URL)
	cmds, err := src.Poll(context.Background(), ControlState{})
	if err != nil {
		t.Fatalf("a 500 must not surface an error to the poll loop: %v", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("cmds = %+v, want none", cmds)
	}
}

func TestParseControlLine_PinnedDocsReferenceScript(t *testing.T) {
	cmds := parseControlLines(tgControlScriptLine + "\ninterrupt\ncancel-task abc123\n")
	if len(cmds) != 3 {
		t.Fatalf("cmds = %d, want 3", len(cmds))
	}
	if cmds[0].Kind != CommandReply || cmds[0].Arg != "hello from telegram" {
		t.Errorf("docs script line = %+v, want reply 'hello from telegram'", cmds[0])
	}
	if cmds[1].Kind != CommandInterrupt || cmds[1].Arg != "" {
		t.Errorf("interrupt line = %+v", cmds[1])
	}
	if cmds[2].Kind != CommandCancelTask || cmds[2].Arg != "abc123" {
		t.Errorf("cancel-task line = %+v", cmds[2])
	}
}

func TestParseControlLine_BareTextIsReply(t *testing.T) {
	cmds := parseControlLines("just a plain answer\n")
	if len(cmds) != 1 || cmds[0].Kind != CommandReply || cmds[0].Arg != "just a plain answer" {
		t.Fatalf("cmds = %+v, want one reply with the full text", cmds)
	}
}

func TestParseControlLine_IgnoresNoise(t *testing.T) {
	cmds := parseControlLines("\nnoop\n/botcommand\n  \n")
	if len(cmds) != 0 {
		t.Fatalf("cmds = %+v, want none from empty/noop/slash lines", cmds)
	}
}

// extractDocsTGScript pulls the ENTIRE tg-control.sh script VERBATIM out of
// docs/HOOKS.md (shebang through the closing fence) — not just its awk
// pipeline — so every line the user ships, including the offset rename, is
// what the pinned test runs.
func extractDocsTGScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "HOOKS.md"))
	if err != nil {
		t.Fatalf("read docs/HOOKS.md: %v", err)
	}
	s := string(data)
	i := strings.Index(s, "### 控制：tg-control.sh")
	if i < 0 {
		t.Fatal("docs/HOOKS.md: tg-control.sh section not found")
	}
	rel := strings.Index(s[i:], "```sh\n")
	if rel < 0 {
		t.Fatal("docs/HOOKS.md: tg-control.sh code fence not found")
	}
	start := i + rel + len("```sh\n")
	end := strings.Index(s[start:], "\n```")
	if end < 0 {
		t.Fatal("docs/HOOKS.md: tg-control.sh closing fence not found")
	}
	return s[start : start+end]
}

// runDocsTGScript executes the verbatim docs script in a sandboxed HOME with
// a fake first-on-PATH curl that emulates Telegram's offset semantics:
// getUpdates returns the canned payload only while the request's offset is
// <= FAKE_LAST_ID, else an empty result. Returns each run's stdout and the
// final offset file content.
func runDocsTGScript(t *testing.T, script, payload, lastID string, runs int) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	home := filepath.Join(dir, "home")
	for _, d := range []string{bin, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	payloadPath := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeCurl := "#!/bin/sh\n" +
		"off=0\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in *offset=*) off=${a#*offset=}; off=${off%%&*} ;; esac\n" +
		"done\n" +
		"if [ \"$off\" -le \"$FAKE_LAST_ID\" ]; then cat \"$FAKE_PAYLOAD\"; else printf '%s' '{\"ok\":true,\"result\":[]}'; fi\n"
	curlPath := filepath.Join(bin, "curl")
	if err := os.WriteFile(curlPath, []byte(fakeCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "tg-control.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	env := make([]string, 0, 16)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "PATH=") ||
			strings.HasPrefix(kv, "FAKE_PAYLOAD=") || strings.HasPrefix(kv, "FAKE_LAST_ID=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+home,
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"FAKE_PAYLOAD="+payloadPath,
		"FAKE_LAST_ID="+lastID,
	)

	var outs []string
	for i := 0; i < runs; i++ {
		cmd := exec.Command(scriptPath)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("run %d of the verbatim docs script failed: %v\nscript:\n%s", i+1, err, script)
		}
		outs = append(outs, string(out))
	}
	off, _ := os.ReadFile(filepath.Join(home, ".deepai", "hooks", "tg-offset"))
	return outs, strings.TrimSpace(string(off))
}

// The docs tg-control.sh, run VERBATIM under emulated Telegram offset
// semantics: run 1 delivers the update and must advance the script's OWN
// offset file; run 2 (next poll) must deliver nothing. A script that writes
// only .tmp without renaming would leave the offset at 0 and replay the same
// message on every poll — the exact failure a pipeline-only pin missed.
func TestDocsTGControlScript_PinnedExtraction(t *testing.T) {
	script := extractDocsTGScript(t)

	payload := `{"ok":true,"result":[{"update_id":1000001,"message":{"message_id":42,"text":"hello from telegram"}}]}`
	outs, off := runDocsTGScript(t, script, payload, "1000001", 2)
	if got := parseControlLines(outs[0]); len(got) != 1 || got[0].Kind != CommandReply || got[0].Arg != "hello from telegram" {
		t.Fatalf("run 1 output %q parsed to %+v, want one reply 'hello from telegram'", outs[0], got)
	}
	if got := parseControlLines(outs[1]); len(got) != 0 {
		t.Fatalf("run 2 replayed an already-consumed update (%q → %+v) — the script's offset rename is missing or broken", outs[1], got)
	}
	if off != "1000002" {
		t.Fatalf("offset file = %q, want 1000002 (max update_id+1)", off)
	}

	payload = `{"ok":true,"result":[{"update_id":1000002,"message":{"text":"interrupt"}},{"update_id":1000005,"message":{"text":"cancel-task abc123"}}]}`
	outs, off = runDocsTGScript(t, script, payload, "1000005", 2)
	cmds := parseControlLines(outs[0])
	if len(cmds) != 2 || cmds[0].Kind != CommandInterrupt || cmds[1].Kind != CommandCancelTask || cmds[1].Arg != "abc123" {
		t.Fatalf("run 1 output %q parsed to %+v, want interrupt + cancel-task abc123", outs[0], cmds)
	}
	if got := parseControlLines(outs[1]); len(got) != 0 {
		t.Fatalf("run 2 replayed already-consumed updates: %q", outs[1])
	}
	if off != "1000006" {
		t.Fatalf("offset file = %q, want 1000006 (max update_id+1)", off)
	}

	// A bot slash command must not become a reply, but its update still
	// advances the offset — otherwise the same /cmd refetches forever.
	payload = `{"ok":true,"result":[{"update_id":1000010,"message":{"text":"/start"}}]}`
	outs, off = runDocsTGScript(t, script, payload, "1000010", 2)
	if got := parseControlLines(outs[0]); len(got) != 0 {
		t.Fatalf("slash command leaked into commands: %+v", got)
	}
	if got := parseControlLines(outs[1]); len(got) != 0 {
		t.Fatalf("run 2 replayed the skipped slash command: %q", outs[1])
	}
	if off != "1000011" {
		t.Fatalf("offset file = %q, want 1000011 even for a skipped slash command", off)
	}
}

// Exit must not be held hostage by an in-flight poll: a hung control script
// is killed by context cancellation, not by waiting out controlCmdTimeout.
func TestCommandSource_ContextCancelReturnsFast(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 30\n"
	path := filepath.Join(dir, "ctl.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := NewCommandSource([]string{path})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	cmds, err := src.Poll(ctx, ControlState{})
	if err != nil {
		t.Fatalf("cancelled poll must fail soft: %v", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("cmds = %+v, want none", cmds)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancelled poll took %v, want a fast return (kill, not timeout)", elapsed)
	}
}

// A grandchild inheriting stdout must not hold Output() open past the direct
// child's exit — WaitDelay bounds the pipe drain.
func TestCommandSource_GrandchildPipeBounded(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n(sleep 10) &\nprintf 'noop\\n'\n"
	path := filepath.Join(dir, "ctl.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := NewCommandSource([]string{path})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := src.Poll(context.Background(), ControlState{}); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Output blocked %v on a grandchild holding the pipe, want WaitDelay-bounded", elapsed)
	}
}
