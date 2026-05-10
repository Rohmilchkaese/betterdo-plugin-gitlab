// Package main is the BetterDo reference plugin: a GitLab webhook → BetterDo
// task mirror, demonstrating the plugin contract end-to-end.
//
// Wire shape:
//   - Startup → POST /api/v1/plugins/register with the manifest;
//     receive {pluginId, token, expiresAt}.
//   - GET /health → {"status": "ok"} (unauthenticated; polled by the
//     BetterDo lifecycle worker).
//   - POST /webhook → parses GitLab "Issue Hook" payloads (open / close /
//     reopen) and translates them to /api/v1/todos calls against BetterDo.
//
// Configuration (env vars):
//   BETTERDO_API_URL                Required. e.g. http://betterdo-api:8080
//   BETTERDO_PLUGIN_GITLAB_USER_MAP JSON object {"<gitlab-username>": "<betterdo-user-uuid>"}
//   BETTERDO_PLUGIN_PORT            Optional; default :8090
//   BETTERDO_PLUGIN_STATE_DIR       Optional; default /tmp (issue→todo state)
//
// Forward-compat: declares contractVersion "1.0" per the v1 lock at
// docs/plugin-contract.md (Story 13.3). When v2 lands, plugin authors
// re-publish under a new manifest version with the new contract version.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	pluginName    = "betterdo-plugin-gitlab"
	pluginVersion = "0.1.0"
	contractVer   = "1.0"
	defaultPort   = ":8090"
	defaultState  = "/tmp"
	tokenWarn     = 7 * 24 * time.Hour // matches PLUGIN_TOKEN_EXPIRY_WARN default
)

type Manifest struct {
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	ContractVersion string   `json:"contractVersion"`
	Capabilities    []string `json:"capabilities"`
	HealthURL       string   `json:"healthUrl"`
	WebhookURL      string   `json:"webhookUrl"`
}

type registerResponse struct {
	PluginID  string    `json:"pluginId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type GitLabIssueHook struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		Action string `json:"action"`
	} `json:"object_attributes"`
	User struct {
		Username string `json:"username"`
	} `json:"user"`
}

type todoCreateRequest struct {
	Title string   `json:"title"`
	Notes string   `json:"notes"`
	Tags  []string `json:"tags,omitempty"`
}

type todoUpdateRequest struct {
	IsCompleted bool `json:"isCompleted"`
}

type todoResponse struct {
	ID string `json:"id"`
}

// Plugin holds the live token + state mapping.
type Plugin struct {
	apiURL    string
	userMap   map[string]string
	stateDir  string
	tokenLock sync.RWMutex
	token     string
	pluginID  string
	expiresAt time.Time
}

func newPlugin() *Plugin {
	apiURL := os.Getenv("BETTERDO_API_URL")
	if apiURL == "" {
		log.Fatal("BETTERDO_API_URL is required")
	}
	mapJSON := os.Getenv("BETTERDO_PLUGIN_GITLAB_USER_MAP")
	userMap := map[string]string{}
	if mapJSON != "" {
		if err := json.Unmarshal([]byte(mapJSON), &userMap); err != nil {
			log.Fatalf("BETTERDO_PLUGIN_GITLAB_USER_MAP malformed: %v", err)
		}
	}
	stateDir := os.Getenv("BETTERDO_PLUGIN_STATE_DIR")
	if stateDir == "" {
		stateDir = defaultState
	}
	return &Plugin{apiURL: apiURL, userMap: userMap, stateDir: stateDir}
}

func (p *Plugin) register(ctx context.Context) error {
	manifest := Manifest{
		Name:            pluginName,
		Version:         pluginVersion,
		ContractVersion: contractVer,
		Capabilities:    []string{"read", "write"},
		HealthURL:       "http://" + pluginName + defaultPort + "/health",
		WebhookURL:      "http://" + pluginName + defaultPort + "/webhook",
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+"/api/v1/plugins/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register: status %d: %s", resp.StatusCode, string(raw))
	}
	var rr registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return err
	}
	p.tokenLock.Lock()
	p.token = rr.Token
	p.pluginID = rr.PluginID
	p.expiresAt = rr.ExpiresAt
	p.tokenLock.Unlock()
	if time.Until(rr.ExpiresAt) < tokenWarn {
		log.Printf("WARN token expires within warn window (%s)", rr.ExpiresAt)
	}
	log.Printf("registered: pluginId=%s expiresAt=%s", rr.PluginID, rr.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (p *Plugin) authToken() string {
	p.tokenLock.RLock()
	defer p.tokenLock.RUnlock()
	return p.token
}

// stateFile maps GitLab issue IDs → BetterDo todo IDs (close path needs the
// reverse lookup). Single-key file mount per the developer guide's "no
// service-layer database" anti-pattern.
func (p *Plugin) statePath() string { return filepath.Join(p.stateDir, "state.json") }

func (p *Plugin) loadState() (map[string]string, error) {
	data, err := os.ReadFile(p.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Plugin) saveState(state map[string]string) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(p.statePath(), data, 0o600)
}

func (p *Plugin) postTodo(ctx context.Context, userID string, req todoCreateRequest) (string, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+"/api/v1/todos", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.authToken())
	httpReq.Header.Set("X-BetterDo-User", userID)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create: status %d: %s", resp.StatusCode, string(raw))
	}
	var out todoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (p *Plugin) putTodo(ctx context.Context, userID, todoID string, req todoUpdateRequest) error {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, p.apiURL+"/api/v1/todos/"+todoID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.authToken())
	httpReq.Header.Set("X-BetterDo-User", userID)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update: status %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

func (p *Plugin) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var hook GitLabIssueHook
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		http.Error(w, "malformed JSON", http.StatusBadRequest)
		return
	}
	if hook.ObjectKind != "issue" {
		// Forward-compat: ignore unknown event types (per the contract spec
		// principle for unknown entityType in federation).
		log.Printf("ignoring object_kind=%q", hook.ObjectKind)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	userID, ok := p.userMap[hook.User.Username]
	if !ok {
		http.Error(w, "unknown user — add to BETTERDO_PLUGIN_GITLAB_USER_MAP", http.StatusBadRequest)
		return
	}
	state, err := p.loadState()
	if err != nil {
		http.Error(w, "state load failed", http.StatusInternalServerError)
		return
	}
	issueKey := fmt.Sprintf("issue_%d", hook.ObjectAttributes.ID)
	switch hook.ObjectAttributes.Action {
	case "open":
		todoID, err := p.postTodo(r.Context(), userID, todoCreateRequest{
			Title: hook.ObjectAttributes.Title,
			Notes: "Source: " + hook.ObjectAttributes.URL,
			Tags:  []string{"gitlab"},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		state[issueKey] = todoID
		if err := p.saveState(state); err != nil {
			log.Printf("WARN state save failed: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	case "close":
		todoID, ok := state[issueKey]
		if !ok {
			// Idempotent — close on an unknown issue is a no-op.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := p.putTodo(r.Context(), userID, todoID, todoUpdateRequest{IsCompleted: true}); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "reopen":
		todoID, ok := state[issueKey]
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := p.putTodo(r.Context(), userID, todoID, todoUpdateRequest{IsCompleted: false}); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		log.Printf("ignoring action=%q", hook.ObjectAttributes.Action)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func main() {
	port := os.Getenv("BETTERDO_PLUGIN_PORT")
	if port == "" {
		port = defaultPort
	}
	plugin := newPlugin()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := plugin.register(ctx); err != nil {
		log.Fatalf("register: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/webhook", plugin.handleWebhook)
	log.Printf("listening on %s (pluginId=%s)", port, plugin.pluginID)
	if err := http.ListenAndServe(port, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
