package connections

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
)

const (
	oauthStateTTL = 10 * time.Minute
	// refreshWindow: a token this close to expiry is refreshed before use.
	refreshWindow = 60 * time.Second
	tokenTimeout  = 20 * time.Second
)

// oauthProvider describes how to sign in to one provider and which scopes each
// of its kinds needs.
type oauthProvider struct {
	name      string
	app       OAuthApp
	authKey   string
	tokenKey  string
	extraAuth url.Values
	scopes    map[string][]string // kind -> scopes
}

func (s *Service) oauthProviders() map[string]oauthProvider {
	out := map[string]oauthProvider{}
	if s.opts.Google.ClientID != "" {
		google := oauthProvider{
			name: "Google", app: s.opts.Google, authKey: "google_auth", tokenKey: "google_token",
			// offline + consent make Google return a refresh token every time.
			extraAuth: url.Values{"access_type": {"offline"}, "prompt": {"consent"}},
			scopes: map[string][]string{
				KindGmail:  {"https://www.googleapis.com/auth/gmail.send", "openid", "email"},
				KindGDrive: {"https://www.googleapis.com/auth/drive.file", "openid", "email"},
			},
		}
		out[KindGmail], out[KindGDrive] = google, google
	}
	if s.opts.Dropbox.ClientID != "" {
		out[KindDropbox] = oauthProvider{
			name: "Dropbox", app: s.opts.Dropbox, authKey: "dropbox_auth", tokenKey: "dropbox_token",
			extraAuth: url.Values{"token_access_type": {"offline"}},
			scopes:    map[string][]string{KindDropbox: {"files.content.write", "account_info.read"}},
		}
	}
	if s.opts.Microsoft.ClientID != "" {
		out[KindOneDrive] = oauthProvider{
			name: "Microsoft", app: s.opts.Microsoft, authKey: "microsoft_auth", tokenKey: "microsoft_token",
			scopes: map[string][]string{KindOneDrive: {"offline_access", "Files.ReadWrite", "User.Read"}},
		}
	}
	return out
}

// oauthKinds registers the OAuth kinds whose provider the operator configured.
func (s *Service) oauthKinds() map[string]kind {
	titles := map[string]string{KindGmail: "Gmail", KindGDrive: "Google Drive", KindDropbox: "Dropbox", KindOneDrive: "OneDrive"}
	out := map[string]kind{}
	for name := range s.oauthProviders() {
		name := name
		out[name] = kind{
			info:  KindInfo{Kind: name, Title: titles[name], OAuth: true},
			check: func(map[string]any, map[string]string) error { return nil },
			test: func(ctx context.Context, s *Service, _ map[string]any, secret map[string]string) error {
				_, err := s.accountOf(ctx, name, secret["access_token"])
				return err
			},
		}
	}
	return out
}

func (s *Service) redirectURI() string {
	return strings.TrimRight(s.opts.PublicURL, "/") + "/v1/oauth/callback"
}

func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func stateHash(state string) string {
	sum := sha256.Sum256([]byte(state))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// StartOAuth begins connecting an OAuth service and returns the provider URL to
// send the user to. Naming an existing connection of the same kind reconnects it
// (new tokens, same name), for when access was revoked.
func (s *Service) StartOAuth(ctx context.Context, orgID, userID pgtype.UUID, kindName, name string) (string, error) {
	provider, ok := s.oauthProviders()[kindName]
	if !ok {
		return "", exception.BadRequest(fmt.Sprintf("kind %q is not an OAuth integration enabled on this installation", kindName))
	}
	if s.opts.PublicURL == "" {
		return "", exception.New(http.StatusNotImplemented, "oauth_not_configured", "OAuth is not configured", "set PUBLIC_URL so providers can redirect back")
	}
	if s.keyring == nil {
		return "", exception.New(http.StatusNotImplemented, "vault_not_configured", "Vault is not configured", "set VAULT_MASTER_KEY to store connection credentials")
	}
	name = strings.TrimSpace(name)
	if !namePattern.MatchString(name) {
		return "", exception.BadRequest("name must be lowercase letters, digits, - or _ (at most 63 characters)")
	}
	if existing, err := s.q.GetConnection(ctx, gen.GetConnectionParams{OrgID: orgID, Name: name}); err == nil && existing.Kind != kindName {
		return "", exception.Conflict("name_taken", fmt.Sprintf("a %s connection named %q already exists", existing.Kind, name))
	}
	state, err := randomToken(32)
	if err != nil {
		return "", err
	}
	verifier, err := randomToken(32)
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	if err := s.q.CreateOAuthState(ctx, gen.CreateOAuthStateParams{
		StateHash: stateHash(state), OrgID: orgID, UserID: userID, Kind: kindName, Name: name, CodeVerifier: verifier,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(oauthStateTTL), Valid: true},
	}); err != nil {
		return "", fmt.Errorf("store oauth state: %w", err)
	}
	_, _ = s.q.DeleteExpiredOAuthStates(ctx)

	params := url.Values{
		"response_type": {"code"}, "client_id": {provider.app.ClientID}, "redirect_uri": {s.redirectURI()},
		"scope": {strings.Join(provider.scopes[kindName], " ")}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
	}
	for key, values := range provider.extraAuth {
		params[key] = values
	}
	return s.endpoint(provider.authKey) + "?" + params.Encode(), nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// token calls a provider's token endpoint.
func (s *Service) token(ctx context.Context, provider oauthProvider, form url.Values) (tokenResponse, error) {
	form.Set("client_id", provider.app.ClientID)
	form.Set("client_secret", provider.app.ClientSecret)
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint(provider.tokenKey), strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := s.policy.Client(tokenTimeout).Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("%s could not be reached", provider.name)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out tokenResponse
	_ = json.Unmarshal(data, &out)
	if res.StatusCode != http.StatusOK || out.Error != "" || out.AccessToken == "" {
		switch out.Error {
		case "invalid_grant", "interaction_required", "consent_required":
			return tokenResponse{}, executor.ErrConnectionNeedsAuth
		}
		detail := out.Error
		if out.Description != "" {
			detail += ": " + out.Description
		}
		return tokenResponse{}, fmt.Errorf("%s refused the token request (HTTP %d) %s", provider.name, res.StatusCode, strings.TrimSpace(detail))
	}
	return out, nil
}

func expiry(resp tokenResponse) pgtype.Timestamptz {
	seconds := resp.ExpiresIn
	if seconds <= 0 {
		seconds = 3600
	}
	return pgtype.Timestamptz{Time: time.Now().Add(time.Duration(seconds) * time.Second), Valid: true}
}

// accountOf asks the provider who the token belongs to; it doubles as the
// connection test.
func (s *Service) accountOf(ctx context.Context, kindName, accessToken string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	var (
		method, target string
		pick           func(map[string]any) string
	)
	switch kindName {
	case KindGmail, KindGDrive:
		method, target = http.MethodGet, s.endpoint("google_userinfo")
		pick = func(m map[string]any) string { v, _ := m["email"].(string); return v }
	case KindDropbox:
		method, target = http.MethodPost, s.endpoint("dropbox_api")+"/2/users/get_current_account"
		pick = func(m map[string]any) string { v, _ := m["email"].(string); return v }
	case KindOneDrive:
		method, target = http.MethodGet, s.endpoint("graph_api")+"/v1.0/me?$select=userPrincipalName,mail"
		pick = func(m map[string]any) string {
			if v, _ := m["mail"].(string); v != "" {
				return v
			}
			v, _ := m["userPrincipalName"].(string)
			return v
		}
	default:
		return "", fmt.Errorf("kind %q has no account lookup", kindName)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	res, err := s.policy.Client(tokenTimeout).Do(req)
	if err != nil {
		return "", errors.New("the service could not be reached")
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("the service rejected the access token (HTTP %d)", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the service answered HTTP %d", res.StatusCode)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return "", errors.New("the service sent an unreadable account response")
	}
	account := pick(decoded)
	if account == "" {
		account = "(unknown account)"
	}
	return account, nil
}

// CompleteOAuth finishes a sign-in: it consumes the state, exchanges the code
// for tokens, looks up the account and stores the connection. It returns the
// connection's name. The state is single-use, so a replayed callback fails.
func (s *Service) CompleteOAuth(ctx context.Context, state, code, providerError string) (string, error) {
	row, err := s.q.ConsumeOAuthState(ctx, stateHash(state))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", exception.BadRequest("this sign-in link is invalid, expired or already used; start again")
	}
	if err != nil {
		return "", fmt.Errorf("consume oauth state: %w", err)
	}
	if providerError != "" {
		return row.Name, exception.BadRequest("the provider reported: " + providerError)
	}
	if code == "" {
		return row.Name, exception.BadRequest("the provider sent no authorization code")
	}
	provider, ok := s.oauthProviders()[row.Kind]
	if !ok {
		return row.Name, exception.BadRequest("this integration is no longer enabled")
	}
	tokens, err := s.token(ctx, provider, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.redirectURI()}, "code_verifier": {row.CodeVerifier},
	})
	if err != nil {
		return row.Name, exception.New(http.StatusBadGateway, "oauth_exchange_failed", "Sign-in failed", err.Error())
	}
	if tokens.RefreshToken == "" {
		return row.Name, exception.New(http.StatusBadGateway, "oauth_no_refresh_token", "Sign-in failed",
			provider.name+" did not return a refresh token; remove this app's access in your account settings and connect again")
	}
	account, err := s.accountOf(ctx, row.Kind, tokens.AccessToken)
	if err != nil {
		return row.Name, exception.New(http.StatusBadGateway, "oauth_exchange_failed", "Sign-in failed", err.Error())
	}
	secret := map[string]string{"access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken, "token_type": tokens.TokenType, "scope": tokens.Scope}
	ciphertext, nonce, version, err := s.seal(row.OrgID, row.Name, secret)
	if err != nil {
		return row.Name, err
	}
	expires := expiry(tokens)
	accountConfig, _ := json.Marshal(map[string]any{"account": account})
	userName := pgtype.Text{}
	if row.UserID.Valid {
		userName = pgtype.Text{String: "oauth", Valid: true}
	}
	_, err = s.q.CreateConnection(ctx, gen.CreateConnectionParams{
		ID: uid.New(), OrgID: row.OrgID, Name: row.Name, Kind: row.Kind, Config: accountConfig,
		SecretCiphertext: ciphertext, SecretNonce: nonce, SecretKeyVersion: pgtype.Int4{Int32: int32(version), Valid: true},
		Account: pgtype.Text{String: account, Valid: true}, TokenExpiresAt: expires, CreatedBy: userName,
	})
	if errors.Is(err, pgx.ErrNoRows) { // the name exists: reconnecting replaces its tokens
		_, err = s.q.UpdateConnection(ctx, gen.UpdateConnectionParams{
			OrgID: row.OrgID, Name: row.Name, Config: accountConfig,
			SecretCiphertext: ciphertext, SecretNonce: nonce, SecretKeyVersion: pgtype.Int4{Int32: int32(version), Valid: true},
			Account: pgtype.Text{String: account, Valid: true}, TokenExpiresAt: expires,
		})
	}
	if err != nil {
		return row.Name, fmt.Errorf("store the connection: %w", err)
	}
	return row.Name, nil
}

// refreshToken exchanges the refresh token for a new access token and stores it.
// If another process refreshed first, theirs is used instead.
func (s *Service) refreshToken(ctx context.Context, orgID pgtype.UUID, row gen.Connection, secret map[string]string) (map[string]string, error) {
	lock, _ := s.refresh.LoadOrStore(uid.String(row.ID), &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Someone in this process may have refreshed while we waited.
	if fresh, err := s.q.GetConnection(ctx, gen.GetConnectionParams{OrgID: orgID, Name: row.Name}); err == nil &&
		fresh.TokenExpiresAt.Valid && time.Until(fresh.TokenExpiresAt.Time) >= refreshWindow {
		return s.open(orgID, fresh)
	}
	provider, ok := s.oauthProviders()[row.Kind]
	if !ok {
		return nil, fmt.Errorf("%s: %w", row.Kind, executor.ErrConnectionNeedsAuth)
	}
	if secret["refresh_token"] == "" {
		return nil, executor.ErrConnectionNeedsAuth
	}
	tokens, err := s.token(ctx, provider, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {secret["refresh_token"]}})
	if err != nil {
		return nil, err
	}
	updated := map[string]string{
		"access_token": tokens.AccessToken, "refresh_token": secret["refresh_token"],
		"token_type": tokens.TokenType, "scope": coalesce(tokens.Scope, secret["scope"]),
	}
	if tokens.RefreshToken != "" { // Microsoft rotates refresh tokens
		updated["refresh_token"] = tokens.RefreshToken
	}
	ciphertext, nonce, version, err := s.seal(orgID, row.Name, updated)
	if err != nil {
		return nil, err
	}
	changed, err := s.q.StoreConnectionSecret(ctx, gen.StoreConnectionSecretParams{
		ID: row.ID, Ciphertext: ciphertext, Nonce: nonce, KeyVersion: pgtype.Int4{Int32: int32(version), Valid: true},
		TokenExpiresAt: expiry(tokens), OldNonce: row.SecretNonce,
	})
	if err != nil {
		return nil, fmt.Errorf("store the refreshed token: %w", err)
	}
	if changed == 0 { // another process won the race: use what it stored
		current, err := s.q.GetConnection(ctx, gen.GetConnectionParams{OrgID: orgID, Name: row.Name})
		if err != nil {
			return nil, fmt.Errorf("reload the connection: %w", err)
		}
		return s.open(orgID, current)
	}
	return updated, nil
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
