package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
)

type stepStatus int

const (
	statusDone     stepStatus = iota
	statusPending
	statusOptional
	statusRunning
	statusFailed
)

type dashboardStep struct {
	title  string
	status stepStatus
	detail string
}

func tuiHeader(title string) string {
	return tuiTitle.Render("🐾 " + title) + "\n\n"
}

func tuiAction(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, tuiLabel.Render(pairs[i])+" "+tuiHint.Render(pairs[i+1]))
	}
	return tuiActionBar.Render(strings.Join(parts, "  ·  "))
}

func tuiStatusLine(icon, label, detail string, focused bool) string {
	line := fmt.Sprintf("  %s  %s", icon, tuiLabel.Render(label))
	if detail != "" {
		line += "\n     " + tuiHint.Render(detail)
	}
	if focused {
		return tuiCardFocusedStyle.Width(tuiStepWidth).Render(line)
	}
	return tuiCardStyle.Width(tuiStepWidth).Render(line)
}

func tuiFormField(label string, input textinput.Model) string {
	return fmt.Sprintf("%s\n%s",
		tuiLabel.Render(label),
		input.View())
}

func tuiInfo(text string) string {
	return tuiInfoBox.Render(tuiHint.Render("ℹ " + text))
}

type progressStep struct {
	label string
	done  bool
}

func tuiProgress(steps []progressStep) string {
	var b strings.Builder
	for _, s := range steps {
		if s.done {
			b.WriteString("  " + tuiIconOK + " " + s.label + "\n")
		} else if len(steps) > 0 && s == steps[0] {
			continue
		} else {
			b.WriteString("  " + tuiIconOpt + " " + s.label + "\n")
		}
	}
	return b.String()
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

func tuiResultCard(title string, pairs ...string) string {
	var b strings.Builder
	b.WriteString(tuiOK.Render(title) + "\n\n")
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString(tuiLabel.Render(pairs[i]) + "  " + tuiPassword.Render(pairs[i+1]) + "\n")
	}
	return tuiCardStyle.Render(b.String())
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
