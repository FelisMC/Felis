package main

import (
	"fmt"
	"strings"
)

func tuiAction(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, tuiLabel.Render(pairs[i])+" "+tuiHint.Render(pairs[i+1]))
	}
	return tuiActionBar.Render(strings.Join(parts, "  ·  "))
}

func tuiInfo(text string) string {
	return tuiInfoBox.Render(tuiHint.Render("ℹ " + text))
}

func tuiErrorBanner(msg string) string {
	return tuiCardFocusedStyle.Render(tuiErr.Render("✗ " + msg))
}

func tuiSuccessBanner(msg string) string {
	return tuiCardFocusedStyle.Render(tuiOK.Render("✓ " + msg))
}

func tuiSeparator() string {
	return tuiHint.Render(strings.Repeat("─", 70))
}

// wrapDisplayURL breaks a long URL into lines no wider than width so the TUI
// renderer never truncates it on a narrow terminal — the one-time setup URL
// carries a 43-char token and overruns 80 columns. It prefers breaking right
// after a '=' or '/' inside the window (the token then lands on its own line)
// and hard-wraps only when no boundary is available. Lines concatenate back to
// the original string.
func wrapDisplayURL(u string, width int) []string {
	if width <= 0 {
		width = 70
	}
	var lines []string
	for len(u) > width {
		cut := width
		if i := strings.LastIndexByte(u[:width], '='); i >= 0 && i >= width/2 {
			cut = i + 1
		} else if i := strings.LastIndexByte(u[:width], '/'); i >= 0 && i >= width/2 {
			cut = i + 1
		}
		lines = append(lines, u[:cut])
		u = u[cut:]
	}
	if u != "" {
		lines = append(lines, u)
	}
	return lines
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
