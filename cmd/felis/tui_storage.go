package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"felis.lolicon.best/internal/platform"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// ---- Storage backends ----

type storageMethod int

const (
	storageLocal storageMethod = iota
	storageS3
)

func storageMethodLabel(m storageMethod) string {
	if m == storageS3 {
		return "Object storage (S3-compatible)"
	}
	return "Local disk (on this node)"
}

// s3Inputs is the operator-entered coordinates for the S3 backend. Only endpoint,
// bucket and region reach felis.toml; the two keys go into a Kubernetes Secret.
type s3Inputs struct {
	endpoint  string
	bucket    string
	region    string
	accessKey string
	secretKey string
}

// storageChooserModel presents the two build-context storage backends as peer
// choices. "Local" persists uploaded modpacks on a node-local PVC (nothing else to
// configure); "S3" points them at an S3-compatible bucket. Both are functional; S3
// suits external object storage. It mirrors connectChooserModel's shape so the two
// mid-wizard forks read identically.
type storageChooserModel struct {
	rootDomain string

	form          *huh.Form
	choice        storageMethod
	prefill       s3Inputs // pre-filled non-secret fields when re-entering to change storage
	width, height int
}

// newStorageChooserModel opens the storage picker pre-selected on method and, for
// S3, carrying prefill's non-secret fields (endpoint/bucket/region) into the detail
// form. First-run callers pass (storageLocal, s3Inputs{}); the reconfigure path
// passes the backend already in felis.toml so an operator fixing a typo doesn't
// retype everything (credentials still must be re-entered — they live only in the
// Secret).
func newStorageChooserModel(rootDomain string, method storageMethod, prefill s3Inputs) *storageChooserModel {
	m := &storageChooserModel{rootDomain: rootDomain, choice: method, prefill: prefill}
	m.form = m.build()
	return m
}

func (m *storageChooserModel) build() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewSelect[storageMethod]().
			Title("Where should uploaded modpacks be stored?").
			Description("Players submit modpacks for review; approved ones are built from here. You can change this later.").
			Value(&m.choice).
			Options(
				huh.NewOption("Local disk · nothing else to set up", storageLocal),
				huh.NewOption("Object storage · S3-compatible bucket", storageS3),
			),
		// A dim footnote, subordinate to the picker — the connect-chooser pattern.
		huh.NewNote().Description(
			"⚠  Local keeps uploads on this node's disk — simplest, ideal for a single-node install. "+
				"Choose S3 for external object storage (AWS S3, MinIO, Cloudflare R2, …)."),
	)))
}

func (m *storageChooserModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *storageChooserModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *storageChooserModel) Init() tea.Cmd { return m.form.Init() }

func (m *storageChooserModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			// Skipping is choosing local — the simplest backend, changeable later.
			return newStorageModel(storageLocal, s3Inputs{}), nil
		}
	}

	form, cmd := m.form.Update(msg)
	if f, ok := form.(*huh.Form); ok {
		m.form = f
	}
	switch m.form.State {
	case huh.StateCompleted:
		return newStorageModel(m.choice, m.prefill), nil
	case huh.StateAborted:
		return m, tea.Quit
	}
	return m, cmd
}

func (m *storageChooserModel) View() string { return m.form.View() }

// arrowNavOK lets the root repurpose ←/→ to walk the step rail: the picker uses
// ↑/↓, so the horizontal arrows are free.
func (m *storageChooserModel) arrowNavOK() bool { return true }

// ---- Storage apply: local applies immediately, S3 collects then applies ----

type storageStep int

const (
	ssForm storageStep = iota // S3 only: collect endpoint/bucket/keys
	ssWorking
	ssDone
	ssError
)

type storageApplyMsg struct{ err error }

// storageResultMsg is the method-agnostic outcome the root advances on, emitted
// once the chosen backend is written and the API has rolled.
type storageResultMsg struct {
	method storageMethod
	detail string
}

// storageBackMsg returns from the storage sub-screen to the chooser.
type storageBackMsg struct{}

func goBackStorage() tea.Cmd { return func() tea.Msg { return storageBackMsg{} } }

// storageModel drives applying the chosen backend. Local has no form and applies
// straight away; S3 first collects its coordinates. Both share the working → done /
// error machine, mirroring reverseProxyModel.
type storageModel struct {
	method storageMethod
	step   storageStep
	form   *huh.Form
	sp     spinner.Model
	err    error
	in     s3Inputs

	width, height int
}

func newStorageModel(method storageMethod, in s3Inputs) *storageModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel

	m := &storageModel{method: method, sp: sp, in: in}
	if method == storageS3 {
		m.step = ssForm
		m.form = m.build()
	} else {
		m.step = ssWorking
	}
	return m
}

func (m *storageModel) build() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().
			Title("Object storage (S3-compatible)").
			Description("Enter your bucket and credentials. The keys go into a Kubernetes Secret; only the endpoint and bucket are written to felis.toml."),
		huh.NewInput().
			Title("Endpoint").
			Description("Your S3 API host, e.g. s3.amazonaws.com or minio.example.com:9000 (prefix http:// for a plaintext dev store).").
			Value(&m.in.endpoint).
			Validate(requiredStorageField("endpoint")),
		huh.NewInput().
			Title("Bucket").
			Description("An existing bucket uploads are written to.").
			Value(&m.in.bucket).
			Validate(validateBucketName),
		huh.NewInput().
			Title("Region").
			Description("Optional — leave blank for MinIO / R2.").
			Value(&m.in.region),
		huh.NewInput().
			Title("Access key ID").
			Value(&m.in.accessKey).
			Validate(requiredStorageField("access key ID")),
		huh.NewInput().
			Title("Secret access key").
			EchoMode(huh.EchoModePassword).
			Value(&m.in.secretKey).
			Validate(requiredStorageField("secret access key")),
	)))
}

func (m *storageModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *storageModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *storageModel) Init() tea.Cmd {
	if m.method == storageS3 {
		return m.form.Init()
	}
	// Local: the chooser already confirmed the choice, so apply immediately.
	return tea.Batch(m.sp.Tick, m.apply())
}

func (m *storageModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case storageApplyMsg:
		if msg.err != nil {
			m.step, m.err = ssError, msg.err
			return m, nil
		}
		m.step = ssDone
		return m, nil

	case spinner.TickMsg:
		if m.step == ssWorking {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case ssForm:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				return m, goBackStorage()
			}
		case ssDone:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, m.emit()
			}
			return m, nil
		case ssError:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				if m.method == storageS3 {
					m.step, m.err = ssForm, nil
					m.form = m.build()
					return m, m.form.Init()
				}
				return m, goBackStorage()
			case "enter":
				m.step, m.err = ssWorking, nil
				return m, tea.Batch(m.sp.Tick, m.apply())
			}
			return m, nil
		case ssWorking:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		}
	}

	if m.step == ssForm && m.form != nil {
		form, cmd := m.form.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.form = f
		}
		switch m.form.State {
		case huh.StateCompleted:
			m.normalizeInputs()
			m.step = ssWorking
			return m, tea.Batch(m.sp.Tick, m.apply())
		case huh.StateAborted:
			return m, goBackStorage()
		}
		return m, cmd
	}
	return m, nil
}

func (m *storageModel) apply() tea.Cmd {
	method, in := m.method, m.in
	return func() tea.Msg {
		return storageApplyMsg{err: applyStorageConfig(context.Background(), method, in)}
	}
}

func (m *storageModel) emit() tea.Cmd {
	method, detail := m.method, storageDetail(m.method, m.in)
	return func() tea.Msg {
		return storageResultMsg{method: method, detail: detail}
	}
}

func (m *storageModel) normalizeInputs() {
	m.in.endpoint = strings.TrimSpace(m.in.endpoint)
	m.in.bucket = strings.TrimSpace(m.in.bucket)
	m.in.region = strings.TrimSpace(m.in.region)
	m.in.accessKey = strings.TrimSpace(m.in.accessKey)
	m.in.secretKey = strings.TrimSpace(m.in.secretKey)
}

func (m *storageModel) View() string {
	switch m.step {
	case ssWorking:
		return "  " + m.sp.View() + " " + tuiHint.Render("Saving storage settings and rolling the API…") + "\n"
	case ssDone:
		var b strings.Builder
		if m.method == storageS3 {
			b.WriteString(tuiSuccessBanner("Object storage configured.") + "\n\n")
			b.WriteString(tuiInfo("Uploads → s3://"+m.in.bucket+"  ·  "+m.in.endpoint) + "\n")
		} else {
			b.WriteString(tuiSuccessBanner("Local storage configured.") + "\n\n")
			b.WriteString(tuiInfo("Uploads → "+platform.UploadsLocalPath+" on this node") + "\n")
		}
		b.WriteString("\n" + tuiAction("enter", "continue"))
		return b.String()
	case ssError:
		var b strings.Builder
		b.WriteString(tuiErrorBanner("Could not save storage settings.") + "\n\n")
		if m.err != nil {
			b.WriteString(tuiHint.Render(m.err.Error()) + "\n")
		}
		if m.method == storageS3 {
			b.WriteString("\n" + tuiAction("enter", "retry", "esc", "edit"))
		} else {
			b.WriteString("\n" + tuiAction("enter", "retry", "esc", "back"))
		}
		return b.String()
	default:
		if m.form == nil {
			return ""
		}
		return m.form.View()
	}
}

// arrowNavOK yields the horizontal arrows to the rail except while the S3 form is
// taking text input (where ←/→ move the cursor).
func (m *storageModel) arrowNavOK() bool { return m.step != ssForm }

// storageDetail is the one-line backend recap shown on the summary and in review.
func storageDetail(method storageMethod, in s3Inputs) string {
	if method == storageS3 {
		return "s3://" + in.bucket + "  ·  " + in.endpoint
	}
	return "local disk · " + platform.UploadsLocalPath
}

// requiredStorageField rejects a blank value with a field-named message.
func requiredStorageField(name string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
}

// bucketNameRE is a pragmatic S3 bucket-name check: 3–63 chars, lowercase
// letters/digits/dots/hyphens, starting and ending alphanumeric. It catches typos
// without trying to encode every provider's exact rules.
var bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`)

func validateBucketName(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("bucket is required")
	}
	if !bucketNameRE.MatchString(s) {
		return errors.New("bucket must be 3–63 chars: lowercase letters, digits, dots or hyphens")
	}
	return nil
}
