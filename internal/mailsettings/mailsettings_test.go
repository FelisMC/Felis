package mailsettings

import (
	"strings"
	"testing"
)

func valid() Settings {
	return Settings{Host: "smtp.example.com", Port: 587, From: "noreply@example.com", Username: "noreply@example.com", Password: "secret", RequireTLS: true}
}

func TestValidateRefusesWhatCannotBeARelay(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("a good relay was refused: %v", err)
	}
	for name, change := range map[string]func(*Settings){
		"empty host":          func(s *Settings) { s.Host = "" },
		"host with spaces":    func(s *Settings) { s.Host = " smtp.example.com" },
		"host with a path":    func(s *Settings) { s.Host = "smtp.example.com/x" },
		"host with userinfo":  func(s *Settings) { s.Host = "user@smtp.example.com" },
		"postgres port":       func(s *Settings) { s.Port = 5432 },
		"kube api port":       func(s *Settings) { s.Port = 6443 },
		"no port":             func(s *Settings) { s.Port = 0 },
		"named sender":        func(s *Settings) { s.From = "Felis <noreply@example.com>" },
		"not an address":      func(s *Settings) { s.From = "noreply" },
		"header in user name": func(s *Settings) { s.Username = "a\r\nBcc: x@y" },
		"huge password":       func(s *Settings) { s.Password = strings.Repeat("x", 2000) },
		"negative cap":        func(s *Settings) { s.MaxPerHour = -1 },
	} {
		s := valid()
		change(&s)
		if s.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, port := range AllowedPorts {
		s := valid()
		s.Port = port
		if err := s.Validate(); err != nil {
			t.Errorf("port %d was refused: %v", port, err)
		}
	}
}

func TestParseTreatsAClearedRowAsAbsent(t *testing.T) {
	for _, raw := range []string{"", "null", " null "} {
		if s, err := Parse([]byte(raw)); s != nil || err != nil {
			t.Errorf("Parse(%q) = %+v, %v; want nil, nil", raw, s, err)
		}
	}
	s, err := Parse([]byte(`{"host":"smtp.example.com","port":465,"from":"a@example.com","username":"a@example.com","password":"pw","require_tls":true}`))
	if err != nil || s == nil || s.Password != "pw" || s.Port != 465 {
		t.Fatalf("Parse = %+v, %v", s, err)
	}
	if smtp := s.SMTP(); smtp.Control == nil {
		t.Error("the relay is dialled without the link-local guard")
	}
	if smtp := s.SMTP(); smtp.Host != "smtp.example.com" || smtp.Password != "pw" || !smtp.RequireTLS {
		t.Errorf("SMTP() = %+v", smtp)
	}
	if _, err := Parse([]byte(`{"host":"","port":587}`)); err == nil {
		t.Error("an invalid stored relay parsed")
	}
}
