package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestIsGmail(t *testing.T) {
	r := NewResolver(Config{})
	cases := map[string]bool{
		"a@gmail.com":      true,
		"a@googlemail.com": true,
		"a@wh1.com.br":     false,
		"noatsign":         false,
	}
	for email, want := range cases {
		if got := r.IsGmail(email); got != want {
			t.Fatalf("IsGmail(%q)=%v want %v", email, got, want)
		}
	}
}

func TestNewResolverDefaults(t *testing.T) {
	r := NewResolver(Config{})
	if r.cfg.HTTPTimeout != 60*time.Second {
		t.Fatalf("timeout=%v", r.cfg.HTTPTimeout)
	}
	if r.cfg.TokenDir != "tokens" {
		t.Fatalf("tokenDir=%q", r.cfg.TokenDir)
	}
}

func TestTokenPath(t *testing.T) {
	r := NewResolver(Config{TokenDir: "/t"})
	if got := r.tokenPath("A.B@Gmail.com"); got != filepath.Join("/t", "a-b-gmail-com.json") {
		t.Fatalf("tokenPath=%q", got)
	}
}

func TestOAuthConfigFromJSON(t *testing.T) {
	installed := []byte(`{"installed":{"client_id":"cid","client_secret":"sec"}}`)
	conf, err := OAuthConfigFromJSON(installed)
	if err != nil || conf.ClientID != "cid" || conf.ClientSecret != "sec" {
		t.Fatalf("installed: conf=%+v err=%v", conf, err)
	}
	if conf.Endpoint.DeviceAuthURL == "" {
		t.Fatal("expected device endpoint")
	}

	web := []byte(`{"web":{"client_id":"wid","client_secret":"wsec"}}`)
	conf, err = OAuthConfigFromJSON(web)
	if err != nil || conf.ClientID != "wid" {
		t.Fatalf("web: conf=%+v err=%v", conf, err)
	}

	if _, err := OAuthConfigFromJSON([]byte(`{}`)); err == nil {
		t.Fatal("expected error for empty client secret")
	}
	if _, err := OAuthConfigFromJSON([]byte(`not json`)); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("Foo.Bar@X_y-z"); got != "foo-bar-x_y-z" {
		t.Fatalf("sanitize=%q", got)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "tok.json")
	tok := &oauth2.Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now()}

	if err := saveToken(p, tok); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm=%v, want 0600", info.Mode().Perm())
	}

	got, err := loadToken(p)
	if err != nil || got.RefreshToken != "rt" {
		t.Fatalf("loadToken got=%+v err=%v", got, err)
	}
}

func TestLoadTokenNoRefresh(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.json")
	b, _ := json.Marshal(&oauth2.Token{AccessToken: "at"}) // no refresh token
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(p); err == nil {
		t.Fatal("expected error for missing refresh_token")
	}
}

func TestDwdServiceErrors(t *testing.T) {
	// missing SA key file
	r := NewResolver(Config{SAKeyPath: "/no/such/key.json"})
	if _, err := r.GetDriveService(context.Background(), "u@wh1.com.br"); err == nil {
		t.Fatal("expected error for missing SA key")
	}
	// malformed SA key
	dir := t.TempDir()
	bad := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(bad, []byte(`{"not":"a key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := NewResolver(Config{SAKeyPath: bad})
	if _, err := r2.GetDriveService(context.Background(), "u@wh1.com.br"); err == nil {
		t.Fatal("expected error for malformed SA key")
	}
}

func TestGmailServiceNoTokenErrors(t *testing.T) {
	dir := t.TempDir()
	cs := filepath.Join(dir, "client_secret.json")
	if err := os.WriteFile(cs, []byte(`{"installed":{"client_id":"c","client_secret":"s"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(Config{ClientSecretPath: cs, TokenDir: dir})
	// no token file for this account -> error
	if _, err := r.GetDriveService(context.Background(), "missing@gmail.com"); err == nil {
		t.Fatal("expected error for missing token")
	}
}
