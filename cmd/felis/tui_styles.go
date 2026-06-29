package main

import "github.com/charmbracelet/lipgloss"

var (
	// Color palette — semantic, terminal-safe
	cPrimary = lipgloss.Color("39")  // bright cyan-blue
	cSuccess = lipgloss.Color("42")  // green
	cWarning = lipgloss.Color("214") // orange
	cError   = lipgloss.Color("196") // red
	cDim     = lipgloss.Color("240") // gray
	cAccent  = lipgloss.Color("99")  // purple
	cBgDark  = lipgloss.Color("236") // dark gray background
	cBgInput = lipgloss.Color("235") // input field bg
	cWhite   = lipgloss.Color("15")

	// Card styles
	tuiCardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cDim).
			Padding(1, 2)

	tuiCardFocusedStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(cPrimary).
				Padding(1, 2)

	// Form label
	tuiLabel = lipgloss.NewStyle().
			Bold(true).
			Foreground(cPrimary)

	// Hint / help text
	tuiHint = lipgloss.NewStyle().
		Foreground(cWhite)

	// Success text
	tuiOK = lipgloss.NewStyle().
		Bold(true).
		Foreground(cSuccess)

	// Warning text
	tuiWarn = lipgloss.NewStyle().
		Bold(true).
		Foreground(cWarning)

	// Error text
	tuiErr = lipgloss.NewStyle().
		Bold(true).
		Foreground(cError)

	// Password display — inverted highlight
	tuiPassword = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("0")).
			Background(cWarning).
			Padding(0, 1)

	// Action bar — bottom stripe
	tuiActionBar = lipgloss.NewStyle().
			Foreground(cWhite).
			Padding(0, 1)

	// Status icon styles
	tuiIconOK   = lipgloss.NewStyle().Bold(true).Foreground(cSuccess).Render("✓")
	tuiIconOpt  = lipgloss.NewStyle().Foreground(cWhite).Render("○")
	tuiIconSpin = lipgloss.NewStyle().Bold(true).Foreground(cWarning).Render("⟳")

	// Info box — subtle background
	tuiInfoBox = lipgloss.NewStyle().
			Background(cBgDark).
			Padding(1, 2).
			Width(60)

	// Step rail (breadcrumb) — shows where you are in the linear wizard.
	tuiRailDone   = lipgloss.NewStyle().Foreground(cSuccess)
	tuiRailActive = lipgloss.NewStyle().Bold(true).Foreground(cWhite).Background(cPrimary).Padding(0, 1)
	tuiRailTodo   = lipgloss.NewStyle().Foreground(cDim)
	tuiRailSep    = lipgloss.NewStyle().Foreground(cDim)

	// Code/guide block — monospace-ish snippet on a subtle background.
	tuiCodeBox = lipgloss.NewStyle().
			Foreground(cWhite).
			Background(cBgInput).
			Padding(0, 1)
)
