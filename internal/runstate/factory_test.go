package runstate

import (
	"testing"

	"github.com/dbarena/benchctl/internal/config"
)

func TestNewStore_LocalModeIgnoresURL(t *testing.T) {
	// LocalStore opens (and creates) a database under $HOME; point it at a
	// temp dir so the test never touches the developer's real state.
	t.Setenv("HOME", t.TempDir())

	cfg := &config.Config{
		Store: config.StoreConfig{
			Mode: config.ModeLocal,
			URL:  "https://example.supabase.co",
		},
	}

	store, err := NewStore(cfg, nil)
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	if _, ok := store.(*LocalStore); !ok {
		t.Errorf("NewStore() = %T, want *LocalStore", store)
	}
}

func TestNewStore_RemoteModeReturnsSupabaseStore(t *testing.T) {
	// Validate rejects remote mode when BENCHCTL_STORE_SERVICE_ROLE_KEY is
	// present-but-empty in the ambient environment; set it explicitly so
	// this test doesn't depend on what the developer's/CI's shell exports.
	t.Setenv("BENCHCTL_STORE_SERVICE_ROLE_KEY", "ambient-placeholder")

	cfg := &config.Config{
		Store: config.StoreConfig{
			Mode:           config.ModeRemote,
			URL:            "https://example.supabase.co",
			AnonKey:        "anon-key",
			ServiceRoleKey: "service-role-key",
		},
	}

	store, err := NewStore(cfg, nil)
	if err != nil {
		t.Fatalf("NewStore() error = %v, want nil", err)
	}
	if _, ok := store.(*SupabaseStore); !ok {
		t.Errorf("NewStore() = %T, want *SupabaseStore", store)
	}
}

func TestNewStore_RemoteModeMissingURLSurfacesValidateError(t *testing.T) {
	cfg := &config.Config{
		Store: config.StoreConfig{
			Mode:    config.ModeRemote,
			AnonKey: "anon-key",
		},
	}

	_, err := NewStore(cfg, nil)
	if err == nil {
		t.Fatal("NewStore() error = nil, want Validate's error")
	}
}
