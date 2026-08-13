package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// reviewerFunc adapts a function to Reviewer.
type reviewerFunc func(ctx context.Context, current Document, messages []models.Message) (RefineReview, error)

func (f reviewerFunc) ReviewRefine(ctx context.Context, current Document, messages []models.Message) (RefineReview, error) {
	return f(ctx, current, messages)
}

func verdict(approve bool, rationale string) reviewerFunc {
	return func(context.Context, Document, []models.Message) (RefineReview, error) {
		return RefineReview{ShouldRefine: approve, Rationale: rationale}, nil
	}
}

const userScopeKey = "__scope__:user:workdir:"

// drainQueue blocks until every job submitted before it has been processed. The
// queue is a single FIFO worker, so a sentinel that runs proves the jobs queued
// ahead of it are done. Service.Close cannot be used for this: it cancels
// in-flight jobs before draining.
func drainQueue(t *testing.T, svc *Service) {
	t.Helper()

	done := make(chan struct{})
	var once sync.Once
	svc.ScheduleUpdateWith("drain-sentinel", refineMessages(), extractorFunc(
		func(context.Context, Document, []models.Message) (Update, error) {
			once.Do(func() { close(done) })
			return Update{}, nil
		},
	))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("queue did not drain")
	}
}

// factCount reports how many facts a storage key holds.
func factCount(t *testing.T, store *SQLiteStore, sessionID string) int {
	t.Helper()

	doc, err := store.Load(context.Background(), sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load(%q) error = %v", sessionID, err)
	}
	return len(doc.Facts)
}

func TestScheduleRefineExtractsBothScopesWhenTheGateApproves(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	if got := factCount(t, store, "s1"); got != 1 {
		t.Fatalf("session scope facts = %d, want 1", got)
	}
	if got := factCount(t, store, userScopeKey); got != 1 {
		t.Fatalf("user scope facts = %d, want 1", got)
	}
}

func TestScheduleRefineRecordsBothScopesUnderOnePairID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	sessionRecords, err := store.ListRefinements(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("ListRefinements(session) error = %v", err)
	}
	userRecords, err := store.ListRefinements(ctx, userScopeKey, 50)
	if err != nil {
		t.Fatalf("ListRefinements(user) error = %v", err)
	}
	if len(sessionRecords) != 1 || len(userRecords) != 1 {
		t.Fatalf("want one record per scope, got %d and %d", len(sessionRecords), len(userRecords))
	}
	// "/refine undo" finds the second half of a refine through this shared id.
	if sessionRecords[0].PairID == "" || sessionRecords[0].PairID != userRecords[0].PairID {
		t.Fatalf("PairID mismatch: %q vs %q", sessionRecords[0].PairID, userRecords[0].PairID)
	}
	if sessionRecords[0].Rationale != "worth keeping" {
		t.Fatalf("gate rationale did not reach the record: %q", sessionRecords[0].Rationale)
	}
}

func TestScheduleRefineExtractsNeitherScopeWhenTheGateRejects(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(false, "transient chatter"))

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	if got := factCount(t, store, "s1"); got != 0 {
		t.Fatalf("session scope extracted despite rejection: %d facts", got)
	}
	if got := factCount(t, store, userScopeKey); got != 0 {
		t.Fatalf("user scope extracted despite rejection: %d facts", got)
	}
}

func TestScheduleRefineFallsOpenWithoutAReviewer(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	// No reviewer configured: the gate is an optimisation, and its absence must
	// not stop extraction.

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	if got := factCount(t, store, "s1"); got != 1 {
		t.Fatalf("session scope facts = %d, want 1", got)
	}
	if got := factCount(t, store, userScopeKey); got != 1 {
		t.Fatalf("user scope facts = %d, want 1", got)
	}
}

func TestScheduleRefineFallsOpenWhenTheGateErrors(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(reviewerFunc(func(context.Context, Document, []models.Message) (RefineReview, error) {
		return RefineReview{}, errors.New("upstream 503")
	}))

	// A flaky gate must degrade to the behaviour that predates it, not silently
	// stop memory extraction.
	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	if got := factCount(t, store, "s1"); got != 1 {
		t.Fatalf("session scope facts = %d, want 1", got)
	}
	if got := factCount(t, store, userScopeKey); got != 1 {
		t.Fatalf("user scope facts = %d, want 1", got)
	}
}

func TestScheduleRefineFallsOpenForUserScopeWhenTheSessionJobIsCancelled(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	// Compaction cancels the session's queued work, which skips the gate job. The
	// user-scope job lives in a different dedup shard and still runs, but finds no
	// verdict. Treating that as a rejection would drop user-scope extraction
	// entirely, and compaction's synchronous flush only covers the session scope.
	release := make(chan struct{})
	svc.ScheduleUpdateWith("blocker", refineMessages(), extractorFunc(
		func(context.Context, Document, []models.Message) (Update, error) {
			<-release
			return Update{}, nil
		},
	))

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	svc.CancelPendingUpdates("s1")
	close(release)
	drainQueue(t, svc)

	if got := factCount(t, store, "s1"); got != 0 {
		t.Fatalf("the cancelled session job must not extract: %d facts", got)
	}
	if got := factCount(t, store, userScopeKey); got != 1 {
		t.Fatalf("user scope must fall open and extract, got %d facts", got)
	}
}

func TestRefineJobsUseTheFlushVersionCapturedAtEnqueue(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(reviewerFunc(func(context.Context, Document, []models.Message) (RefineReview, error) {
		// Runs after both jobs are queued and before the user-scope job executes.
		// A flush landing in that window supersedes the queued work; detecting it
		// requires the version captured at enqueue, not at execution.
		svc.queue.flushVersion.Store("update:"+userScopeKey, uint64(99))
		return RefineReview{ShouldRefine: true}, nil
	}))

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	if got := factCount(t, store, "s1"); got != 1 {
		t.Fatalf("session scope facts = %d, want 1", got)
	}
	if got := factCount(t, store, userScopeKey); got != 0 {
		t.Fatalf("superseded user-scope job must not write, got %d facts", got)
	}
}

func TestScheduleRefineDoesNotLeakVerdicts(t *testing.T) {
	t.Parallel()

	svc, _ := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	svc.ScheduleRefine("s1", userScopeKey, refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	remaining := 0
	svc.verdicts.Range(func(any, any) bool {
		remaining++
		return true
	})
	if remaining != 0 {
		t.Fatalf("verdict entries left behind: %d", remaining)
	}
}

func TestScheduleRefineWithoutAUserScopeOnlyRunsTheSession(t *testing.T) {
	t.Parallel()

	svc, store := newRefineService(t)
	svc.WithReviewer(verdict(true, "worth keeping"))

	// The REPL only has a user scope when a workdir identity is configured.
	svc.ScheduleRefine("s1", "", refineMessages(), addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	if got := factCount(t, store, "s1"); got != 1 {
		t.Fatalf("session scope facts = %d, want 1", got)
	}
	remaining := 0
	svc.verdicts.Range(func(any, any) bool {
		remaining++
		return true
	})
	if remaining != 0 {
		t.Fatalf("a verdict with no consumer must not be left behind: %d", remaining)
	}
}

func TestRefineGateSeesTheSameFilteredTrajectoryAsTheExtractor(t *testing.T) {
	t.Parallel()

	svc, _ := newRefineService(t)

	var gateSaw []models.Message
	svc.WithReviewer(reviewerFunc(func(_ context.Context, _ Document, messages []models.Message) (RefineReview, error) {
		gateSaw = messages
		return RefineReview{ShouldRefine: false}, nil
	}))

	// Tool results and uploaded-file blocks are stripped from everything else the
	// memory subsystem sends to a provider; the gate must not be the one path
	// that leaks them.
	messages := []models.Message{
		{Role: models.RoleHuman, Content: "<uploaded_files>secret.pem</uploaded_files>\nplease review"},
		{Role: models.RoleTool, Content: "raw tool output with credentials"},
		{Role: models.RoleAI, Content: "done"},
	}

	svc.ScheduleRefine("s1", userScopeKey, messages, addFact("f1", "uses gofmt"))
	drainQueue(t, svc)

	for _, msg := range gateSaw {
		if msg.Role == models.RoleTool {
			t.Fatalf("tool output reached the gate: %+v", msg)
		}
		if strings.Contains(msg.Content, "uploaded_files") {
			t.Fatalf("upload block reached the gate: %q", msg.Content)
		}
	}
	if len(gateSaw) == 0 {
		t.Fatal("the gate needs some trajectory to judge")
	}
}
