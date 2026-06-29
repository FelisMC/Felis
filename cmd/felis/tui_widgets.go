package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
)

func tuiHeader(title string) string {
	return tuiTitle.Render("🐾 "+title) + "\n\n"
}

func tuiAction(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, tuiLabel.Render(pairs[i])+" "+tuiHint.Render(pairs[i+1]))
	}
	return tuiActionBar.Render(strings.Join(parts, "  ·  "))
}

func tuiFormField(label string, input textinput.Model) string {
	return fmt.Sprintf("%s\n%s",
		tuiLabel.Render(label),
		input.View())
}

func tuiInfo(text string) string {
	return tuiInfoBox.Render(tuiHint.Render("ℹ " + text))
}

func tuiWizardCard(title, desc, body string) string {
	var b strings.Builder
	b.WriteString(tuiSection.Render(title))
	if desc != "" {
		b.WriteString("\n")
		b.WriteString(tuiHint.Render(desc))
	}
	b.WriteString("\n\n")
	b.WriteString(tuiCardStyle.Render(body))
	return b.String()
}

func tuiErrorBanner(msg string) string {
	return tuiCardFocusedStyle.Render(tuiErr.Render("✗ " + msg))
}

func tuiSuccessBanner(msg string) string {
	return tuiCardFocusedStyle.Render(tuiOK.Render("✓ " + msg))
}

func tuiInput(placeholder string, charLimit int, password bool) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.CharLimit = charLimit
	ti.Width = 44
	ti.Prompt = ""
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(cWhite)
	if password {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '•'
	}
	return ti
}

func tuiSeparator() string {
	return tuiHint.Render(strings.Repeat("─", 70))
}

// tuiStepRail renders a breadcrumb of wizard stages. Steps before `current`
// render as done, `current` is highlighted, and later steps are dimmed.
func tuiStepRail(steps []string, current int) string {
	var parts []string
	for i, s := range steps {
		switch {
		case i < current:
			parts = append(parts, tuiRailDone.Render("✓ "+s))
		case i == current:
			parts = append(parts, tuiRailActive.Render(fmt.Sprintf("%d. %s", i+1, s)))
		default:
			parts = append(parts, tuiRailTodo.Render(fmt.Sprintf("%d. %s", i+1, s)))
		}
	}
	return tuiRailSep.Render("  ") + strings.Join(parts, tuiRailSep.Render("  →  "))
}

// tuiGuideBlock renders a titled, copy-pasteable snippet (e.g. a Caddyfile).
func tuiGuideBlock(title, body string) string {
	var b strings.Builder
	if title != "" {
		b.WriteString(tuiLabel.Render(title) + "\n")
	}
	for _, line := range strings.Split(body, "\n") {
		b.WriteString(tuiCodeBox.Render(line) + "\n")
	}
	return b.String()
}
