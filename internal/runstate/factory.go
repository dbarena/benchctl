package runstate

import (
	"fmt"

	"github.com/dbarena/benchctl/internal/auth"
	"github.com/dbarena/benchctl/internal/config"
)

// NewStore returns the Store implementation selected by cfg.Store.Mode.
//
// cfg.Validate() is checked first, so a remote mode with a missing URL or
// anon key is rejected here rather than surfacing later as a confusing HTTP
// error. ModeLocal returns a LocalStore backed by ~/.benchctl/state.db.
// ModeRemote returns a SupabaseStore for cfg.Store.URL, authenticated with
// cfg.Store.AnonKey and the token resolved by resolveAuthToken.
//
// dial lets a LocalStore read a run back from the driver instance executing it.
// Pass nil to get a store that only ever reads what is on this machine, which
// is what the driver-side process itself wants, since there the local record is
// the authoritative one.
func NewStore(cfg *config.Config, dial Dialer) (Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch cfg.Store.Mode {
	case config.ModeLocal:
		refreshInterval, err := cfg.StoreRefreshInterval()
		if err != nil {
			return nil, err
		}
		store, err := NewLocalStore()
		if err != nil {
			return nil, err
		}
		store.dial = dial
		store.refreshInterval = refreshInterval
		return store, nil
	case config.ModeRemote:
		authToken, creds, err := resolveAuthToken(cfg)
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		store, err := NewSupabaseStore(cfg.Store.URL, cfg.Store.AnonKey, authToken)
		if err != nil {
			return nil, err
		}
		store.creds = creds
		return store, nil
	default:
		return nil, fmt.Errorf("runstate: unknown store.mode %q", cfg.Store.Mode)
	}
}

// resolveAuthToken returns the Bearer credential for the Supabase store, plus
// the Credentials if the user-JWT path was taken (enabling token refresh).
// Resolution order:
//  1. cfg.Store.ServiceRoleKey (CI and superuser path; takes precedence over
//     any locally cached credentials)
//  2. User JWT from ~/.benchctl/credentials (interactive users on laptop);
//     if expired and a refresh token is present, a refresh is attempted first.
//  3. cfg.Store.AuthToken (set on remote drivers)
func resolveAuthToken(cfg *config.Config) (string, *auth.Credentials, error) {
	if cfg.Store.ServiceRoleKey != "" {
		return cfg.Store.ServiceRoleKey, nil, nil
	}
	if creds, err := auth.LoadCredentials(); err == nil {
		if !creds.Valid() && creds.RefreshToken != "" {
			if err := creds.Refresh(cfg.Store.URL, cfg.Store.AnonKey); err == nil {
				_ = auth.SaveCredentials(creds)
			}
		}
		if creds.Valid() {
			return creds.AccessToken, creds, nil
		}
	}
	if cfg.Store.AuthToken != "" {
		return cfg.Store.AuthToken, nil, nil
	}
	return "", nil, fmt.Errorf("no auth token available: set BENCHCTL_STORE_SERVICE_ROLE_KEY or run 'benchctl auth login'")
}
