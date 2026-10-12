package connections

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// Connection kinds.
const (
	KindSMTP      = "smtp"
	KindResend    = "resend"
	KindGmail     = "gmail"
	KindGDrive    = "gdrive"
	KindGDocs     = "gdocs"
	KindGSheets   = "gsheets"
	KindGCalendar = "gcalendar"
	KindDropbox   = "dropbox"
	KindOneDrive  = "onedrive"
	KindPostgres  = "postgres"
	KindMySQL     = "mysql"
	KindMCP       = "mcp"
	KindMongo     = "mongo"
	KindSlack     = "slack"
	KindNotion    = "notion"
)

// Field describes one setting of a kind, for validation and for the editor.
type Field struct {
	Name     string
	Title    string
	Type     string // string | int | bool | enum
	Required bool
	Secret   bool
	Enum     []string
	Default  any
	Help     string
}

// KindInfo describes a kind of connection.
type KindInfo struct {
	Kind   string
	Title  string
	Fields []Field
	// OAuth kinds are created by signing in with the provider, not by entering
	// credentials. OAuthConfigured distinguishes available sign-in from setup
	// that still needs an operator to register an OAuth app.
	OAuth           bool
	OAuthConfigured bool
}

type kind struct {
	info KindInfo
	// check normalises and validates the settings. It may fill in defaults.
	check func(config map[string]any, secret map[string]string) error
	// test proves the connection works without side effects.
	test func(ctx context.Context, s *Service, config map[string]any, secret map[string]string) error
	// hintField names the secret whose last characters identify it in the UI.
	hintField string
}

func (s *Service) kinds() map[string]kind {
	registry := map[string]kind{
		KindMCP:      mcpKind(),
		KindMongo:    mongoKind(),
		KindSlack:    tokenKind(KindSlack, "Slack"),
		KindNotion:   tokenKind(KindNotion, "Notion"),
		KindSMTP:     smtpKind(),
		KindResend:   resendKind(),
		KindPostgres: databaseKind(KindPostgres, "PostgreSQL database", 5432),
		KindMySQL:    databaseKind(KindMySQL, "MySQL database", 3306),
	}
	for name, k := range s.oauthKinds() {
		registry[name] = k
	}
	return registry
}

// Kinds lists the kinds this installation offers.
func (s *Service) Kinds() []KindInfo {
	out := make([]KindInfo, 0, len(s.registry))
	for _, name := range []string{KindSMTP, KindResend, KindGmail, KindGDrive, KindGDocs, KindGSheets, KindGCalendar, KindDropbox, KindOneDrive, KindPostgres, KindMySQL, KindMongo, KindSlack, KindNotion, KindMCP} {
		if k, ok := s.registry[name]; ok {
			out = append(out, k.info)
		}
	}
	return out
}

// --- small validation helpers shared by the kinds ---

func stringSetting(config map[string]any, name string, required bool, max int) (string, error) {
	value, present := config[name]
	if !present || value == nil || value == "" {
		if required {
			return "", fmt.Errorf("%s is required", name)
		}
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	text = strings.TrimSpace(text)
	if len(text) > max || strings.ContainsAny(text, "\r\n\x00") {
		return "", fmt.Errorf("%s is too long or contains a control character", name)
	}
	config[name] = text
	return text, nil
}

func intSetting(config map[string]any, name string, def, lo, hi int) (int, error) {
	value, present := config[name]
	if !present || value == nil {
		return def, nil
	}
	number, ok := value.(float64)
	if !ok {
		if i, isInt := value.(int); isInt {
			number, ok = float64(i), true
		}
	}
	if !ok || number != float64(int(number)) || int(number) < lo || int(number) > hi {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, lo, hi)
	}
	config[name] = int(number)
	return int(number), nil
}

func rejectUnknown(config map[string]any, secret map[string]string, kind KindInfo) error {
	known := map[string]bool{}
	secrets := map[string]bool{}
	for _, f := range kind.Fields {
		if f.Secret {
			secrets[f.Name] = true
		} else {
			known[f.Name] = true
		}
	}
	for name := range config {
		if !known[name] {
			if secrets[name] {
				return fmt.Errorf("%s is a credential: send it under secret, not config", name)
			}
			return fmt.Errorf("unknown setting %q", name)
		}
	}
	for name := range secret {
		if !secrets[name] {
			return fmt.Errorf("unknown credential %q", name)
		}
	}
	return nil
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, r := range label {
			if !(r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}
