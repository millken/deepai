package memory

import "testing"

func TestDedupKeySeparatesSkillFromPlainUpdate(t *testing.T) {
	q := &UpdateQueue{}
	plain := updateJob{typ: jobUpdate, sessionID: "s1"}
	skill := updateJob{typ: jobUpdateScopeWithSkill, sessionID: "s1", skillName: "polish"}
	if a, b := q.dedupKey(plain), q.dedupKey(skill); a == b {
		t.Fatalf("plain update and skill update must use different dedup keys, both=%q", a)
	}
}

func TestDedupKeyCoalescesSameSkill(t *testing.T) {
	q := &UpdateQueue{}
	first := updateJob{typ: jobUpdateScopeWithSkill, sessionID: "s1", skillName: "polish"}
	second := updateJob{typ: jobUpdateScopeWithSkill, sessionID: "s1", skillName: "polish"}
	if q.dedupKey(first) != q.dedupKey(second) {
		t.Fatalf("same (session,skill) jobs should share dedup key")
	}
}

func TestClearPendingSeq_OnlyDeletesOwnEntry(t *testing.T) {
	q := &UpdateQueue{pendingSeq: map[string]uint64{"update:s": 5}}
	// A stale/dropped job (seq 4) must not erase a newer job's entry (seq 5).
	q.clearPendingSeq("update:s", 4)
	if _, ok := q.pendingSeq["update:s"]; !ok {
		t.Fatal("clearPendingSeq dropped a newer job's entry")
	}
	// The owning job (seq 5) clears it.
	q.clearPendingSeq("update:s", 5)
	if _, ok := q.pendingSeq["update:s"]; ok {
		t.Fatal("clearPendingSeq did not delete its own entry")
	}
}
