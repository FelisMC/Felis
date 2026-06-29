package main

import (
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// felisTheme recolors huh's base theme to the Felis palette (cyan primary,
// purple accent, green success) so the forms feel like part of the product
// rather than a generic huh prompt. We start from ThemeBase — a neutral
// skeleton with the right structure — and override only the colored bits.
func felisTheme() *huh.Theme {
	t := huh.ThemeBase()

	f := &t.Focused
	f.Base = f.Base.BorderForeground(cPrimary)
	f.Card = f.Base
	f.Title = lipgloss.NewStyle().Foreground(cPrimary).Bold(true)
	f.NoteTitle = f.Title
	f.Description = lipgloss.NewStyle().Foreground(cDim)
	f.ErrorIndicator = lipgloss.NewStyle().Foreground(cError).SetString(" ✗")
	f.ErrorMessage = lipgloss.NewStyle().Foreground(cError)

	f.SelectSelector = lipgloss.NewStyle().Foreground(cPrimary).SetString("▸ ")
	// Unselected rows recede to dim and the active row is bright cyan + bold, so
	// the list reads at a glance as a picker with one row chosen — not a wall of
	// equally-lit info text. huh's single-select paints UnselectedOption (not
	// Option), which ThemeBase leaves bright; overriding it is the key fix.
	f.Option = lipgloss.NewStyle().Foreground(cDim)
	f.UnselectedOption = lipgloss.NewStyle().Foreground(cDim)
	f.SelectedOption = lipgloss.NewStyle().Foreground(cPrimary).Bold(true)
	f.NextIndicator = lipgloss.NewStyle().Foreground(cAccent).MarginLeft(1).SetString("→")
	f.PrevIndicator = lipgloss.NewStyle().Foreground(cAccent).MarginRight(1).SetString("←")

	f.TextInput.Cursor = lipgloss.NewStyle().Foreground(cPrimary)
	f.TextInput.Prompt = lipgloss.NewStyle().Foreground(cPrimary)
	f.TextInput.Placeholder = lipgloss.NewStyle().Foreground(cDim)
	f.TextInput.Text = lipgloss.NewStyle().Foreground(cWhite)

	button := lipgloss.NewStyle().Padding(0, 2).MarginRight(1)
	f.FocusedButton = button.Foreground(cWhite).Background(cPrimary).Bold(true)
	f.BlurredButton = button.Foreground(cWhite).Background(cBgDark)

	// Blurred mirrors focused but hides the left bar and dims the title so the
	// active field is unmistakable.
	t.Blurred = *f
	t.Blurred.Base = f.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Card = t.Blurred.Base
	t.Blurred.Title = lipgloss.NewStyle().Foreground(cDim).Bold(true)
	t.Blurred.NextIndicator = lipgloss.NewStyle()
	t.Blurred.PrevIndicator = lipgloss.NewStyle()

	// Help line at the bottom of every form.
	t.Help.ShortKey = lipgloss.NewStyle().Foreground(cPrimary)
	t.Help.ShortDesc = lipgloss.NewStyle().Foreground(cDim)
	t.Help.ShortSeparator = lipgloss.NewStyle().Foreground(cDim)
	t.Help.FullKey = t.Help.ShortKey
	t.Help.FullDesc = t.Help.ShortDesc
	t.Help.FullSeparator = t.Help.ShortSeparator

	return t
}

// theme is built once; lipgloss styles are immutable values so sharing is safe.
var felisFormTheme = felisTheme()

// newFelisForm wires a form to the Felis theme with the conventions every wizard
// screen wants: themed, help line shown, errors shown inline.
func newFelisForm(groups ...*huh.Group) *huh.Form {
	return huh.NewForm(groups...).
		WithTheme(felisFormTheme).
		WithShowHelp(true).
		WithShowErrors(true)
}
