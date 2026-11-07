package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAvailableAuthName(t *testing.T) {
	cases := []struct {
		name, label string
		existing    map[string]bool
		want        string
	}{
		{"email", "dillonzq@outlook.com", nil, "dillonzq-outlook-com.json"},
		{"default label", "DeepSeek 04a98abc", nil, "DeepSeek-04a98abc.json"},
		{"empty after sanitizing", "@@@", nil, "DeepSeek.json"},
		{"case-insensitive collision", "Personal", map[string]bool{"personal.json": true, "PERSONAL-2.JSON": true}, "Personal-3.json"},
		{"windows device", "con", nil, "DeepSeek-con.json"},
		{"device prefix is ordinary", "Console", nil, "Console.json"},
		{"truncated", strings.Repeat("a", 100), nil, strings.Repeat("a", 80) + ".json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := availableAuthName(tc.label, tc.existing); got != tc.want {
				t.Fatalf("filename=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestMaterializedAuthUsesReadableFilename(t *testing.T) {
	_, f := newFixture(t, nil)
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) == 0 {
		t.Fatal("no credentials materialized")
	}
	for _, call := range saves {
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(call.payload, &req); err != nil {
			t.Fatal(err)
		}
		var record struct {
			ID, Label string
		}
		if err := json.Unmarshal(req.JSON, &record); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(req.Name, "deepseek-key-") || req.Name != availableAuthName(record.Label, nil) {
			t.Fatalf("filename=%q for label %q", req.Name, record.Label)
		}
		if !strings.HasPrefix(record.ID, "deepseek-key-") {
			t.Fatalf("identity changed: %q", record.ID)
		}
	}
}

// Before the auth manager is ready, v8 lists disk files without IDs; a
// readable filename must still be matched to its key to avoid duplicates.
func TestMaterializeRecoversIdentityFromDiskListing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Personal.json")
	if err := os.WriteFile(path, []byte(`{"type":"deepseek","id":"deepseek-key-x","label":"Personal","api_key":"key-a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{Name: "Personal.json", Type: ProviderID, Provider: ProviderID, Source: "file", Path: path},
			}}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	cfg := config.Config{APIKeys: []config.APIKey{{Name: "Personal", Value: "key-a"}, {Name: "Personal", Value: "key-b"}}}
	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 1 {
		t.Fatalf("auth saves=%d, want 1 (only key-b)", len(saves))
	}
	var req pluginapi.HostAuthSaveRequest
	if err := json.Unmarshal(saves[0].payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "Personal-2.json" || !strings.Contains(string(req.JSON), credentialID("key-b")) {
		t.Fatalf("saved %q with wrong record", req.Name)
	}
}

// AuthList can omit files the auth manager did not load; AuthSave overwrites,
// so a readable name must not land on an unlisted file in the auth directory.
func TestMaterializeSkipsUnlistedFiles(t *testing.T) {
	dir := t.TempDir()
	listed := filepath.Join(dir, "other.json")
	for path, body := range map[string]string{listed: `{"type":"codex"}`, filepath.Join(dir, "Team.json"): `not json`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: "other.json", Name: "other.json", Type: "codex", Provider: "codex", Source: "memory", Path: listed},
			}}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	cfg := config.Config{APIKeys: []config.APIKey{{Name: "Team", Value: "key-a"}}}
	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 1 {
		t.Fatalf("auth saves=%d, want 1", len(saves))
	}
	var req pluginapi.HostAuthSaveRequest
	if err := json.Unmarshal(saves[0].payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "Team-2.json" {
		t.Fatalf("saved name=%q, want Team-2.json", req.Name)
	}
}
