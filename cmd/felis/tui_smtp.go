package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/platform"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// ---- Email (SMTP) relay: the post-install "configure email" screen ----
//
// Bootstrap deliberately has no SMTP (the Owner's address is recorded
// unverified and the passkey is the only pre-SMTP credential), so this screen
// is where a deployment gains real mail: email verification, email-OTP login
// and the op-login mailbox factor all start working once it applies. It is
// reached from the summary/status screen ("e"), mirroring "change storage".

// smtpInputs is the operator-entered relay coordinates. Only host/port/from/
// username reach felis.toml; the password goes into the felis-smtp Secret.
type smtpInputs struct {
	host     string
	port     string
	from     string
	username string
	password string
}

// reconfigureSMTPMsg is sent from the summary/status screen to open the email
// relay form — the supported way to configure or fix SMTP after install,
// without hand-editing felis.toml and the Secret.
type reconfigureSMTPMsg struct{}

// smtpResultMsg returns control to the root once the screen is done: applied
// (configured=true, with a recap) or backed out of (configured=false).
type smtpResultMsg struct {
	configured bool
	detail     string
}

type smtpStep int

const (
	esForm smtpStep = iota
	esWorking
	esDone
	esError
)

type smtpApplyMsg struct{ err error }

// smtpModel drives the email relay form: collect → verify+apply → done/error,
// the storageModel machine with a single form and no chooser.
type smtpModel struct {
	step smtpStep
	form *huh.Form
	sp   spinner.Model
	err  error
	in   smtpInputs

	width, height int
}

func newSMTPModel(in smtpInputs) *smtpModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel

	if in.port == "" {
		in.port = "587"
	}
	m := &smtpModel{step: esForm, sp: sp, in: in}
	m.form = m.build()
	return m
}

func (m *smtpModel) build() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().
			Title("Email (SMTP)").
			Description("The relay Felis mails one-time codes through — email verification, email login and operator sign-in all need it. The password goes into a Kubernetes Secret; only the other fields are written to felis.toml."),
		huh.NewInput().
			Title("SMTP host").
			Description("Your provider's relay, e.g. smtp.gmail.com or smtp.mailgun.org.").
			Value(&m.in.host).
			Validate(requiredStorageField("SMTP host")),
		huh.NewInput().
			Title("Port").
			Description("587 = STARTTLS (most providers) · 465 = implicit TLS.").
			Value(&m.in.port).
			Validate(validateSMTPPort),
		huh.NewInput().
			Title("From address").
			Description("The sender codes are mailed as, e.g. felis@your-domain.").
			Value(&m.in.from).
			Validate(validateSMTPFrom),
		huh.NewInput().
			Title("Username").
			Description("Optional — leave blank for an unauthenticated relay.").
			Value(&m.in.username),
		huh.NewInput().
			Title("Password").
			Description("Required when a username is set.").
			EchoMode(huh.EchoModePassword).
			Value(&m.in.password),
	)))
}

func (m *smtpModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *smtpModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *smtpModel) Init() tea.Cmd { return m.form.Init() }

func (m *smtpModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case smtpApplyMsg:
		if msg.err != nil {
			m.step, m.err = esError, msg.err
			return m, nil
		}
		m.step = esDone
		return m, nil

	case spinner.TickMsg:
		if m.step == esWorking {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case esForm:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				return m, smtpEmit(smtpResultMsg{})
			}
		case esDone:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, smtpEmit(smtpResultMsg{configured: true, detail: smtpDetail(m.in)})
			}
			return m, nil
		case esError:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.step, m.err = esForm, nil
				m.form = m.build()
				return m, m.form.Init()
			case "enter":
				m.step, m.err = esWorking, nil
				return m, tea.Batch(m.sp.Tick, m.apply())
			}
			return m, nil
		case esWorking:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		}
	}

	if m.step == esForm && m.form != nil {
		form, cmd := m.form.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.form = f
		}
		switch m.form.State {
		case huh.StateCompleted:
			m.normalizeInputs()
			m.step = esWorking
			return m, tea.Batch(m.sp.Tick, m.apply())
		case huh.StateAborted:
			return m, smtpEmit(smtpResultMsg{})
		}
		return m, cmd
	}
	return m, nil
}

func smtpEmit(msg smtpResultMsg) tea.Cmd { return func() tea.Msg { return msg } }

func (m *smtpModel) apply() tea.Cmd {
	in := m.in
	return func() tea.Msg {
		return smtpApplyMsg{err: applySMTPConfig(context.Background(), in)}
	}
}

func (m *smtpModel) normalizeInputs() {
	m.in.host = strings.TrimSpace(m.in.host)
	m.in.port = strings.TrimSpace(m.in.port)
	m.in.from = strings.TrimSpace(m.in.from)
	m.in.username = strings.TrimSpace(m.in.username)
	m.in.password = strings.TrimSpace(m.in.password)
}

func (m *smtpModel) View() string {
	switch m.step {
	case esWorking:
		return "  " + m.sp.View() + " " + tuiHint.Render("Verifying the relay, saving email settings and rolling the API…") + "\n"
	case esDone:
		var b strings.Builder
		b.WriteString(tuiSuccessBanner("Email configured — codes are now mailed.") + "\n\n")
		b.WriteString(tuiInfo("Relay → "+smtpDetail(m.in)) + "\n")
		b.WriteString("\n" + tuiAction("enter", "continue"))
		return b.String()
	case esError:
		var b strings.Builder
		b.WriteString(tuiErrorBanner("Could not configure email.") + "\n\n")
		if m.err != nil {
			b.WriteString(tuiHint.Render(m.err.Error()) + "\n")
		}
		b.WriteString("\n" + tuiAction("enter", "retry", "esc", "edit"))
		return b.String()
	default:
		if m.form == nil {
			return ""
		}
		return m.form.View()
	}
}

// arrowNavOK yields the horizontal arrows to the rail except while the form is
// taking text input (where ←/→ move the cursor).
func (m *smtpModel) arrowNavOK() bool { return m.step != esForm }

// smtpDetail is the one-line relay recap shown on the done screen.
func smtpDetail(in smtpInputs) string {
	return in.host + ":" + in.port + "  ·  from " + in.from
}

func validateSMTPPort(s string) error {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be a number 1-65535 (587 STARTTLS, 465 implicit TLS)")
	}
	return nil
}

func validateSMTPFrom(s string) error {
	if !strings.Contains(strings.TrimSpace(s), "@") {
		return errors.New("from must be the sender email address")
	}
	return nil
}

// currentSMTPInputs reads the relay already recorded in felis.toml so the form
// pre-fills the non-secret fields. The password lives only in the felis-smtp
// Secret and is deliberately never read back — it must be re-entered to change.
// Any read error falls back to a blank form rather than blocking reconfig.
func currentSMTPInputs() smtpInputs {
	cfg, err := config.Load(hostSetupConfigPath)
	if err != nil || cfg.SMTP.Host == "" {
		return smtpInputs{}
	}
	return smtpInputs{
		host:     cfg.SMTP.Host,
		port:     strconv.Itoa(cfg.SMTP.Port),
		from:     cfg.SMTP.From,
		username: cfg.SMTP.Username,
	}
}

// applySMTPConfig proves the relay works, then persists it and rolls felis-api:
// Ping (connect/STARTTLS/AUTH, no mail sent) → [smtp] into both config files →
// the felis-smtp Secret → the config Secret → rollout. A failed Ping leaves the
// install untouched, so a typo dies at the keyboard, not at a player's OTP.
func applySMTPConfig(ctx context.Context, in smtpInputs) error {
	port, err := strconv.Atoi(in.port)
	if err != nil {
		return fmt.Errorf("port %q is not a number", in.port)
	}
	relay := &mail.SMTP{Host: in.host, Port: port, From: in.from, Username: in.username, Password: in.password}
	if err := relay.Ping(ctx); err != nil {
		return err
	}

	smtpCfg := config.SMTPConfig{
		Host:        in.host,
		Port:        port,
		From:        in.from,
		Username:    in.username,
		PasswordRef: platform.SMTPPasswordEnv,
	}
	for _, path := range []string{hostSetupConfigPath, podSetupConfigPath} {
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		cfg.SMTP = smtpCfg
		if err := writeConfig(path, cfg); err != nil {
			return err
		}
	}
	if err := applySMTPSecret(ctx, in.password); err != nil {
		return err
	}
	if err := applyFelisConfigSecret(ctx); err != nil {
		return err
	}
	if err := kubectl(ctx, "-n", "felis", "rollout", "restart", "deployment/felis-api"); err != nil {
		return err
	}
	return kubectl(ctx, "-n", "felis", "rollout", "status", "deployment/felis-api", "--timeout=180s")
}

// applySMTPSecret creates (or replaces) the felis-smtp Secret the felis-api
// Deployment injects the relay password from. Rendered in-process and piped to
// `kubectl apply` — the password is never a command-line arg, so it never
// appears in the host process table.
func applySMTPSecret(ctx context.Context, password string) error {
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: platform.SMTPSecretName, Namespace: "felis"},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			platform.SMTPSecretPasswordKey: password,
		},
	}
	manifest, err := yaml.Marshal(secret)
	if err != nil {
		return fmt.Errorf("render smtp secret: %w", err)
	}
	return kubectlWithInput(ctx, manifest, "apply", "-f", "-")
}
