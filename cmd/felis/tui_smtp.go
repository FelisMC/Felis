package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
			Description("The relay Felis mails one-time codes through — email verification, email login and operator sign-in all need it. The password goes into a Kubernetes Secret; only the other fields are written to felis.toml. Saving sends one self-test message to the From address: nothing is written unless it is delivered."),
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
			Description("The sender codes are mailed as, e.g. felis@your-domain. It must be an address this account is allowed to send as — providers reject a From on a domain you have not verified with them, and they usually do it only after the message body, not when you connect.").
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
		return "  " + m.sp.View() + " " + tuiHint.Render("Delivering a self-test message, saving email settings and rolling the API…") + "\n"
	case esDone:
		var b strings.Builder
		b.WriteString(tuiSuccessBanner("Email configured — codes are now mailed.") + "\n\n")
		b.WriteString(tuiInfo("Relay → "+smtpDetail(m.in)) + "\n")
		// Named because it is checkable: the operator can open that inbox and see the
		// proof, rather than taking "configured" on faith.
		b.WriteString(tuiHint.Render("A self-test message was delivered to "+m.in.from+".") + "\n")
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
// Ping (a full transaction — connect/STARTTLS/AUTH/MAIL FROM/RCPT/DATA, which
// delivers one self-test message to the From address) → [smtp] into both config
// files → the felis-smtp Secret → the config Secret → rollout. A failed Ping
// leaves the install untouched, so a bad relay dies at the keyboard, not at a
// player's OTP.
//
// Ping really sends, because a cheaper probe cannot answer the question this
// screen exists to answer. Relays that validate sender identity — Fastmail, and
// it is not alone — return an unconditional 250 to MAIL FROM and only refuse at
// end-of-DATA. The earlier connect/AUTH/NOOP check therefore accepted a From on
// a domain the account could not send as, wrote the config, and left every OTP
// failing afterwards with this screen reporting success.
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
	// Refresh the workload-namespace copies too (the reaper's warning path): the
	// OTP path is already live in the control namespace, so a replica miss is
	// reported but not fatal.
	if err := replicateSMTPToWorkloadNamespace(ctx, in.password); err != nil {
		fmt.Fprintf(os.Stderr, "felis setup: warning: email is configured, but refreshing the workload copies failed (pre-reap warning emails may stay suppressed): %v\n", err)
	}
	if err := kubectl(ctx, "-n", "felis", "rollout", "restart", "deployment/felis-api"); err != nil {
		return err
	}
	return kubectl(ctx, "-n", "felis", "rollout", "status", "deployment/felis-api", "--timeout=180s")
}

// smtpSecretManifest renders the felis-smtp Secret for the given namespace, the
// one the receiving Deployment/CronJob resolves its secretKeyRef against (felis
// for felis-api, the workload namespace for the reaper's mirror). The namespace
// must be IN the manifest: kubectl rejects a manifest whose namespace conflicts
// with -n, so leaving the control namespace hardcoded made every workload-ns
// replica fail before it started. Rendered in-process and piped to
// `kubectl apply` — the password is never a command-line arg, so it never
// appears in the host process table.
func smtpSecretManifest(password, namespace string) ([]byte, error) {
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: platform.SMTPSecretName, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			platform.SMTPSecretPasswordKey: password,
		},
	}
	manifest, err := yaml.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("render smtp secret: %w", err)
	}
	return manifest, nil
}

func applySMTPSecret(ctx context.Context, password string) error {
	manifest, err := smtpSecretManifest(password, "felis")
	if err != nil {
		return err
	}
	return kubectlWithInput(ctx, manifest, "apply", "-f", "-")
}

// replicateSMTPToWorkloadNamespace refreshes the workload-namespace (minecraft)
// copies of felis-smtp and felis-config after email is reconfigured. The
// reaper's CronJob runs there and resolves both by local reference — a
// secretKeyRef is namespace-local, and `felis setup` creates the felis-config
// replica create-if-absent, so without this refresh a later SMTP change would
// never reach the pre-reap warning emails. Deliberately OVERWRITES both: these
// are mirrors of the control-namespace sources, and a stale mirror is exactly
// the failure this closes.
func replicateSMTPToWorkloadNamespace(ctx context.Context, password string) error {
	cfg, err := config.Load(hostSetupConfigPath)
	if err != nil {
		return err
	}
	ns := cfg.K8s.Namespace
	if ns == "" || ns == "felis" {
		return nil
	}
	smtpManifest, err := smtpSecretManifest(password, ns)
	if err != nil {
		return err
	}
	if err := kubectlWithInput(ctx, smtpManifest, "-n", ns, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("replicate %s to %s: %w", platform.SMTPSecretName, ns, err)
	}
	manifest, err := kubectlOutput(ctx,
		"-n", ns, "create", "secret", "generic", "felis-config",
		"--from-file=felis.toml="+podSetupConfigPath,
		"--dry-run=client", "-o", "yaml",
	)
	if err != nil {
		return fmt.Errorf("render felis-config for %s: %w", ns, err)
	}
	if err := kubectlWithInput(ctx, manifest, "-n", ns, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("replicate felis-config to %s: %w", ns, err)
	}
	return nil
}
