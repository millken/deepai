package chat

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The mission loop: design → design review → implement → implement review
// (docs/LONG_TASK_LOOP_DESIGN.md). Like runEpisode before it, this is an
// OUTER bounded for-loop around runTurn — not a new orchestration layer.
// commit 87772b6 deleted pkg/orchestrator after measuring that a state
// machine living OUTSIDE the ReAct loop, detached from sessions, events and
// persistence, is unmaintainable; the rule this file obeys is that every
// design revision round and every implementation round is an ORDINARY turn
// (persisted, memory-scheduled, r.turn-incremented) and the loop only
// decides what the next turn's input is.
//
// This file holds the on-disk half: the mission directory, the five-state
// status machine, and the charter. The phases themselves live in
// mission_design.go / mission_implement.go.
// ---------------------------------------------------------------------------

// missionPhase is which half of the loop a mission is in. Persisted, because
// a crashed or exited REPL must resume in the phase it left (§5.5 C12): the
// SessionCarry is deliberately NOT the authority here — it never reaches the
// database.
type missionPhase string

const (
	missionPhaseDesign    missionPhase = "design"
	missionPhaseImplement missionPhase = "implement"
)

// missionStatus is the mission's terminal-state model (R32). Only "active"
// resumes; every other value is terminal and is written the moment the loop
// leaves (leaveMission). The distinction between done and the other three is
// load-bearing: ONLY a correctness-reviewer pass writes done, so "the loop
// stopped" can never be mistaken for "the change was reviewed and held up".
type missionStatus string

const (
	// missionStatusActive: created, or resumed and still running.
	missionStatusActive missionStatus = "active"
	// missionStatusDone: the implementation review returned at least one
	// pass verdict. The only status that means "reviewed and passed".
	missionStatusDone missionStatus = "done"
	// missionStatusDesignFailed: the design rounds of THIS design phase ran
	// out with the plan still failing review. Two different situations share
	// the name (R37): in the initial design phase nothing has been
	// implemented yet, while after an escalation the worktree already holds
	// implementation edits that never passed review — the UI must say so.
	missionStatusDesignFailed missionStatus = "design_failed"
	// missionStatusHandedOver: implementation rounds exhausted, idle rounds
	// exhausted, an escalation signal with no escalations left, or a
	// fail-soft review. Changes are on disk and NOT known to be reviewed.
	missionStatusHandedOver missionStatus = "handed_over"
	// missionStatusAborted: /mission abort, /clear during an active mission,
	// or a new /mission <text> in the same session. Already-landed edits are
	// never rolled back — same rule the review gate's round cap follows.
	missionStatusAborted missionStatus = "aborted"
)

// Round caps. All constants, never config: the bound IS the safety property,
// and a configurable cap is a configurable "unbounded" (the same decision
// maxReviewRounds took, design §5.7).
const (
	// maxDesignRounds bounds the INITIAL design phase.
	maxDesignRounds = 3
	// maxEscalatedDesignRounds bounds a design phase re-entered from
	// implementation, counted from zero and NOT sharing maxDesignRounds'
	// budget (R4): sharing it meant an escalation that arrived after two
	// initial rounds got one round with no room to revise, which makes the
	// escalation path decorative. Worst case is therefore 3 + 2 = 5 design
	// reviews for one mission — still bounded.
	maxEscalatedDesignRounds = 2
	// maxDesignEscalations bounds how many times implementation may send the
	// mission back to design. One. After that the mission is handed to the
	// user.
	maxDesignEscalations = 1
	// maxScopeFixRounds bounds out-of-scope revert rounds. Counted
	// SEPARATELY from maxReviewRounds (R5): two scope violations must not
	// consume the budget for reviewing real defects, since a scope round
	// dispatches no reviewer at all.
	maxScopeFixRounds = 2
	// maxIdleRounds bounds turns that produced nothing to review (R31). An
	// empty review scope is "nothing happened", never "done" — without this
	// counter a purely conversational turn would end the mission silently
	// with zero implementation and zero review.
	maxIdleRounds = 2
)

// missionState is the persisted phase/round bookkeeping — the single
// authority for where a mission stands, on disk at state.json.
type missionState struct {
	ID     string        `json:"id"`
	Status missionStatus `json:"status"`
	Phase  missionPhase  `json:"phase"`
	// DesignRound counts rounds in the INITIAL design phase;
	// EscalatedDesignRound counts them in a post-escalation design phase and
	// is reset to 0 on every escalation (R4).
	DesignRound          int `json:"design_round"`
	EscalatedDesignRound int `json:"escalated_design_round"`
	ImplementRound       int `json:"implement_round"`
	ScopeRound           int `json:"scope_round"`
	IdleRound            int `json:"idle_round"`
	Escalation           int `json:"escalation"`
	// Reviewed records that the implementation review returned a pass at
	// least once. Only this may turn into status=done (R31/§9 #20).
	Reviewed  bool      `json:"reviewed"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Charter is what the mission is actually held to after the design gate
// passes (§5.3). It is written to disk, and a rendered copy rides the
// trailing turn injection into every subsequent request, because the point
// of it is to survive context compaction: an agent that can only see "what
// the conversation lately talked about" is exactly how a long task drifts
// off the original ask (D1/D7).
//
// Brief is the mission's opening user message verbatim and never changes,
// not even across an escalation — a re-designed plan still answers the same
// original request.
type Charter struct {
	MissionID string `json:"mission_id"`
	Brief     string `json:"brief"`
	// DesignHash fingerprints the plan this charter was locked from, so a
	// later revision of design.md on disk is distinguishable from the locked
	// copy the gate and the injection actually read (design.locked.md).
	DesignHash string `json:"design_hash"`
	// ScopeFiles is workdir-relative and already normalized (R19) — the
	// hard scope check does set arithmetic against it, and three different
	// path spellings for one file (the reviewer's, git's, the tool
	// records') would silently mis-classify every one of them.
	ScopeFiles []string  `json:"scope_files"`
	Acceptance []string  `json:"acceptance"`
	LockedAt   time.Time `json:"locked_at"`
	Escalation int       `json:"escalation"`
}

// mission is one mission's live handle: its directory, its persisted state,
// the immutable brief, and (once the design gate passes) the locked charter.
type mission struct {
	dir   string
	brief string
	state missionState
	// charter is nil in the design phase and non-nil for the whole
	// implementation phase. An escalation archives the file and clears this
	// — an implementer working under a charter the gate just declared wrong
	// would be told to obey it and re-design it in the same breath (R28).
	charter *Charter
	// baseline is S_impl: the worktree snapshot taken when the
	// implementation phase STARTS, and the authority for what this phase
	// changed (R25). Persisted to implement.baseline so a resumed mission
	// compares against the phase start rather than against its own restart.
	baseline worktreeSnapshot
}

// missionsRoot is where every mission directory lives. Under .deepai/ so it
// shares the scope exemption the rest of the tool's own bookkeeping has —
// a mission writing its own state must never register as an out-of-scope
// edit by the mission (§5.4.2).
func missionsRoot(workDir string) string {
	return filepath.Join(workDir, ".deepai", "missions")
}

func missionDir(workDir, id string) string {
	return filepath.Join(missionsRoot(workDir), id)
}

// newMissionID is timestamp + 4 random hex digits. The timestamp makes the
// directory listing readable in the order missions were run; the suffix
// keeps two missions started in the same second from colliding.
func newMissionID() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failed CSPRNG read must not stop a mission from starting; the
		// timestamp alone is still unique unless two start in one second.
		return time.Now().Format("20060102-150405")
	}
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// Mission directory layout (§5.5). Named rather than inlined so the design
// gate, the implementation gate and the tests cannot drift apart on a
// filename.
const (
	missionBriefFile       = "brief.md"
	missionDesignFile      = "design.md"
	missionLockedPlanFile  = "design.locked.md"
	missionCharterFile     = "charter.lock.json"
	missionStateFile       = "state.json"
	missionReviewsFile     = "reviews.jsonl"
	missionBaselineFile    = "implement.baseline"
	missionCharterArchived = "charter.v%d.json"
)

func (m *mission) path(name string) string { return filepath.Join(m.dir, name) }

// designPath is the ONE plan file a mission's design phase uses, for every
// round (R9). Pinned through AgentConfig.PlanFile so a revision round writes
// over the previous plan instead of opening a fresh timestamped file that
// leaves the gate reading an empty document.
func (m *mission) designPath() string { return m.path(missionDesignFile) }

// createMission lays out a new mission directory and writes the brief. The
// brief is written ONCE and never rewritten: it is the fixed anchor every
// later review compares against, so nothing in the loop may edit it.
func createMission(workDir, brief string) (*mission, error) {
	id := newMissionID()
	dir := missionDir(workDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create mission dir: %w", err)
	}
	now := time.Now().UTC()
	m := &mission{
		dir:   dir,
		brief: brief,
		state: missionState{
			ID:        id,
			Status:    missionStatusActive,
			Phase:     missionPhaseDesign,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
	if err := os.WriteFile(m.path(missionBriefFile), []byte(brief), 0o644); err != nil {
		return nil, fmt.Errorf("write brief: %w", err)
	}
	if err := m.save(); err != nil {
		return nil, err
	}
	return m, nil
}

// openMission reloads a mission from disk: state, brief, the locked charter
// if there is one, and the implementation baseline if the mission got that
// far. Resuming is the reason state.json exists at all — a SessionCarry
// never reaches the database, so without this a crash mid-mission would
// leave no way to tell design from implementation (§5.5 C12).
func openMission(workDir, id string) (*mission, error) {
	dir := missionDir(workDir, id)
	data, err := os.ReadFile(filepath.Join(dir, missionStateFile))
	if err != nil {
		return nil, fmt.Errorf("read mission state: %w", err)
	}
	var st missionState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse mission state: %w", err)
	}
	if st.ID == "" {
		st.ID = id
	}
	m := &mission{dir: dir, state: st}
	if b, err := os.ReadFile(m.path(missionBriefFile)); err == nil {
		m.brief = string(b)
	}
	if c, err := m.loadCharter(); err == nil {
		m.charter = c
	}
	m.baseline = m.loadBaseline()
	return m, nil
}

// save writes state.json. Every status and round change goes through here
// immediately rather than at loop exit: a mission that crashes between two
// turns must not come back claiming a round it already spent.
func (m *mission) save() error {
	m.state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mission state: %w", err)
	}
	return os.WriteFile(m.path(missionStateFile), append(data, '\n'), 0o644)
}

// setStatus persists a status transition. Errors are logged by the caller's
// UI path rather than aborting the loop — losing the on-disk record of a
// terminal state is bad, but it must not also swallow the user's changes.
func (m *mission) setStatus(s missionStatus) error {
	m.state.Status = s
	return m.save()
}

// readDesign returns the current plan text. The gate's "is there a plan yet"
// test is emptiness of THIS file and nothing else — deliberately not "did
// the agent call exit_plan_mode", which a model can skip while still having
// written a perfectly good plan (§5.2).
func (m *mission) readDesign() string {
	b, err := os.ReadFile(m.designPath())
	if err != nil {
		return ""
	}
	return string(b)
}

// lockCharter writes charter.lock.json and freezes the plan it was derived
// from as design.locked.md. The locked copy is what the injection and the
// implementation reviewer read: design.md itself stays writable (a later
// escalation re-enters the design phase and overwrites it), so reading the
// live file would let the implementation phase silently follow a plan no
// gate ever passed.
func (m *mission) lockCharter(c *Charter) error {
	plan := m.readDesign()
	c.MissionID = m.state.ID
	c.Brief = m.brief
	c.DesignHash = hashText(plan)
	c.LockedAt = time.Now().UTC()
	c.Escalation = m.state.Escalation
	if err := os.WriteFile(m.path(missionLockedPlanFile), []byte(plan), 0o644); err != nil {
		return fmt.Errorf("write locked plan: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal charter: %w", err)
	}
	if err := os.WriteFile(m.path(missionCharterFile), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write charter: %w", err)
	}
	m.charter = c
	return nil
}

func (m *mission) loadCharter() (*Charter, error) {
	data, err := os.ReadFile(m.path(missionCharterFile))
	if err != nil {
		return nil, err
	}
	var c Charter
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// lockedPlan is the plan text the current charter was locked from.
func (m *mission) lockedPlan() string {
	b, err := os.ReadFile(m.path(missionLockedPlanFile))
	if err != nil {
		return ""
	}
	return string(b)
}

// archiveCharter renames the lock file to charter.v<n>.json on escalation.
// The archive is kept for audit — the point of an escalation is that the
// charter was WRONG, and the record of what the mission was held to while
// it produced the edits now sitting in the worktree is worth more than the
// disk space.
func (m *mission) archiveCharter(n int) error {
	src := m.path(missionCharterFile)
	if _, err := os.Stat(src); err != nil {
		return nil // nothing locked yet
	}
	dst := m.path(fmt.Sprintf(missionCharterArchived, n))
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("archive charter: %w", err)
	}
	m.charter = nil
	return nil
}

// missionReviewRecord is one line of reviews.jsonl: every gate decision the
// mission made, for after-the-fact audit of a loop that ran without a human
// approving each step.
type missionReviewRecord struct {
	At      time.Time `json:"at"`
	Phase   string    `json:"phase"`
	Round   int       `json:"round"`
	Verdict string    `json:"verdict"`
	Summary string    `json:"summary"`
	Detail  any       `json:"detail,omitempty"`
}

func (m *mission) appendReview(rec missionReviewRecord) {
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(m.path(missionReviewsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// persistedStamp mirrors fileStamp for JSON. fileStamp's fields are
// unexported (and must stay that way — it is compared by == inside the
// snapshot code), so the baseline file gets its own shape rather than
// exporting the snapshot internals just to serialize them.
type persistedStamp struct {
	Status  string `json:"s"`
	Size    int64  `json:"z"`
	ModTime int64  `json:"m"`
}

type persistedSnapshot struct {
	Root    string                    `json:"root"`
	Entries map[string]persistedStamp `json:"entries"`
}

// saveBaseline persists S_impl. Without this, a mission resumed in the
// implementation phase would take a FRESH baseline, which makes every edit
// the previous session already made invisible to both the scope check and
// the reviewer (they compare against "now", and "now" already contains
// them).
func (m *mission) saveBaseline(s worktreeSnapshot) {
	m.baseline = s
	ps := persistedSnapshot{Root: s.root, Entries: make(map[string]persistedStamp, len(s.entries))}
	for p, st := range s.entries {
		ps.Entries[p] = persistedStamp{Status: st.status, Size: st.size, ModTime: st.modTime}
	}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(m.path(missionBaselineFile), append(data, '\n'), 0o644)
}

// loadBaseline returns the zero snapshot when there is no baseline file or
// it cannot be parsed. A zero baseline is the same "no snapshot" value the
// gate already handles: changedSince returns nil, the hard scope check turns
// itself off, and review attribution falls back to tool records (R30).
func (m *mission) loadBaseline() worktreeSnapshot {
	data, err := os.ReadFile(m.path(missionBaselineFile))
	if err != nil {
		return worktreeSnapshot{}
	}
	var ps persistedSnapshot
	if err := json.Unmarshal(data, &ps); err != nil || ps.Root == "" {
		return worktreeSnapshot{}
	}
	entries := make(map[string]fileStamp, len(ps.Entries))
	for p, st := range ps.Entries {
		entries[p] = fileStamp{status: st.Status, size: st.Size, modTime: st.ModTime}
	}
	return worktreeSnapshot{root: ps.Root, entries: entries}
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Charter normalization and rendering
// ---------------------------------------------------------------------------

// normalizeScopeFiles turns the reviewer's free-form path list into the one
// form every later comparison uses: workdir-relative, slash-separated,
// cleaned, deduplicated, order preserved (R19).
//
// This is not cosmetic. The hard scope check subtracts one path set from
// another; the sets come from three different producers (an LLM's prose,
// git's canonical output, the edit-tool records), and un-normalized set
// arithmetic does not error — it silently reports an in-scope file as a
// violation, which costs a scope-fix round and, twice, the mission's only
// escalation.
//
// Paths that resolve outside the working directory are DROPPED rather than
// clamped: a charter cannot authorize edits outside the tree the mission
// runs in, and a clamped path would authorize some unrelated in-tree file.
// Paths that do not exist yet are kept — a plan is supposed to name the
// files it will create.
func normalizeScopeFiles(workDir string, in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		p = filepath.FromSlash(p)
		if filepath.IsAbs(p) {
			rel, ok := workdirRel(workDir, p)
			if !ok {
				continue
			}
			p = rel
		}
		p = filepath.ToSlash(filepath.Clean(p))
		p = strings.TrimPrefix(p, "./")
		if p == "" || p == "." {
			continue
		}
		if strings.HasPrefix(p, "../") || p == ".." {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// workdirRel renders an absolute path as a slash-separated path relative to
// the working directory, and reports false when it lies outside.
//
// It tries the canonicalized forms first and the raw forms second, because
// neither alone is right in every case. Canonicalizing is required when the
// workdir is reached through a symlink (macOS's /var → /private/var): git
// reports real paths, the REPL's WorkDir may be the symlinked spelling, and
// mixing the two makes every in-tree file look like it is outside. But
// canonicalization silently no-ops on a path that does not exist — a file
// the plan has yet to create, or one the turn DELETED along with its
// directory — and then the two forms disagree again, in the other
// direction. Trying both is what makes both cases land inside the tree.
func workdirRel(workDir, p string) (string, bool) {
	if rel, ok := relUnderRoot(resolveWorktreePath(workDir), resolveWorktreePath(p)); ok {
		return filepath.ToSlash(rel), true
	}
	if rel, ok := relUnderRoot(filepath.Clean(workDir), filepath.Clean(p)); ok {
		return filepath.ToSlash(rel), true
	}
	return "", false
}

// relUnderRoot returns path relative to root, and false when it escapes the
// root (or cannot be made relative at all).
func relUnderRoot(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// renderCharter is the text that rides the trailing turn injection. Kept
// compact — it is re-sent with EVERY request for the rest of the mission,
// and the plan itself is on disk at a path this text names.
func renderCharter(c *Charter) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Mission charter (locked)\n\n")
	b.WriteString("This is the mission you are executing. It outranks anything later in the conversation: if the recent discussion and this charter disagree, the charter wins. You may not edit it — only an escalation from the implementation review can change it.\n\n")
	b.WriteString("## Original request (verbatim)\n\n")
	b.WriteString(strings.TrimSpace(c.Brief))
	b.WriteString("\n\n## In-scope files\n\n")
	for _, f := range c.ScopeFiles {
		b.WriteString("- " + f + "\n")
	}
	b.WriteString("\nEditing anything else (beyond *_test.go companions of these files) is caught by a hard scope check and has to be reverted.\n")
	b.WriteString("\n## Acceptance criteria\n\n")
	for i, a := range c.Acceptance {
		fmt.Fprintf(&b, "%d. %s\n", i+1, a)
	}
	return strings.TrimRight(b.String(), "\n")
}
