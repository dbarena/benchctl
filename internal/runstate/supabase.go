package runstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dbarena/benchctl/internal/auth"
)

// SupabaseStore persists run state in a Supabase (PostgREST) runs table.
type SupabaseStore struct {
	baseURL   string            // https://{project-ref}.supabase.co
	restURL   string            // https://{project-ref}.supabase.co/rest/v1
	anonKey   string            // project anon key; identifies the project to the API gateway
	authToken string            // Bearer token: user JWT or service role key
	creds     *auth.Credentials // non-nil for user-JWT path; enables automatic token refresh
	mu        sync.Mutex        // guards authToken and creds during refresh
	client    *http.Client
}

// NewSupabaseStore returns a SupabaseStore. rawURL accepts either the Supabase
// project URL (https://{ref}.supabase.co) or the DB host (db.{ref}.supabase.co);
// the DB-host form is automatically converted to the PostgREST base URL.
// anonKey is the project's public anon key (sent as the apikey header).
// authToken is the Bearer credential, a user JWT or service role key.
func NewSupabaseStore(rawURL, anonKey, authToken string) (*SupabaseStore, error) {
	if rawURL == "" {
		return nil, fmt.Errorf("runstate: Supabase URL is empty")
	}
	if anonKey == "" {
		return nil, fmt.Errorf("runstate: Supabase anon key is empty")
	}
	if authToken == "" {
		return nil, fmt.Errorf("runstate: Supabase auth token is empty")
	}
	base := normalizeSupabaseURL(rawURL)
	return &SupabaseStore{
		baseURL:   base,
		restURL:   base + "/rest/v1",
		anonKey:   anonKey,
		authToken: authToken,
		client:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// normalizeSupabaseURL converts a DB host or bare project URL into the HTTPS
// project base URL (without a trailing path).
//
//	db.{ref}.supabase.co          → https://{ref}.supabase.co
//	https://{ref}.supabase.co     → unchanged
//	{ref}.supabase.co             → https://{ref}.supabase.co
func normalizeSupabaseURL(raw string) string {
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

// supabaseRow is the PostgREST wire format for the runs table.
// JSONB columns are held as raw JSON so we control serialisation of the
// nested Go types (map[Phase]Status, map[string]any, etc.).
type supabaseRow struct {
	RunID          string          `json:"run_id"`
	ScenarioName   string          `json:"scenario_name"`
	ScenarioPath   string          `json:"scenario_path"`
	TargetProvider string          `json:"target_provider"`
	DriverProvider string          `json:"driver_provider"`
	StartedAt      time.Time       `json:"started_at"`
	Inputs         json.RawMessage `json:"inputs"`
	Phases         json.RawMessage `json:"phases"`
	TargetOutputs  json.RawMessage `json:"target_outputs,omitempty"`
	DriverOutputs  json.RawMessage `json:"driver_outputs,omitempty"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	TerminatedAt   *time.Time      `json:"terminated_at,omitempty"`
	Error          *string         `json:"error,omitempty"`
	CreatedBy      string          `json:"created_by,omitempty"`
	CreatedByEmail string          `json:"created_by_email,omitempty"`
	LastHeartbeat  *time.Time      `json:"last_heartbeat,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	// No omitempty: Update must be able to explicitly clear this to "" (e.g.
	// teardown dropping the blob once it's no longer needed) rather than
	// silently leaving the previous value in place because the zero value
	// was omitted from the PATCH body.
	TofuState        string          `json:"tofu_state"`
	CurrentFixture   json.RawMessage `json:"current_fixture,omitempty"`
	CurrentIteration int             `json:"current_iteration"`
	CurrentStep      string          `json:"current_step,omitempty"`
	StepStartedAt    *time.Time      `json:"step_started_at,omitempty"`
}

func stateToRow(s *State) (*supabaseRow, error) {
	inputs, err := json.Marshal(s.Inputs)
	if err != nil {
		return nil, fmt.Errorf("marshal inputs: %w", err)
	}
	phases, err := json.Marshal(s.Phases)
	if err != nil {
		return nil, fmt.Errorf("marshal phases: %w", err)
	}
	row := &supabaseRow{
		RunID:          s.RunID,
		ScenarioName:   s.ScenarioName,
		ScenarioPath:   s.ScenarioPath,
		TargetProvider: s.TargetProvider,
		DriverProvider: s.DriverProvider,
		StartedAt:      s.StartedAt,
		Inputs:         inputs,
		Phases:         phases,
		CompletedAt:    s.CompletedAt,
		TerminatedAt:   s.TerminatedAt,
		CreatedBy:      s.CreatedBy,
		CreatedByEmail: s.CreatedByEmail,
		LastHeartbeat:  s.LastHeartbeat,
	}
	if s.Error != "" {
		e := s.Error
		row.Error = &e
	}
	if s.TargetOutputs != nil {
		to, err := json.Marshal(s.TargetOutputs)
		if err != nil {
			return nil, fmt.Errorf("marshal target_outputs: %w", err)
		}
		row.TargetOutputs = to
	}
	if s.DriverOutputs != nil {
		do, err := json.Marshal(s.DriverOutputs)
		if err != nil {
			return nil, fmt.Errorf("marshal driver_outputs: %w", err)
		}
		row.DriverOutputs = do
	}
	if len(s.Metadata) > 0 {
		md, err := json.Marshal(s.Metadata)
		if err != nil {
			return nil, fmt.Errorf("marshal metadata: %w", err)
		}
		row.Metadata = md
	}
	row.TofuState = s.TofuState
	if s.CurrentFixture != nil {
		cf, err := json.Marshal(s.CurrentFixture)
		if err != nil {
			return nil, fmt.Errorf("marshal current_fixture: %w", err)
		}
		row.CurrentFixture = cf
	}
	row.CurrentIteration = s.CurrentIteration
	row.CurrentStep = s.CurrentStep
	row.StepStartedAt = s.StepStartedAt
	return row, nil
}

func rowToState(row *supabaseRow) (*State, error) {
	s := &State{
		RunID:          row.RunID,
		ScenarioName:   row.ScenarioName,
		ScenarioPath:   row.ScenarioPath,
		TargetProvider: row.TargetProvider,
		DriverProvider: row.DriverProvider,
		StartedAt:      row.StartedAt,
		CompletedAt:    row.CompletedAt,
		TerminatedAt:   row.TerminatedAt,
		CreatedBy:      row.CreatedBy,
		CreatedByEmail: row.CreatedByEmail,
		LastHeartbeat:  row.LastHeartbeat,
	}
	if row.Error != nil {
		s.Error = *row.Error
	}
	if err := json.Unmarshal(row.Inputs, &s.Inputs); err != nil {
		return nil, fmt.Errorf("unmarshal inputs: %w", err)
	}
	if err := json.Unmarshal(row.Phases, &s.Phases); err != nil {
		return nil, fmt.Errorf("unmarshal phases: %w", err)
	}
	if len(row.TargetOutputs) > 0 && string(row.TargetOutputs) != "null" {
		if err := json.Unmarshal(row.TargetOutputs, &s.TargetOutputs); err != nil {
			return nil, fmt.Errorf("unmarshal target_outputs: %w", err)
		}
	}
	if len(row.DriverOutputs) > 0 && string(row.DriverOutputs) != "null" {
		if err := json.Unmarshal(row.DriverOutputs, &s.DriverOutputs); err != nil {
			return nil, fmt.Errorf("unmarshal driver_outputs: %w", err)
		}
	}
	if len(row.Metadata) > 0 && string(row.Metadata) != "null" && string(row.Metadata) != "{}" {
		if err := json.Unmarshal(row.Metadata, &s.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	s.TofuState = row.TofuState
	if len(row.CurrentFixture) > 0 && string(row.CurrentFixture) != "null" {
		if err := json.Unmarshal(row.CurrentFixture, &s.CurrentFixture); err != nil {
			return nil, fmt.Errorf("unmarshal current_fixture: %w", err)
		}
	}
	s.CurrentIteration = row.CurrentIteration
	s.CurrentStep = row.CurrentStep
	s.StepStartedAt = row.StepStartedAt
	return s, nil
}

// Create inserts a new row. Returns an error if run_id already exists.
func (s *SupabaseStore) Create(state *State) error {
	row, err := stateToRow(state)
	if err != nil {
		return fmt.Errorf("runstate: supabase create: %w", err)
	}
	resp, err := s.doRequest("POST", s.restURL+"/runs", row)
	if err != nil {
		return fmt.Errorf("runstate: supabase create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runstate: supabase create: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// Update loads the current state for runID, applies fn, and PATCHes the result.
// This is a read-modify-write with no server-side locking; concurrent callers
// within the same process must serialize their own calls (engine.Runner does
// this with a mutex) to avoid lost updates.
func (s *SupabaseStore) Update(runID string, fn func(*State)) error {
	state, err := s.Load(runID)
	if err != nil {
		return err
	}
	fn(state)
	row, err := stateToRow(state)
	if err != nil {
		return fmt.Errorf("runstate: supabase update: %w", err)
	}
	u := s.restURL + "/runs?run_id=eq." + url.QueryEscape(runID)
	resp, err := s.doRequest("PATCH", u, row)
	if err != nil {
		return fmt.Errorf("runstate: supabase update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runstate: supabase update: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// Load returns the state for the given run ID.
func (s *SupabaseStore) Load(runID string) (*State, error) {
	u := s.restURL + "/runs?run_id=eq." + url.QueryEscape(runID) + "&limit=1"
	resp, err := s.doRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("runstate: supabase load: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("runstate: supabase load: HTTP %d: %s", resp.StatusCode, body)
	}
	var rows []supabaseRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("runstate: supabase load: decode: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("runstate: run %q not found: %w", runID, ErrRunNotFound)
	}
	return rowToState(&rows[0])
}

// List returns all run states ordered by start time, most recent first.
func (s *SupabaseStore) List() ([]*State, error) {
	return s.listWithFilter("")
}

// ListActive returns only runs that are still active or whose environment
// hasn't been torn down yet, ordered by start time, most recent first. The
// filter mirrors showsByDefault (cmd/benchctl/status.go) so that the default
// `status` listing never has to transfer full historical run data; some
// rows carry large blob columns (tofu_state, error) that are irrelevant to
// runs excluded by that predicate anyway.
func (s *SupabaseStore) ListActive() ([]*State, error) {
	return s.listWithFilter("&or=(completed_at.is.null,and(terminated_at.is.null,target_outputs.not.is.null))")
}

// listWithFilter issues the runs list query with an optional additional
// PostgREST query-string filter appended after the base ordering clause.
func (s *SupabaseStore) listWithFilter(filter string) ([]*State, error) {
	u := s.restURL + "/runs?order=started_at.desc" + filter
	resp, err := s.doRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("runstate: supabase list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("runstate: supabase list: HTTP %d: %s", resp.StatusCode, body)
	}
	var rows []supabaseRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("runstate: supabase list: decode: %w", err)
	}
	states := make([]*State, 0, len(rows))
	for i := range rows {
		st, err := rowToState(&rows[i])
		if err != nil {
			continue // skip unparseable rows
		}
		states = append(states, st)
	}
	return states, nil
}

// Delete hard-deletes the run with the given ID. When called with a service
// role key the delete bypasses RLS and removes any run regardless of created_by.
// With a user JWT only runs owned by the caller (created_by = auth.uid()) are deleted.
func (s *SupabaseStore) Delete(runID string) error {
	u := s.restURL + "/runs?run_id=eq." + url.QueryEscape(runID)
	resp, err := s.doRequest("DELETE", u, nil)
	if err != nil {
		return fmt.Errorf("runstate: supabase delete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runstate: supabase delete: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// IsShared implements runstate.SharedStore: a Supabase project's records are
// readable by every authenticated user, from any machine.
func (*SupabaseStore) IsShared() bool { return true }

// isNewFormatKey reports whether key is a Supabase new-format API key
// (sb_secret_* or sb_publishable_*) rather than a legacy JWT token.
func isNewFormatKey(key string) bool {
	return strings.HasPrefix(key, "sb_secret_") || strings.HasPrefix(key, "sb_publishable_")
}

func (s *SupabaseStore) doRequest(method, u string, body any) (*http.Response, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
	}
	resp, err := s.executeRequest(method, u, bodyBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && s.creds != nil {
		resp.Body.Close()
		if refreshErr := s.refreshCredentials(); refreshErr == nil {
			return s.executeRequest(method, u, bodyBytes)
		}
		// Refresh failed; make one more attempt so the caller gets a real error response.
		return s.executeRequest(method, u, bodyBytes)
	}
	return resp, nil
}

func (s *SupabaseStore) executeRequest(method, u string, bodyBytes []byte) (*http.Response, error) {
	var reqBody io.Reader
	if bodyBytes != nil {
		reqBody = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequest(method, u, reqBody)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	authToken := s.authToken
	s.mu.Unlock()
	// New Supabase API keys (sb_secret_* / sb_publishable_*) are not JWTs and
	// must be sent as the apikey header. Sending them as Authorization: Bearer
	// causes PostgREST to reject them with PGRST301 "Expected 3 parts in JWT".
	// Legacy JWT tokens (user JWTs from benchctl auth login) use the old path.
	if isNewFormatKey(authToken) {
		req.Header.Set("apikey", authToken)
	} else {
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("apikey", s.anonKey)
	}
	req.Header.Set("Accept", "application/json")
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Prefer", "return=minimal")
	}
	return s.client.Do(req)
}

func (s *SupabaseStore) refreshCredentials() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.creds.Refresh(s.baseURL, s.anonKey); err != nil {
		return err
	}
	s.authToken = s.creds.AccessToken
	_ = auth.SaveCredentials(s.creds)
	return nil
}
