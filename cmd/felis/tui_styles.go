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

	// Title bar — inverted primary
	tuiTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(cWhite).
			Background(cPrimary).
			Padding(0, 2).
			Width(70)

	// Section header
	tuiSection = lipgloss.NewStyle().
			Bold(true).
			Foreground(cPrimary).
			Padding(0, 1)

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
	tuiIconOK    = lipgloss.NewStyle().Bold(true).Foreground(cSuccess).Render("✓")
	tuiIconInPro = lipgloss.NewStyle().Bold(true).Foreground(cPrimary).Render("→")
	tuiIconOpt   = lipgloss.NewStyle().Foreground(cWhite).Render("○")
	tuiIconErr   = lipgloss.NewStyle().Bold(true).Foreground(cError).Render("✗")
	tuiIconSpin  = lipgloss.NewStyle().Bold(true).Foreground(cWarning).Render("⟳")

	// Step card dimensions
	tuiStepWidth  = 60
	tuiStepHeight = 5

	// Info box — subtle background
	tuiInfoBox = lipgloss.NewStyle().
			Background(cBgDark).
			Padding(1, 2).
			Width(60)
)

func tuiIcon(status stepStatus) string {
	switch status {
	case statusDone:
		return tuiIconOK
	case statusPending:
		return tuiIconInPro
	case statusOptional:
		return tuiIconOpt
	case statusFailed:
		return tuiIconErr
	case statusRunning:
		return tuiIconSpin
	default:
		return " "
	}
}
