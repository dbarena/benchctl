// Package auth manages benchctl user credentials (JWT from GitHub SSO via Supabase).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Credentials holds the tokens returned by a successful Supabase auth flow.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Valid reports whether the credentials are present and not expired.
func (c *Credentials) Valid() bool {
	return c.AccessToken != "" && time.Now().Before(c.ExpiresAt)
}

// credentialsPath returns ~/.benchctl/credentials.
func credentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("auth: cannot determine home dir: %w", err)
	}
	return filepath.Join(home, ".benchctl", "credentials"), nil
}

// LoadCredentials reads credentials from ~/.benchctl/credentials.
// Returns an error if the file does not exist or cannot be parsed.
func LoadCredentials() (*Credentials, error) {
	p, err := credentialsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("auth: read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("auth: parse credentials: %w", err)
	}
	return &c, nil
}

// SaveCredentials writes credentials to ~/.benchctl/credentials (0600).
func SaveCredentials(c *Credentials) error {
	p, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("auth: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: marshal credentials: %w", err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return fmt.Errorf("auth: write credentials: %w", err)
	}
	return nil
}

// LoginWithGitHub performs a PKCE OAuth flow against the Supabase project at
// supabaseURL, opening a browser for the GitHub provider and waiting for the
// local callback. anonKey is the Supabase project's public anon key, required
// by the API gateway on the token exchange request. The returned Credentials
// are ready to be saved.
func LoginWithGitHub(supabaseURL, anonKey string) (*Credentials, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("auth: listen for callback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	verifier, challenge, err := pkce()
	if err != nil {
		return nil, fmt.Errorf("auth: pkce: %w", err)
	}

	authURL := strings.TrimSuffix(supabaseURL, "/") + "/auth/v1/authorize" +
		"?provider=github" +
		"&code_challenge=" + challenge +
		"&code_challenge_method=S256" +
		"&redirect_to=" + redirectURI

	fmt.Printf("Opening browser for GitHub login...\nIf it does not open automatically, visit:\n\n  %s\n\n", authURL)
	openBrowser(authURL)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("auth: callback missing code parameter")
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Login successful. You can close this tab.")
		codeCh <- code
	})
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("auth: timed out waiting for browser callback")
	}
	srv.Close()

	return exchangeCode(supabaseURL, anonKey, code, verifier)
}

// exchangeCode calls the Supabase PKCE token endpoint.
// anonKey is the project's public anon key, required by the Supabase API gateway.
func exchangeCode(supabaseURL, anonKey, code, verifier string) (*Credentials, error) {
	body := fmt.Sprintf(`{"auth_code":%q,"code_verifier":%q}`, code, verifier)
	tokenURL := strings.TrimSuffix(supabaseURL, "/") + "/auth/v1/token?grant_type=pkce"

	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("auth: token exchange: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", anonKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: token exchange: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("auth: token exchange: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("auth: token exchange: decode: %w", err)
	}

	return &Credentials{
		AccessToken:  payload.AccessToken,
		TokenType:    payload.TokenType,
		RefreshToken: payload.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}

// Refresh calls the Supabase token endpoint with the stored refresh token and
// updates the receiver in-place with the new token pair.
func (c *Credentials) Refresh(storeURL, anonKey string) error {
	if c.RefreshToken == "" {
		return fmt.Errorf("auth: no refresh token available")
	}
	body := fmt.Sprintf(`{"refresh_token":%q}`, c.RefreshToken)
	tokenURL := strings.TrimSuffix(storeURL, "/") + "/auth/v1/token?grant_type=refresh_token"

	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("auth: refresh token: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", anonKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("auth: refresh token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("auth: refresh token: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("auth: refresh token: decode: %w", err)
	}

	c.AccessToken = payload.AccessToken
	c.TokenType = payload.TokenType
	c.RefreshToken = payload.RefreshToken
	c.ExpiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	return nil
}

// pkce generates a random PKCE code_verifier and its S256 code_challenge.
func pkce() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// openBrowser attempts to open the URL in the default system browser.
func openBrowser(url string) {
	// exec.Command is intentionally fire-and-forget; ignore errors.
	_ = openBrowserCmd(url)
}
