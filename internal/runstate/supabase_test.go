package runstate

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestStateToRowRoundTrip_CurrentStepFields guards against the exact bug that
// caused current_fixture/current_iteration/current_step/step_started_at to be
// silently dropped by SupabaseStore: supabaseRow is a hand-maintained
// projection of State for the PostgREST wire format, so a field added to
// State only round-trips through LocalStore (generic JSON) until
// stateToRow/rowToState are updated to carry it too.
func TestStateToRowRoundTrip_CurrentStepFields(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Second)
	s := &State{
		RunID:            "run-a",
		ScenarioName:     "scenario",
		Phases:           map[Phase]Status{PhaseProvision: StatusCompleted},
		CurrentFixture:   map[string]string{"clients": "8"},
		CurrentIteration: 2,
		CurrentStep:      "benchmark",
		StepStartedAt:    &started,
	}

	row, err := stateToRow(s)
	if err != nil {
		t.Fatalf("stateToRow: %v", err)
	}
	got, err := rowToState(row)
	if err != nil {
		t.Fatalf("rowToState: %v", err)
	}

	if got.CurrentFixture["clients"] != "8" {
		t.Errorf("CurrentFixture[clients] = %q, want 8", got.CurrentFixture["clients"])
	}
	if got.CurrentIteration != 2 {
		t.Errorf("CurrentIteration = %d, want 2", got.CurrentIteration)
	}
	if got.CurrentStep != "benchmark" {
		t.Errorf("CurrentStep = %q, want benchmark", got.CurrentStep)
	}
	if got.StepStartedAt == nil || !got.StepStartedAt.Equal(started) {
		t.Errorf("StepStartedAt = %v, want %v", got.StepStartedAt, started)
	}
}

func TestIsNewFormatKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"sb_secret_B2xaRorKNTy3twgitoxF0Q_qhyYJaTr", true},
		{"sb_publishable_abc123", true},
		{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig", false},
		{"", false},
		{"service_role", false},
	}
	for _, tc := range cases {
		if got := isNewFormatKey(tc.key); got != tc.want {
			t.Errorf("isNewFormatKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestDoRequestHeaders verifies that new-format keys (sb_secret_*) are sent as
// the apikey header only, while legacy JWT tokens use Authorization: Bearer +
// apikey: anonKey. This is the regression that caused PGRST301 "Expected 3
// parts in JWT; got 1" when a service role key in the new format was used.
func TestDoRequestHeaders(t *testing.T) {
	const anonKey = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.anon.sig"

	tests := []struct {
		name           string
		authToken      string
		wantAPIKey     string
		wantAuthBearer string // empty means header must be absent
	}{
		{
			name:           "new-format secret key goes to apikey only",
			authToken:      "sb_secret_B2xaRorKNTy3twgitoxF0Q_qhyYJaTr",
			wantAPIKey:     "sb_secret_B2xaRorKNTy3twgitoxF0Q_qhyYJaTr",
			wantAuthBearer: "",
		},
		{
			name:           "new-format publishable key goes to apikey only",
			authToken:      "sb_publishable_abc123",
			wantAPIKey:     "sb_publishable_abc123",
			wantAuthBearer: "",
		},
		{
			name:           "legacy JWT uses Authorization Bearer and anonKey as apikey",
			authToken:      "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.user.sig",
			wantAPIKey:     anonKey,
			wantAuthBearer: "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.user.sig",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotAPIKey, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAPIKey = r.Header.Get("apikey")
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte("[]"))
			}))
			defer srv.Close()

			// Construct directly to avoid normalizeSupabaseURL rewriting the
			// plain-HTTP test server URL to https://.
			store := &SupabaseStore{
				restURL:   srv.URL + "/rest/v1",
				anonKey:   anonKey,
				authToken: tc.authToken,
				client:    &http.Client{},
			}

			// List triggers a GET /runs request, enough to exercise doRequest.
			store.List()

			if gotAPIKey != tc.wantAPIKey {
				t.Errorf("apikey header = %q, want %q", gotAPIKey, tc.wantAPIKey)
			}
			if gotAuth != tc.wantAuthBearer {
				t.Errorf("Authorization header = %q, want %q", gotAuth, tc.wantAuthBearer)
			}
		})
	}
}
