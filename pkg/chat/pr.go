package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/sandbox"
)

// prStatus is the PR loop's persisted state model (docs plan: PR review
// pipeline). Only reviewing/awaiting_ci/awaiting_merge resume after a crash;
// every other value is terminal and written the moment the loop leaves —
// same discipline as missionStatus (R32): "merged" can never be mistaken for
// "the loop merely stopped".
type prStatus string

const (
	prStatusReviewing     prStatus = "reviewing"
	prStatusAwaitingCI    prStatus = "awaiting_ci"
	prStatusAwaitingMerge prStatus = "awaiting_merge"
	prStatusMerged        prStatus = "merged"
	prStatusAborted       prStatus = "aborted"
)

func (s prStatus) active() bool {
	switch s {
	case prStatusReviewing, prStatusAwaitingCI, prStatusAwaitingMerge:
		return true
	}
	return false
}

func (s prStatus) String() string { return string(s) }

// prState is one tracked pull request. Persisted under
// .deepai/pr/pr-<number>/state.json; the PR number is the natural key because
// the loop only ever starts AFTER the PR exists.
type prState struct {
	Number int      `json:"number"`
	Repo   string   `json:"repo,omitempty"`
	Branch string   `json:"branch,omitempty"`
	Base   string   `json:"base,omitempty"`
	URL    string   `json:"url,omitempty"`
	Status prStatus `json:"status"`
	// Round is the review round in progress, 1-based. The ONLY authority on
	// which round we are in — comment markers group content, they never
	// decide the round, because a role may post several comments per round.
	Round int      `json:"round"`
	Brief string   `json:"brief,omitempty"`
	Scope []string `json:"scope,omitempty"`
	// LastExternalComment is the id of the newest external PR comment
	// already surfaced to a fix turn; anything newer is pending input.
	LastExternalComment string    `json:"last_external_comment,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func prsRoot(workDir string) string {
	return filepath.Join(workDir, ".deepai", "pr")
}

func prDir(workDir string, number int) string {
	return filepath.Join(prsRoot(workDir), fmt.Sprintf("pr-%d", number))
}

const prStateFile = "state.json"

// prVerdictFile append-logs every review verdict. The resume path reloads
// the last one so a crash between the reviewer's post and the fix turn still
// re-reviews against the issues that were actually reported.
const prVerdictFile = "reviews.jsonl"

// appendVerdict writes one JSON line: round + verdict.
func (s *prState) appendVerdict(workDir string, round int, v *agent.ReviewResult) error {
	data, err := json.Marshal(struct {
		Round   int                 `json:"round"`
		Verdict *agent.ReviewResult `json:"verdict"`
	}{Round: round, Verdict: v})
	if err != nil {
		return fmt.Errorf("marshal pr verdict: %w", err)
	}
	if err := os.MkdirAll(prDir(workDir, s.Number), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(prDir(workDir, s.Number), prVerdictFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open pr verdict log: %w", err)
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// loadLastVerdict returns the newest logged verdict and its round, or (nil,
// 0). Only FAIL verdicts are ever appended (a pass moves the loop past
// reviewing), so the last entry is the prev a resumed re-review needs —
// the caller additionally requires its round to be exactly st.Round-1, so
// a verdict from an older cycle (e.g. before a CI fix round) never
// masquerades as the previous review's findings.
func loadLastVerdict(workDir string, number int) (*agent.ReviewResult, int) {
	data, err := os.ReadFile(filepath.Join(prDir(workDir, number), prVerdictFile))
	if err != nil {
		return nil, 0
	}
	var last *agent.ReviewResult
	lastRound := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			Round   int                 `json:"round"`
			Verdict *agent.ReviewResult `json:"verdict"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Verdict != nil {
			last, lastRound = entry.Verdict, entry.Round
		}
	}
	return last, lastRound
}

func prStatePath(workDir string, number int) string {
	return filepath.Join(prDir(workDir, number), prStateFile)
}

func newPRState(workDir string, number int, repo, branch, base, url, brief string) (*prState, error) {
	st := &prState{
		Number: number, Repo: repo, Branch: branch, Base: base, URL: url,
		Status: prStatusReviewing, Round: 1, Brief: brief,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.save(workDir); err != nil {
		return nil, err
	}
	return st, nil
}

func openPRState(workDir string, number int) (*prState, error) {
	data, err := os.ReadFile(prStatePath(workDir, number))
	if err != nil {
		return nil, fmt.Errorf("read pr state: %w", err)
	}
	var st prState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse pr state: %w", err)
	}
	return &st, nil
}

// save writes state.json on every transition, not at loop exit: a crash
// between two turns must not come back claiming a round it already spent.
func (s *prState) save(workDir string) error {
	s.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(prDir(workDir, s.Number), 0o755); err != nil {
		return fmt.Errorf("create pr dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pr state: %w", err)
	}
	return os.WriteFile(prStatePath(workDir, s.Number), append(data, '\n'), 0o644)
}

func (s *prState) setStatus(workDir string, st prStatus) error {
	s.Status = st
	return s.save(workDir)
}

// activePRStates lists tracked PRs in non-terminal states, oldest first.
// The loop runs one PR at a time; more than one active state means a crash
// raced a new PR — the caller surfaces all of them for /pr resume.
func activePRStates(workDir string) []*prState {
	entries, err := os.ReadDir(prsRoot(workDir))
	if err != nil {
		return nil
	}
	var out []*prState
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "pr-") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "pr-"))
		if err != nil {
			continue
		}
		st, err := openPRState(workDir, n)
		if err != nil || !st.Status.active() {
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// prCreateURLRe matches the URL `gh pr create` prints on success
// ("https://github.com/owner/repo/pull/42"). Anchored on the tail of the
// line so it survives redirect noise and ANSI wrapping, and it must not
// match `gh pr view` output of an OLD PR mentioned in passing — only a line
// that IS a PR URL (optionally prefixed by gh's "Creating pull request..."/
// leading text) counts.
var prCreateURLRe = regexp.MustCompile(`(?:https://(?:www\.)?github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))/pull/(\d+)`)

// detectPRCreate scans one turn's bash tool outputs for a freshly created
// PR. Only outputs that also contain gh pr create's "Creating pull request"
// marker line count: `gh pr view <n>` prints the same /pull/N URL for an OLD
// PR, and attaching the loop to that would re-review something the loop
// already finished. The LAST match wins (a turn that opened two PRs tracks
// the newest); repo/number/url come from the URL itself, no extra gh call.
func detectPRCreate(outputs []string) (repo string, number int, url string, ok bool) {
	for _, out := range outputs {
		if !strings.Contains(out, "Creating pull request") {
			continue
		}
		for _, m := range prCreateURLRe.FindAllStringSubmatch(out, -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil || n <= 0 {
				continue
			}
			repo, number, url, ok = m[1], n, m[0], true
		}
	}
	return repo, number, url, ok
}

// filterExternalComments keeps only comments NOT authored by deepai's own
// login. Every party comments through a distinct, configurable author
// identity (cursor uses "cursor"), so authorship alone draws the boundary —
// bodies stay plain and human-readable, no hidden markers.
func filterExternalComments(comments []prComment, ownLogin string) []prComment {
	var external []prComment
	for _, c := range comments {
		if !strings.EqualFold(c.Author, ownLogin) {
			external = append(external, c)
		}
	}
	return external
}

type prComment struct {
	ID        string
	Author    string
	Body      string
	CreatedAt time.Time
}

// prGH is everything the PR loop needs from gh. Interface-shaped so tests
// inject a fake and the loop never shells out behind an abstraction that
// hides a network call.
type prGH interface {
	// Login returns the authenticated account's login — the author every
	// comment deepai posts carries. External passthrough filters on it.
	Login(ctx context.Context) (string, error)
	// View returns a tracked-PR bootstrap: title, branch, base, url.
	View(ctx context.Context, repo string, number int) (title, branch, base, url string, err error)
	PostComment(ctx context.Context, repo string, number int, body string) error
	ListComments(ctx context.Context, repo string, number int) ([]prComment, error)
	Diff(ctx context.Context, repo string, number int) (string, error)
	ChangedFiles(ctx context.Context, repo string, number int) ([]string, error)
	Checks(ctx context.Context, repo string, number int) (done, ok bool, summary string, err error)
	Merge(ctx context.Context, repo string, number int) error
}

const prGhTimeout = 60 * time.Second

type ghPRClient struct{}

func (ghPRClient) run(ctx context.Context, args ...string) (stdout, stderr string, code int, err error) {
	result, err := sandbox.ExecDirect(ctx, "gh "+strings.Join(args, " "), prGhTimeout)
	if err != nil {
		return "", "", -1, fmt.Errorf("gh failed to run: %w", err)
	}
	return result.Stdout(), result.Stderr(), result.ExitCode(), nil
}

// prRepoArgs builds one gh PR subcommand's arguments. The leading "pr" is
// load-bearing: without it every caller degenerates into `gh view 3` —
// observed live as `unknown command "view" for "gh"` on /pr review. The
// optional --repo goes last (quoted; repo comes from state/config and is
// never trusted on a shell line); empty repo means "current directory's
// repository" and omits the flag entirely.
func prRepoArgs(repo string, extra ...string) []string {
	args := append([]string{"pr"}, extra...)
	if repo != "" {
		args = append(args, "--repo", strconv.Quote(repo))
	}
	return args
}

// PostComment goes through --body-file, not --body: ExecDirect takes a
// command string, and review issues contain quotes/backticks/newlines that
// must never be shell-interpolated.
func (ghPRClient) PostComment(ctx context.Context, repo string, number int, body string) error {
	f, err := os.CreateTemp("", "deepai-pr-comment-*")
	if err != nil {
		return fmt.Errorf("temp comment file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return fmt.Errorf("write comment file: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	args := prRepoArgs(repo, "comment", strconv.Itoa(number), "--body-file", strconv.Quote(f.Name()))
	_, stderr, code, err := ghPRClient{}.run(ctx, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("gh pr comment exited %d: %s", code, strings.TrimSpace(stderr))
	}
	return nil
}

func parsePRCommentsJSON(data []byte) ([]prComment, error) {
	var payload struct {
		Comments []struct {
			ID     string `json:"id"`
			Body   string `json:"body"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			CreatedAt string `json:"createdAt"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse gh pr view comments: %w", err)
	}
	out := make([]prComment, 0, len(payload.Comments))
	for _, c := range payload.Comments {
		ts, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse comment createdAt %q: %w", c.CreatedAt, err)
		}
		out = append(out, prComment{ID: c.ID, Author: c.Author.Login, Body: c.Body, CreatedAt: ts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Login returns the authenticated account's login via `gh api user`.
// EqualFold comparisons downstream make its case GitHub-canonical enough.
func (g ghPRClient) Login(ctx context.Context) (string, error) {
	out, _, code, err := g.run(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("gh api user exited %d", code)
	}
	login := strings.ToLower(strings.TrimSpace(out))
	if login == "" {
		return "", fmt.Errorf("gh api user returned an empty login")
	}
	return login, nil
}

func (g ghPRClient) View(ctx context.Context, repo string, number int) (title, branch, base, url string, err error) {
	args := prRepoArgs(repo, "view", strconv.Itoa(number),
		"--json", "title,headRefName,baseRefName,url")
	out, stderr, code, err := g.run(ctx, args...)
	if err != nil {
		return "", "", "", "", err
	}
	if code != 0 {
		return "", "", "", "", fmt.Errorf("gh pr view exited %d: %s", code, strings.TrimSpace(stderr))
	}
	var payload struct {
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return "", "", "", "", fmt.Errorf("parse gh pr view: %w", err)
	}
	return payload.Title, payload.HeadRefName, payload.BaseRefName, payload.URL, nil
}

func (g ghPRClient) ListComments(ctx context.Context, repo string, number int) ([]prComment, error) {
	args := prRepoArgs(repo, "view", strconv.Itoa(number), "--json", "comments")
	out, _, code, err := g.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("gh pr view exited %d", code)
	}
	return parsePRCommentsJSON([]byte(out))
}

func (g ghPRClient) Diff(ctx context.Context, repo string, number int) (string, error) {
	args := prRepoArgs(repo, "diff", strconv.Itoa(number))
	out, stderr, code, err := g.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("gh pr diff exited %d: %s", code, strings.TrimSpace(stderr))
	}
	return out, nil
}

func (g ghPRClient) ChangedFiles(ctx context.Context, repo string, number int) ([]string, error) {
	args := prRepoArgs(repo, "view", strconv.Itoa(number), "--json", "files")
	out, _, code, err := g.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("gh pr view exited %d", code)
	}
	var payload struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, fmt.Errorf("parse gh pr view files: %w", err)
	}
	paths := make([]string, 0, len(payload.Files))
	for _, f := range payload.Files {
		paths = append(paths, f.Path)
	}
	return paths, nil
}

// Checks mirrors ci_wait's exit-code mapping (pkg/tools/builtin/ciwait.go):
// 0 all passed, 8 pending, 1 failed-or-lookup-error, 127 gh missing.
func (g ghPRClient) Checks(ctx context.Context, repo string, number int) (bool, bool, string, error) {
	args := prRepoArgs(repo, "checks", strconv.Itoa(number))
	out, stderr, code, err := g.run(ctx, args...)
	if err != nil {
		return false, false, out, err
	}
	done, ok, lookupErr := classifyChecksCode(code, stderr)
	if lookupErr != "" {
		return false, false, out, fmt.Errorf("gh pr checks: %s", lookupErr)
	}
	return done, ok, out, nil
}

// classifyChecksCode is the pure exit-code verdict shared by Checks and its
// tests: done/ok plus a nonempty lookupErr when the PR itself could not be
// resolved (which must never be reported as a failed CI verdict).
func classifyChecksCode(code int, stderr string) (done, ok bool, lookupErr string) {
	switch {
	case code == 0:
		return true, true, ""
	case code == 8:
		return false, false, ""
	case code == 127:
		return false, false, "gh is not installed or not on PATH"
	case code == 1:
		s := strings.ToLower(stderr)
		if strings.Contains(s, "could not resolve") || strings.Contains(s, "not found") || strings.Contains(s, "no pull requests found") {
			return false, false, strings.TrimSpace(stderr)
		}
		return true, false, ""
	default:
		return true, false, ""
	}
}

func (g ghPRClient) Merge(ctx context.Context, repo string, number int) error {
	args := prRepoArgs(repo, "merge", strconv.Itoa(number), "--squash", "--delete-branch")
	_, stderr, code, err := g.run(ctx, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("gh pr merge exited %d: %s", code, strings.TrimSpace(stderr))
	}
	return nil
}
