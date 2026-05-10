package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pluginFixture builds a Plugin pointing at a captured BetterDo API stub.
// Returns the plugin, a record-of-calls slice, and a cleanup fn.
func pluginFixture(t *testing.T, apiHandler http.Handler) (*Plugin, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(apiHandler)
	t.Cleanup(srv.Close)
	stateDir := t.TempDir()
	p := &Plugin{
		apiURL:   srv.URL,
		userMap:  map[string]string{"alice": "alice-uuid"},
		stateDir: stateDir,
		token:    "bdo_plg_test",
		pluginID: "00000000-0000-0000-0000-000000000001",
	}
	return p, srv
}

func decodeIssueOpenedBody(t *testing.T, title, url, action, user string, id int) []byte {
	t.Helper()
	hook := GitLabIssueHook{}
	hook.ObjectKind = "issue"
	hook.ObjectAttributes.ID = id
	hook.ObjectAttributes.Title = title
	hook.ObjectAttributes.URL = url
	hook.ObjectAttributes.Action = action
	hook.User.Username = user
	body, err := json.Marshal(hook)
	if err != nil {
		t.Fatalf("marshal hook: %v", err)
	}
	return body
}

func TestWebhook_IssueOpen_CreatesTask(t *testing.T) {
	called := 0
	api := http.NewServeMux()
	api.HandleFunc("/api/v1/todos", func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Header.Get("Authorization") != "Bearer bdo_plg_test" {
			t.Errorf("missing/wrong Authorization: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-BetterDo-User") != "alice-uuid" {
			t.Errorf("wrong X-BetterDo-User: %q", r.Header.Get("X-BetterDo-User"))
		}
		body, _ := io.ReadAll(r.Body)
		var req todoCreateRequest
		_ = json.Unmarshal(body, &req)
		if !strings.Contains(req.Notes, "https://example.com/issues/7") {
			t.Errorf("notes missing source URL: %q", req.Notes)
		}
		if req.Title != "test-issue" {
			t.Errorf("title=%q want test-issue", req.Title)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"todo-uuid-1"}`))
	})
	p, _ := pluginFixture(t, api)
	rec := httptest.NewRecorder()
	body := decodeIssueOpenedBody(t, "test-issue", "https://example.com/issues/7", "open", "alice", 7)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if called != 1 {
		t.Errorf("expected 1 API call, got %d", called)
	}
	state, _ := p.loadState()
	if state["issue_7"] != "todo-uuid-1" {
		t.Errorf("state[issue_7]=%q want todo-uuid-1", state["issue_7"])
	}
}

func TestWebhook_IssueClose_CompletesTask(t *testing.T) {
	called := 0
	api := http.NewServeMux()
	api.HandleFunc("/api/v1/todos/todo-uuid-1", func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Method != http.MethodPut {
			t.Errorf("method=%s want PUT", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var req todoUpdateRequest
		_ = json.Unmarshal(body, &req)
		if !req.IsCompleted {
			t.Errorf("isCompleted=false want true")
		}
		w.WriteHeader(http.StatusOK)
	})
	p, _ := pluginFixture(t, api)
	// seed state
	_ = p.saveState(map[string]string{"issue_7": "todo-uuid-1"})
	rec := httptest.NewRecorder()
	body := decodeIssueOpenedBody(t, "ignored", "ignored", "close", "alice", 7)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if called != 1 {
		t.Errorf("expected 1 API call, got %d", called)
	}
}

func TestWebhook_IssueClose_UnknownID_NoOp(t *testing.T) {
	api := http.NewServeMux()
	api.HandleFunc("/api/v1/todos/", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("API should not be called for unknown issue close")
	})
	p, _ := pluginFixture(t, api)
	rec := httptest.NewRecorder()
	body := decodeIssueOpenedBody(t, "ignored", "ignored", "close", "alice", 999)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d want 204", rec.Code)
	}
}

func TestWebhook_IssueReopen_UncompletesTask(t *testing.T) {
	called := 0
	api := http.NewServeMux()
	api.HandleFunc("/api/v1/todos/todo-uuid-2", func(w http.ResponseWriter, r *http.Request) {
		called++
		var req todoUpdateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.IsCompleted {
			t.Errorf("isCompleted=true want false")
		}
		w.WriteHeader(http.StatusOK)
	})
	p, _ := pluginFixture(t, api)
	_ = p.saveState(map[string]string{"issue_8": "todo-uuid-2"})
	rec := httptest.NewRecorder()
	body := decodeIssueOpenedBody(t, "ignored", "ignored", "reopen", "alice", 8)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if called != 1 {
		t.Errorf("expected 1 API call, got %d", called)
	}
}

func TestWebhook_UnknownEvent_NoCall(t *testing.T) {
	api := http.NewServeMux()
	api.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("API should not be called for unknown object_kind")
	})
	p, _ := pluginFixture(t, api)
	rec := httptest.NewRecorder()
	hook := map[string]any{"object_kind": "merge_request"}
	body, _ := json.Marshal(hook)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d want 204", rec.Code)
	}
}

func TestWebhook_UnknownAction_NoCall(t *testing.T) {
	api := http.NewServeMux()
	api.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("API should not be called for unknown action")
	})
	p, _ := pluginFixture(t, api)
	rec := httptest.NewRecorder()
	body := decodeIssueOpenedBody(t, "x", "x", "weighted", "alice", 1)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d want 204", rec.Code)
	}
}

func TestWebhook_MalformedJSON_400(t *testing.T) {
	p, _ := pluginFixture(t, http.NewServeMux())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("not json"))
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

func TestWebhook_UnknownUser_400(t *testing.T) {
	p, _ := pluginFixture(t, http.NewServeMux())
	rec := httptest.NewRecorder()
	body := decodeIssueOpenedBody(t, "x", "x", "open", "bob", 1)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "BETTERDO_PLUGIN_GITLAB_USER_MAP") {
		t.Errorf("body missing user-map hint: %s", rec.Body.String())
	}
}

func TestWebhook_WrongMethod_405(t *testing.T) {
	p, _ := pluginFixture(t, http.NewServeMux())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	p.handleWebhook(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", rec.Code)
	}
}

func TestHealth_ReturnsOK(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handleHealth(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("body=%q", body)
	}
}

func TestState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := &Plugin{stateDir: dir}
	state := map[string]string{"issue_1": "todo-1", "issue_2": "todo-2"}
	if err := p.saveState(state); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := p.loadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got["issue_1"] != "todo-1" {
		t.Errorf("got=%v", got)
	}
	// Confirm file mode is 0600.
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm=%v want 0600", info.Mode().Perm())
	}
}

func TestState_LoadMissing_ReturnsEmptyMap(t *testing.T) {
	dir := t.TempDir()
	p := &Plugin{stateDir: dir}
	state, err := p.loadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state) != 0 {
		t.Errorf("got=%v want empty", state)
	}
}
