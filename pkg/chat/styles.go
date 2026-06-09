package chat

import "charm.land/lipgloss/v2"

// Styles holds all terminal styling definitions.
type Styles struct {
	Banner       lipgloss.Style
	BannerDim    lipgloss.Style
	UserPrompt   lipgloss.Style
	Assistant    lipgloss.Style
	ToolCall     lipgloss.Style
	ToolResult   lipgloss.Style
	Error        lipgloss.Style
	Stats        lipgloss.Style
	Compaction   lipgloss.Style
	Dim          lipgloss.Style
	Bold         lipgloss.Style
	Highlight    lipgloss.Style
	Separator    lipgloss.Style
	SeverityCrit lipgloss.Style
	SeverityWarn lipgloss.Style
	SeveritySugg lipgloss.Style
	ReviewPass   lipgloss.Style
	ReviewFail   lipgloss.Style
	DiffAdd      lipgloss.Style
	DiffDel      lipgloss.Style
}

// DefaultStyles returns the default style palette.
func DefaultStyles() Styles {
	return Styles{
		Banner: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFD700")).
			Bold(true),
		BannerDim: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888")),
		UserPrompt: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7DCFFF")).
			Bold(true),
		Assistant: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFF8DC")),
		ToolCall: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFBF00")),
		ToolResult: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9ECE6A")),
		Error: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555")).
			Bold(true),
		Stats: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086")),
		Compaction: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086")),
		Dim: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086")),
		Bold: lipgloss.NewStyle().
			Bold(true),
		Highlight: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7AA2F7")),
		Separator: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#3B4261")),
		SeverityCrit: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555")).Bold(true),
		SeverityWarn: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFBF00")),
		SeveritySugg: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7AA2F7")),
		ReviewPass: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9ECE6A")),
		ReviewFail: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555")).Bold(true),
		DiffAdd: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9ECE6A")),
		DiffDel: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F7768E")),
	}
}
