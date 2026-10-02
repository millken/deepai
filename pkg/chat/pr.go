package chat

import (
	"context"
	"encoding/json"
	"errors"
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
	Round int `json:"round"`
	// Brief is what the reviewer judges the change against. Auto-attach
	// stores the user's own request; /pr review stores the PR body, with
	// the title kept as a one-line summary in front of it (prReviewBrief).
	Brief string   `json:"brief,omitempty"`
	Scope []string `json:"scope,omitempty"`
	// CommentPostedRound is the round whose review comment is CONFIRMED on
	// the PR (0 = none). The resume path gates "skip to the fix turn" on
	// this, never on the verdict log: appendVerdict runs before PostComment,
	// so a transient gh failure between them would otherwise make a resume
	// treat an un-posted review as posted and leave the PR timeline without
	// its review comment (new-loop round-1 issue 1). A resume that finds a
	// logged verdict for the round but CommentPostedRound behind it RE-POSTS
	// the comment from the stored verdict instead of re-reviewing.
	CommentPostedRound int `json:"comment_posted_round,omitempty"`
	// LastReviewHead is the commit sha the last review ran against (empty
	// before round 1). A re-review reads only the COMMITS SINCE this sha —
	// the human second-reviewer's approach: pull the incremental diff,
	// verify the previous findings against it, never re-read the whole PR.
	// A missing/unresolvable sha falls back to the full PR diff.
	LastReviewHead string `json:"last_review_head,omitempty"`
	// LastExternalCommentAt is the createdAt of the newest external PR
	// comment already surfaced to a fix turn; anything newer is pending input.
	// A time, not a comment id: gh ids are opaque base64 relay strings with no
	// usable ordering (round-1 review, verified live).
	LastExternalCommentAt time.Time `json:"last_external_comment_at,omitempty"`
	// SurfacedCommentIDs holds the ids of external comments already surfaced
	// that share the watermark's exact second — gh timestamps are
	// second-granular, so time alone cannot tell "already surfaced" from
	// "posted in the same second later" (round-2 review issue 4). The set is
	// rebuilt from each round's surfaced list, so it never outlives one busy
	// second; earlier ids age out behind the timestamp.
	SurfacedCommentIDs []string  `json:"surfaced_comment_ids,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func prsRoot(workDir string) string {
	return filepath.Join(workDir, ".deepai", "pr")
}

func prDir(workDir string, number int) string {
	return filepath.Join(prsRoot(workDir), fmt.Sprintf("pr-%d", number))
}

const prStateFile = "state.json"

// prVerdictFile append-logs every review verdict. The resume path reloads
// the last one so a crash after the reviewer reported — before or after the
// fix turn landed — still re-reviews against the issues that were actually
// reported.
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

// resetPRVerdicts drops the verdict log so a re-attached loop starts a clean
// cycle: a stale round-N entry would satisfy the resume gate's round == st.Round
// arm against the fresh round-1 state and masquerade as this round's prev.
// A missing log is not an error.
func resetPRVerdicts(workDir string, number int) error {
	err := os.Remove(filepath.Join(prDir(workDir, number), prVerdictFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// loadLastVerdict returns the newest logged verdict and its round, or (nil,
// 0). Only FAIL verdicts are ever appended (a pass moves the loop past
// reviewing), so the last entry is the prev a resumed re-review needs —
// the caller adopts it when its round is st.Round (verdict posted, fix turn
// never finished) or st.Round-1 (fix finished, next review pending), so a
// verdict from an older cycle never masquerades as this round's findings.
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

// repoFromPRURL extracts "owner/repo" from a github PR URL. Empty string
// when the URL is not a PR URL — callers keep the empty repo meaning "cwd's
// repository" rather than guessing (round-2 review issue 3: the manual
// /pr review path dropped a repo it had already fetched, silently retargeting
// every later gh call at whatever the process's cwd happened to be).
func repoFromPRURL(url string) string {
	m := prCreateURLRe.FindStringSubmatch(url)
	if m == nil {
		return ""
	}
	return m[1]
}

type prRole string

const (
	prRoleReviewer prRole = "reviewer"
	prRoleCoder    prRole = "coder"
	prRoleMerged   prRole = "merged"
)

var prMarkerRe = regexp.MustCompile(`<!-- deepai:role=(coder|reviewer|merged) round=(\d+) -->`)

// prMarker tags every comment the loop posts. The GitHub API stamps the
// token account as the author — with gh CLI that is always the owner's
// login — so authorship cannot separate deepai from the human owner or from
// other same-account tools (PR #3's live run showed all comments as the repo
// owner). The HTML comment is invisible in the GitHub UI, survives the API
// round trip, and nothing else generates it: the marker IS the boundary.
type prMarker struct {
	Role  prRole
	Round int
}

func (m prMarker) String() string {
	return fmt.Sprintf("<!-- deepai:role=%s round=%d -->", m.Role, m.Round)
}

func markerFromBody(body string) (prMarker, bool) {
	mm := prMarkerRe.FindStringSubmatch(body)
	if mm == nil {
		return prMarker{}, false
	}
	n, err := strconv.Atoi(mm[2])
	if err != nil {
		return prMarker{}, false
	}
	return prMarker{Role: prRole(mm[1]), Round: n}, true
}

// isOwnComment reports whether a PR comment was posted by this loop: current
// comments carry a deepai marker; comments posted before the marker existed
// (PR #3's first two rounds) start with the visible bold header — kept so
// resuming an in-flight PR never re-feeds its own history to the coder.
func isOwnComment(body string) bool {
	if _, ok := markerFromBody(body); ok {
		return true
	}
	return strings.HasPrefix(body, "**deepai ")
}

// filterExternalComments keeps only comments the loop did not post itself —
// the owner's own hand-written comments, other humans, cursor, any identity
// — told apart by the hidden marker (or the legacy visible header), never by
// author: every gh-CLI comment carries the same account login.
func filterExternalComments(comments []prComment) []prComment {
	var external []prComment
	for _, c := range comments {
		if !isOwnComment(c.Body) {
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
	// View returns a tracked-PR bootstrap: title, body, branch, base, url.
	// body is the claim /pr review judges against; empty when the PR has none.
	View(ctx context.Context, repo string, number int) (title, body, branch, base, url string, err error)
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

func (g ghPRClient) View(ctx context.Context, repo string, number int) (title, body, branch, base, url string, err error) {
	args := prRepoArgs(repo, "view", strconv.Itoa(number),
		"--json", "title,body,headRefName,baseRefName,url")
	out, stderr, code, err := g.run(ctx, args...)
	if err != nil {
		return "", "", "", "", "", err
	}
	if code != 0 {
		return "", "", "", "", "", fmt.Errorf("gh pr view exited %d: %s", code, strings.TrimSpace(stderr))
	}
	var payload struct {
		Title       string `json:"title"`
		Body        string `json:"body"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return "", "", "", "", "", fmt.Errorf("parse gh pr view: %w", err)
	}
	return payload.Title, payload.Body, payload.HeadRefName, payload.BaseRefName, payload.URL, nil
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

// Checks mirrors ci_wait's exit-code mapping, with one PR-loop-specific
// addition: exit 1 with gh's "no checks reported" stderr means the branch
// has NO CI configured — nothing failed, so the PR goes green (verified
// live against repos without workflows; deepai itself is one).
func (g ghPRClient) Checks(ctx context.Context, repo string, number int) (bool, bool, string, error) {
	args := prRepoArgs(repo, "checks", strconv.Itoa(number))
	out, stderr, code, err := g.run(ctx, args...)
	if err != nil {
		return false, false, out, err
	}
	summary := strings.TrimSpace(out)
	if s := strings.TrimSpace(stderr); s != "" {
		if summary != "" {
			summary += "\n"
		}
		summary += s
	}
	done, ok, lookupErr := classifyChecksCode(code, stderr)
	if lookupErr != "" {
		return false, false, summary, fmt.Errorf("gh pr checks: %s", lookupErr)
	}
	return done, ok, summary, nil
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
		if strings.Contains(s, "no checks reported") {
			return true, true, ""
		}
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
