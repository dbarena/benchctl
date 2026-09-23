package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dbarena/benchctl/internal/auth"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage benchctl authentication",
}

var authLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Log in with GitHub SSO (stores JWT in ~/.benchctl/credentials)",
	Args:  cobra.NoArgs,
	RunE:  runAuthLogin,
}

func init() {
	authCmd.AddCommand(authLoginCmd)
}

func runAuthLogin(_ *cobra.Command, _ []string) error {
	supabaseURL := supabaseAuthURL()
	anonKey := appCfg.Store.AnonKey
	if anonKey == "" {
		return fmt.Errorf("no anon key available: run 'benchctl config set store.anon_key <key>'")
	}

	creds, err := auth.LoginWithGitHub(supabaseURL, anonKey)
	if err != nil {
		return err
	}

	if err := auth.SaveCredentials(creds); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "Logged in. Credentials saved to ~/.benchctl/credentials.")
	return nil
}

// supabaseAuthURL returns the canonical Supabase project base URL for auth endpoints.
func supabaseAuthURL() string {
	raw := appCfg.Store.URL
	if raw == "" {
		return ""
	}
	// Reuse the same normalization as SupabaseStore.
	raw = strings.TrimSuffix(raw, "/")
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	if strings.HasPrefix(raw, "db.") && strings.Contains(raw, ".supabase.co") {
		ref := strings.TrimPrefix(raw, "db.")
		ref = strings.TrimSuffix(ref, ".supabase.co")
		return "https://" + ref + ".supabase.co"
	}
	return "https://" + raw
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current authentication status",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		creds, err := auth.LoadCredentials()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Not logged in (no credentials found).")
			return nil //nolint:nilerr
		}
		if !creds.Valid() {
			if creds.RefreshToken == "" {
				fmt.Fprintln(os.Stderr, "Credentials expired. Run 'benchctl auth login'.")
				return nil
			}
			fmt.Fprintln(os.Stderr, "Credentials expired; refreshing...")
			supabaseURL := supabaseAuthURL()
			anonKey := appCfg.Store.AnonKey
			if err := creds.Refresh(supabaseURL, anonKey); err != nil {
				fmt.Fprintf(os.Stderr, "Refresh failed: %v\nRun 'benchctl auth login'.\n", err)
				return nil
			}
			if err := auth.SaveCredentials(creds); err != nil {
				fmt.Fprintf(os.Stderr, "Refreshed but could not save credentials: %v\n", err)
			} else {
				fmt.Fprintln(os.Stderr, "Credentials refreshed.")
			}
		}
		store, err := openStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Logged in. Could not verify the token: %v\n", err)
			return nil
		}
		if _, err := store.List(); err != nil {
			fmt.Fprintf(os.Stderr, "Logged in. Store access failed: %v\n", err)
			return nil
		}
		fmt.Fprintln(os.Stderr, "Logged in. Store access verified.")
		return nil
	},
}

func init() {
	authCmd.AddCommand(authStatusCmd)
}
