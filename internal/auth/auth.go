// Package auth resolves a Drive service for any account, picking the right
// credential automatically: OAuth (refresh token stored on-disk in the VM) for
// personal @gmail accounts, Domain-Wide Delegation for Workspace accounts.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// Config configures credential resolution.
type Config struct {
	SAKeyPath        string        // service-account JSON for DWD (Workspace accounts)
	ClientSecretPath string        // OAuth client secret JSON file on disk
	TokenDir         string        // directory holding per-account OAuth token files
	HTTPTimeout      time.Duration // per-client HTTP timeout
	gmailDomains     map[string]bool
}

// Resolver builds Drive services on demand.
type Resolver struct {
	cfg        Config
	oauthConf  *oauth2.Config // lazily loaded from disk
	saKeyBytes []byte         // lazily loaded from disk
}

// NewResolver returns a Resolver.
func NewResolver(cfg Config) *Resolver {
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 60 * time.Second
	}
	if cfg.TokenDir == "" {
		cfg.TokenDir = "tokens"
	}
	cfg.gmailDomains = map[string]bool{"gmail.com": true, "googlemail.com": true}
	return &Resolver{cfg: cfg}
}

// IsGmail reports whether email is a personal Google account (no DWD possible).
func (r *Resolver) IsGmail(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	return r.cfg.gmailDomains[strings.ToLower(email[at+1:])]
}

// GetDriveService returns a Drive service authenticated as email.
func (r *Resolver) GetDriveService(ctx context.Context, email string) (*drive.Service, error) {
	if r.IsGmail(email) {
		return r.gmailService(ctx, email)
	}
	return r.dwdService(ctx, email)
}

// tokenPath returns the on-disk path for an account's OAuth token.
func (r *Resolver) tokenPath(email string) string {
	return filepath.Join(r.cfg.TokenDir, sanitize(email)+".json")
}

func (r *Resolver) gmailService(ctx context.Context, email string) (*drive.Service, error) {
	conf, err := r.oauthConfig()
	if err != nil {
		return nil, err
	}
	path := r.tokenPath(email)
	tok, err := loadToken(path)
	if err != nil {
		return nil, fmt.Errorf("oauth token for %q (run `drivemig auth %s`?): %w", email, email, err)
	}
	// Persist refreshed tokens back to disk so a rotated refresh_token survives.
	src := &persistingSource{base: conf.TokenSource(ctx, tok), path: path, last: tok}
	client := oauth2.NewClient(ctx, src)
	client.Timeout = r.cfg.HTTPTimeout
	return drive.NewService(ctx, option.WithHTTPClient(client))
}

func (r *Resolver) dwdService(ctx context.Context, email string) (*drive.Service, error) {
	if r.saKeyBytes == nil {
		b, err := os.ReadFile(r.cfg.SAKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read SA key %q: %w", r.cfg.SAKeyPath, err)
		}
		r.saKeyBytes = b
	}
	jwtConf, err := google.JWTConfigFromJSON(r.saKeyBytes, drive.DriveScope)
	if err != nil {
		return nil, fmt.Errorf("parse SA key: %w", err)
	}
	jwtConf.Subject = email // impersonate the Workspace user (DWD)
	ts := jwtConf.TokenSource(ctx)
	client := oauth2.NewClient(ctx, ts)
	client.Timeout = r.cfg.HTTPTimeout
	return drive.NewService(ctx, option.WithHTTPClient(client))
}

// oauthConfig loads and caches the OAuth client config from disk.
func (r *Resolver) oauthConfig() (*oauth2.Config, error) {
	if r.oauthConf != nil {
		return r.oauthConf, nil
	}
	raw, err := os.ReadFile(r.cfg.ClientSecretPath)
	if err != nil {
		return nil, fmt.Errorf("read oauth client secret %q: %w", r.cfg.ClientSecretPath, err)
	}
	conf, err := OAuthConfigFromJSON(raw)
	if err != nil {
		return nil, err
	}
	r.oauthConf = conf
	return conf, nil
}

// installedSecret is the shape of a downloaded OAuth client secret JSON. The
// "TVs and Limited Input devices" client type uses the "installed" key.
type installedSecret struct {
	Installed *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"installed"`
	Web *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"web"`
}

// OAuthConfigFromJSON builds an oauth2.Config (with the Google device endpoint)
// from a downloaded client secret JSON.
func OAuthConfigFromJSON(raw []byte) (*oauth2.Config, error) {
	var s installedSecret
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse client secret json: %w", err)
	}
	var id, secret string
	switch {
	case s.Installed != nil:
		id, secret = s.Installed.ClientID, s.Installed.ClientSecret
	case s.Web != nil:
		id, secret = s.Web.ClientID, s.Web.ClientSecret
	default:
		return nil, fmt.Errorf("client secret json has neither 'installed' nor 'web' block")
	}
	return &oauth2.Config{
		ClientID:     id,
		ClientSecret: secret,
		Scopes:       []string{drive.DriveScope},
		Endpoint: oauth2.Endpoint{
			AuthURL:       "https://accounts.google.com/o/oauth2/auth",
			TokenURL:      "https://oauth2.googleapis.com/token",
			DeviceAuthURL: "https://oauth2.googleapis.com/device/code",
		},
	}, nil
}

// persistingSource writes the token back to disk whenever it changes (e.g. on
// access-token refresh or refresh-token rotation).
type persistingSource struct {
	base oauth2.TokenSource
	path string
	last *oauth2.Token
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	t, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if p.last == nil || t.AccessToken != p.last.AccessToken || t.RefreshToken != p.last.RefreshToken {
		_ = saveToken(p.path, t)
		p.last = t
	}
	return t, nil
}

// loadToken reads an oauth2.Token from a JSON file.
func loadToken(path string) (*oauth2.Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parse token %q: %w", path, err)
	}
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("token %q has no refresh_token", path)
	}
	return &t, nil
}

// saveToken writes an oauth2.Token to a JSON file with 0600 perms.
func saveToken(path string, t *oauth2.Token) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// sanitize turns an email into a safe filename component.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
