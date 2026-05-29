package auth

import (
	"context"
	"fmt"
)

// DeviceLogin runs the OAuth 2.0 device flow for email: it prints a
// verification URL + user code, polls until the user consents, and stores the
// resulting token (incl. refresh_token) on disk under TokenDir.
func (r *Resolver) DeviceLogin(ctx context.Context, email string) error {
	conf, err := r.oauthConfig()
	if err != nil {
		return err
	}

	da, err := conf.DeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("start device auth: %w", err)
	}

	url := da.VerificationURIComplete
	if url == "" {
		url = da.VerificationURI
	}
	fmt.Printf("\n  Conta:  %s\n  Acesse: %s\n  Código: %s\n\n  Aguardando autorização...\n", email, url, da.UserCode)

	tok, err := conf.DeviceAccessToken(ctx, da)
	if err != nil {
		return fmt.Errorf("device token exchange: %w", err)
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("no refresh token returned (revoke prior grant and retry, ensuring access_type=offline)")
	}

	path := r.tokenPath(email)
	if err := saveToken(path, tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Printf("  OK: token salvo em %s (chmod 0600).\n", path)
	return nil
}
