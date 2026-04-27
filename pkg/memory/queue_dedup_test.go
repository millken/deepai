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
