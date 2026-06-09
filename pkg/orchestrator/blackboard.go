package orchestrator

import "strings"

type Blackboard struct {
	Plan  string
	notes []string
}

func (b *Blackboard) AddNote(note string) {
	if b == nil {
		return
	}
	if n := strings.TrimSpace(note); n != "" {
		b.notes = append(b.notes, n)
	}
}

func (b *Blackboard) Render() string {
	if b == nil {
		return ""
	}
	var sb strings.Builder
	if p := strings.TrimSpace(b.Plan); p != "" {
		sb.WriteString("## Approved plan (agreed for this task)\n")
		sb.WriteString(p)
	}
	if len(b.notes) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("## Decisions & notes so far\n")
		for _, n := range b.notes {
			sb.WriteString("- ")
			sb.WriteString(n)
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
