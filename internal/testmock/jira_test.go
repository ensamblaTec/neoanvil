package testmock

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// makeRequest is a small helper that issues a request against the mock with
// optional Basic Auth and a JSON body.
func makeRequest(t *testing.T, method, url string, auth [2]string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if auth[0] != "" {
		req.SetBasicAuth(auth[0], auth[1])
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http: %v", err)
	}
	return resp
}

func defaultAuth() [2]string { return [2]string{"test@example.com", "fake-token"} }

func TestJiraMock_GetIssue_NotFoundWhenNoFixture(t *testing.T) {
	m := NewJira(t)
	resp := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/UNKNOWN-1", defaultAuth(), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestJiraMock_GetIssue_ReturnsRegisteredFixture(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{
		Summary:     "Test issue",
		Status:      "In Progress",
		Description: "Body of the issue",
		Comments: []JiraComment{
			{Author: "Alice", Body: "First comment", Created: "2026-05-08T00:00:00Z"},
		},
	})

	resp := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1", defaultAuth(), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var got struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
			Description any `json:"description"`
			Comment     struct {
				Comments []map[string]any `json:"comments"`
			} `json:"comment"`
		} `json:"fields"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Key != "MCPI-1" {
		t.Errorf("key=%q want MCPI-1", got.Key)
	}
	if got.Fields.Summary != "Test issue" {
		t.Errorf("summary=%q want Test issue", got.Fields.Summary)
	}
	if got.Fields.Status.Name != "In Progress" {
		t.Errorf("status=%q want In Progress", got.Fields.Status.Name)
	}
	if got.Fields.Description == nil {
		t.Errorf("description was nil, want ADF doc")
	}
	if len(got.Fields.Comment.Comments) != 1 {
		t.Errorf("comments=%d want 1", len(got.Fields.Comment.Comments))
	}
}

func TestJiraMock_RejectsBadAuth(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "Open"})

	cases := []struct {
		name string
		auth [2]string
	}{
		{"no_auth", [2]string{"", ""}},
		{"wrong_email", [2]string{"hacker@example.com", "fake-token"}},
		{"wrong_token", [2]string{"test@example.com", "wrong"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1", tc.auth, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401", resp.StatusCode)
			}
		})
	}
}

func TestJiraMock_SetCredentialsOverride(t *testing.T) {
	m := NewJira(t)
	m.SetCredentials("custom@user.com", "secret")
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "Open"})

	resp := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1",
		[2]string{"custom@user.com", "secret"}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200 with overridden creds", resp.StatusCode)
	}

	resp2 := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1", defaultAuth(), nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("default creds should now fail, got status=%d", resp2.StatusCode)
	}
}

func TestJiraMock_RateLimitReturns429(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "Open"})
	m.SetRateLimit(true)

	resp := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1", defaultAuth(), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Errorf("Retry-After header missing")
	}

	m.SetRateLimit(false)
	resp2 := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1", defaultAuth(), nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("after disable: status=%d want 200", resp2.StatusCode)
	}
}

func TestJiraMock_ListTransitionsReturnsRegistered(t *testing.T) {
	m := NewJira(t)
	m.SetTransitions("MCPI-1", []JiraTransition{
		{ID: "11", Name: "Start Progress", ToStatus: "In Progress"},
		{ID: "21", Name: "Mark Done", ToStatus: "Done"},
	})

	resp := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1/transitions", defaultAuth(), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var got struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Transitions) != 2 {
		t.Fatalf("len=%d want 2", len(got.Transitions))
	}
	if got.Transitions[1].To.Name != "Done" {
		t.Errorf("transition[1].to.name=%q want Done", got.Transitions[1].To.Name)
	}
}

func TestJiraMock_DoTransitionRequiresKnownIssue(t *testing.T) {
	m := NewJira(t)
	resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue/UNKNOWN/transitions",
		defaultAuth(), map[string]any{"transition": map[string]any{"id": "11"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
}

func TestJiraMock_DoTransitionMissingID(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "Open"})
	resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue/MCPI-1/transitions",
		defaultAuth(), map[string]any{"transition": map[string]any{}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestJiraMock_DoTransitionSuccess(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "Open"})
	resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue/MCPI-1/transitions",
		defaultAuth(), map[string]any{
			"transition": map[string]any{"id": "11"},
			"update": map[string]any{
				"comment": []map[string]any{
					{"add": map[string]any{"body": map[string]any{"type": "doc"}}},
				},
			},
		})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want 204", resp.StatusCode)
	}
}

func TestJiraMock_CreateIssueAssignsKey(t *testing.T) {
	m := NewJira(t)
	resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue", defaultAuth(),
		map[string]any{
			"fields": map[string]any{
				"project":   map[string]any{"key": "MCPI"},
				"issuetype": map[string]any{"name": "Story"},
				"summary":   "New story",
			},
		})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d want 201", resp.StatusCode)
	}
	var got struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Self string `json:"self"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Key != "MCPI-1" {
		t.Errorf("key=%q want MCPI-1", got.Key)
	}
	if got.ID == "" || !strings.HasPrefix(got.Self, m.URL()) {
		t.Errorf("id=%q self=%q malformed", got.ID, got.Self)
	}

	resp2 := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue", defaultAuth(),
		map[string]any{
			"fields": map[string]any{
				"project":   map[string]any{"key": "MCPI"},
				"issuetype": map[string]any{"name": "Bug"},
				"summary":   "Second one",
			},
		})
	defer resp2.Body.Close()
	var got2 struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	if got2.Key != "MCPI-2" {
		t.Errorf("second key=%q want MCPI-2", got2.Key)
	}
}

func TestJiraMock_CreateIssueRequiredFields(t *testing.T) {
	m := NewJira(t)
	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"missing_project", map[string]any{
			"issuetype": map[string]any{"name": "Story"},
			"summary":   "x",
		}},
		{"missing_issuetype", map[string]any{
			"project": map[string]any{"key": "MCPI"},
			"summary": "x",
		}},
		{"missing_summary", map[string]any{
			"project":   map[string]any{"key": "MCPI"},
			"issuetype": map[string]any{"name": "Story"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue", defaultAuth(),
				map[string]any{"fields": tc.fields})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d want 400", resp.StatusCode)
			}
		})
	}
}

func TestJiraMock_CallsAndCallCount(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "Open"})

	for range 3 {
		resp := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1", defaultAuth(), nil)
		_ = resp.Body.Close()
	}
	if got := m.CallCount(); got != 3 {
		t.Errorf("CallCount=%d want 3", got)
	}
	calls := m.Calls()
	if len(calls) != 3 {
		t.Fatalf("len(Calls)=%d want 3", len(calls))
	}
	for i, c := range calls {
		if c.Method != http.MethodGet {
			t.Errorf("call[%d].Method=%q want GET", i, c.Method)
		}
		if !strings.HasSuffix(c.Path, "/issue/MCPI-1") {
			t.Errorf("call[%d].Path=%q missing /issue/MCPI-1", i, c.Path)
		}
		if c.Header.Get("Authorization") == "" {
			t.Errorf("call[%d] missing Authorization header", i)
		}
	}
}

func TestJiraMock_CreateThenGetRoundTrips(t *testing.T) {
	m := NewJira(t)
	resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue", defaultAuth(),
		map[string]any{
			"fields": map[string]any{
				"project":   map[string]any{"key": "MCPI"},
				"issuetype": map[string]any{"name": "Story"},
				"summary":   "Round-trip story",
			},
		})
	defer resp.Body.Close()
	var created struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	if created.Key == "" {
		t.Fatalf("create did not return a key")
	}

	resp2 := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/"+created.Key, defaultAuth(), nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET created issue: status=%d want 200", resp2.StatusCode)
	}
	var got struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
		} `json:"fields"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Key != created.Key {
		t.Errorf("round-trip key mismatch got=%q want=%q", got.Key, created.Key)
	}
	if got.Fields.Summary != "Round-trip story" {
		t.Errorf("round-trip summary=%q", got.Fields.Summary)
	}
	if got.Fields.Status.Name != "To Do" {
		t.Errorf("default status=%q want To Do", got.Fields.Status.Name)
	}
}

func TestJiraMock_DoTransitionRejectsUnregisteredID(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "Open"})
	m.SetTransitions("MCPI-1", []JiraTransition{
		{ID: "11", Name: "Start", ToStatus: "In Progress"},
	})

	resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue/MCPI-1/transitions",
		defaultAuth(), map[string]any{"transition": map[string]any{"id": "999"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 for unregistered transition id", resp.StatusCode)
	}

	resp2 := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue/MCPI-1/transitions",
		defaultAuth(), map[string]any{"transition": map[string]any{"id": "11"}})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("registered id: status=%d want 204", resp2.StatusCode)
	}
}

func TestJiraMock_TransitionFlipsStatus(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "To Do"})
	m.SetTransitions("MCPI-1", []JiraTransition{
		{ID: "11", Name: "Start", ToStatus: "In Progress"},
		{ID: "21", Name: "Mark Done", ToStatus: "Done"},
	})

	resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue/MCPI-1/transitions",
		defaultAuth(), map[string]any{"transition": map[string]any{"id": "21"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want 204", resp.StatusCode)
	}

	resp2 := makeRequest(t, http.MethodGet, m.URL()+"/rest/api/3/issue/MCPI-1", defaultAuth(), nil)
	defer resp2.Body.Close()
	var got struct {
		Fields struct {
			Status struct {
				Name string `json:"name"`
			} `json:"status"`
		} `json:"fields"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got)
	if got.Fields.Status.Name != "Done" {
		t.Errorf("status after transition=%q want Done", got.Fields.Status.Name)
	}
}

func TestJiraMock_CallsBodyCaptured(t *testing.T) {
	m := NewJira(t)
	m.SetIssue("MCPI-1", JiraIssue{Summary: "x", Status: "Open"})
	body := map[string]any{"transition": map[string]any{"id": "11"}}
	resp := makeRequest(t, http.MethodPost, m.URL()+"/rest/api/3/issue/MCPI-1/transitions",
		defaultAuth(), body)
	_ = resp.Body.Close()

	calls := m.Calls()
	if len(calls) != 1 {
		t.Fatalf("len=%d want 1", len(calls))
	}
	var got map[string]any
	if err := json.Unmarshal(calls[0].Body, &got); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}
	tr, _ := got["transition"].(map[string]any)
	if tr["id"] != "11" {
		t.Errorf("captured transition.id=%v want 11", tr["id"])
	}
}
