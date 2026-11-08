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
	dir := t.TempDir()
	listed := filepath.Join(dir, "other.json")
	if err := os.WriteFile(listed, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatal(err)
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
	cfg := config.Config{APIKeys: []config.APIKey{{Name: "dillonzq@outlook.com", Value: "key-a"}, {Value: "key-b"}}}
	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	want := []string{"dillonzq-outlook-com.json", "DeepSeek-" + strings.TrimPrefix(credentialID("key-b"), "deepseek-key-")[:8] + ".json"}
	if len(saves) != len(want) {
		t.Fatalf("auth saves=%d, want %d", len(saves), len(want))
	}
	for i, call := range saves {
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(call.payload, &req); err != nil {
			t.Fatal(err)
		}
		var record struct{ ID string }
		if err := json.Unmarshal(req.JSON, &record); err != nil {
			t.Fatal(err)
		}
		if req.Name != want[i] || record.ID != credentialID(cfg.APIKeys[i].Value) {
			t.Fatalf("saved %q id %q, want %q", req.Name, record.ID, want[i])
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

// With no listed absolute path the auth directory cannot be checked, so a
// readable name could overwrite an unlisted file; use the key-derived name.
func TestMaterializeFallsBackWithoutAuthDir(t *testing.T) {
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{}}), nil
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
	if want := credentialID("key-a") + ".json"; req.Name != want {
		t.Fatalf("saved name=%q, want %q", req.Name, want)
	}
}

func TestMaterializeMatchesKeyBehindHostFilenameID(t *testing.T) {
	for _, tc := range []struct{ name, provider string }{
		{"Personal.json", ProviderID},
		{"deepseek-key-Personal.json", ProviderID},
		{"deepseek-key-" + strings.Repeat("a", 59) + ".json", ProviderID},
		{credentialID("key-a") + ".json", ProviderID},
		{"ProviderMissing.json", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.name
			path := filepath.Join(t.TempDir(), name)
			original := []byte(`{"type":"deepseek","api_key":"key-a","disabled":true,"proxy_url":"http://localhost:9999","custom":"keep"}`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
				if method == pluginabi.MethodHostAuthList {
					return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
						{ID: name, Name: name, Provider: tc.provider, Path: path},
					}}), nil
				}
				return hostOK(struct{}{}), nil
			}}
			cfg := config.Config{APIKeys: []config.APIKey{{Name: "Renamed", Value: "key-a"}}}
			// Fresh managers exercise restarts as well as repeated initialization.
			for range 2 {
				m := NewManager(NewHostBridge(f.call))
				if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			if saves := len(f.callsOf(pluginabi.MethodHostAuthSave)); saves != 0 {
				t.Fatalf("saved %d duplicate records for an existing key", saves)
			}
			if stored, err := os.ReadFile(path); err != nil || string(stored) != string(original) {
				t.Fatal("existing credential state changed")
			}
		})
	}
}

func TestMaterializeFindsUnlistedCredentialByKey(t *testing.T) {
	dir := t.TempDir()
	listed := filepath.Join(dir, "other.json")
	path := filepath.Join(dir, "Personal.json")
	original := []byte(`{"type":"deepseek","api_key":"key-a","disabled":true,"proxy_url":"http://localhost:9999","custom":"keep"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listed, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			// The host manager has not loaded the DeepSeek file yet.
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: "other.json", Name: "other.json", Provider: "codex", Path: listed},
			}}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	cfg := config.Config{APIKeys: []config.APIKey{{Name: "Renamed", Value: "key-a"}, {Name: "Personal", Value: "key-b"}}}
	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 1 {
		t.Fatalf("auth saves=%d, want 1 (only the new key)", len(saves))
	}
	var req pluginapi.HostAuthSaveRequest
	if err := json.Unmarshal(saves[0].payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "Personal-2.json" || !strings.Contains(string(req.JSON), credentialID("key-b")) {
		t.Fatalf("saved %q with wrong record", req.Name)
	}
	if stored, err := os.ReadFile(path); err != nil || string(stored) != string(original) {
		t.Fatal("existing credential state changed")
	}
}

func TestMaterializeStopsWhenExistingIdentityCannotBeRead(t *testing.T) {
	for _, body := range []string{"", `{"type":"deepseek","api_key":"key-a"`, `{"type":"deepseek"}`} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Personal.json")
			if body != "" {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
				if method == pluginabi.MethodHostAuthList {
					return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
						{ID: "Personal.json", Name: "Personal.json", Provider: ProviderID, Path: path},
					}}), nil
				}
				return hostOK(struct{}{}), nil
			}}
			m := NewManager(NewHostBridge(f.call))
			cfg := config.Config{APIKeys: []config.APIKey{{Name: "Personal", Value: "key-a"}}}
			err := m.materializeAuthRecords(context.Background(), cfg)
			if err == nil {
				t.Fatal("unreadable existing identity was treated as a missing credential")
			}
			if strings.Contains(err.Error(), "key-a") || strings.Contains(err.Error(), path) {
				t.Fatal("materialization error exposes credential data")
			}
			if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
				t.Fatal("saved a duplicate after failing to read the existing credential")
			}
		})
	}
}

func TestMaterializeAcceptsRemovedKeyIdentityWithoutDiskPath(t *testing.T) {
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: credentialID("key-a"), Name: "Personal.json", Provider: ProviderID},
				{ID: credentialID("removed-key"), Name: "Old.json", Provider: ProviderID},
			}}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	cfg := config.Config{APIKeys: []config.APIKey{{Name: "Personal", Value: "key-a"}, {Name: "Team", Value: "key-b"}}}
	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatalf("removed key's missing path blocked materialization: %v", err)
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 1 {
		t.Fatalf("auth saves=%d, want 1 (only the new key)", len(saves))
	}
	var req pluginapi.HostAuthSaveRequest
	if err := json.Unmarshal(saves[0].payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != credentialID("key-b")+".json" {
		t.Fatalf("saved %q, want the new key's filename", req.Name)
	}
}

func TestMaterializeStopsWhenUnlistedCredentialCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Personal.json")
	if err := os.WriteFile(path, []byte(`{"type":"deepseek","api_key":"key-a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this platform or user can read files without read permissions")
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: "other.json", Name: "other.json", Provider: "codex", Path: filepath.Join(dir, "other.json")},
			}}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	err := m.materializeAuthRecords(context.Background(), config.Config{APIKeys: []config.APIKey{{Name: "Personal", Value: "key-a"}}})
	if err == nil {
		t.Fatal("unreadable unlisted credential was treated as a missing credential")
	}
	if strings.Contains(err.Error(), "key-a") || strings.Contains(err.Error(), path) {
		t.Fatal("materialization error exposes credential data")
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("saved a duplicate after failing to read an unlisted credential")
	}
}

// Nested credentials must keep their original files and host-managed state.
func TestMaterializeFindsUnlistedNestedCredential(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "accounts", "personal")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "Personal.JSON")
	original := []byte(`{"type":"deepseek","api_key":"key-a","disabled":true,"proxy_url":"http://localhost:9999","custom":"keep"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{Name: "Other.json", Provider: "codex", Path: filepath.Join(dir, "Other.json")},
			}}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	for range 2 {
		m := NewManager(NewHostBridge(f.call))
		cfg := config.Config{APIKeys: []config.APIKey{{Name: "Renamed", Value: "key-a"}}}
		if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("saved a duplicate of a nested credential")
	}
	if stored, err := os.ReadFile(path); err != nil || string(stored) != string(original) {
		t.Fatal("nested credential state changed")
	}
}

// A digest-shaped filename reserves a name but does not identify its contents.
func TestMaterializeSeparatesReservedNamesFromIdentities(t *testing.T) {
	for _, tc := range []struct {
		name, body, provider string
		listed               bool
	}{
		{"unlisted invalid JSON", `not json`, "", false},
		{"unlisted other key", `{"type":"deepseek","api_key":"key-b"}`, ProviderID, false},
		{"unlisted other provider", `{"type":"codex","api_key":"key-a"}`, "codex", false},
		{"listed other key", `{"type":"deepseek","api_key":"key-b"}`, ProviderID, true},
		{"listed other provider", `{"type":"codex","api_key":"key-a"}`, "codex", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			name := credentialID("key-a") + ".json"
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
				if method == pluginabi.MethodHostAuthList {
					entries := []pluginapi.HostAuthFileEntry{{Name: "Other.json", Provider: "codex", Path: filepath.Join(dir, "Other.json")}}
					if tc.listed {
						entries = append(entries, pluginapi.HostAuthFileEntry{ID: name, Name: name, Provider: tc.provider, Path: path})
					}
					return hostOK(hostAuthListResponse{Files: entries}), nil
				}
				return hostOK(struct{}{}), nil
			}}
			m := NewManager(NewHostBridge(f.call))
			cfg := config.Config{APIKeys: []config.APIKey{{Name: strings.TrimSuffix(name, ".json"), Value: "key-a"}}}
			if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			saves := f.callsOf(pluginabi.MethodHostAuthSave)
			if len(saves) != 1 {
				t.Fatalf("auth saves=%d, want 1 for the missing key", len(saves))
			}
			var req pluginapi.HostAuthSaveRequest
			if err := json.Unmarshal(saves[0].payload, &req); err != nil {
				t.Fatal(err)
			}
			if req.Name != strings.TrimSuffix(name, ".json")+"-2.json" {
				t.Fatalf("saved %q over an occupied name", req.Name)
			}
			var record authRecord
			if err := json.Unmarshal(req.JSON, &record); err != nil || record.APIKey != "key-a" {
				t.Fatal("saved the wrong credential")
			}
			if stored, err := os.ReadFile(path); err != nil || string(stored) != tc.body {
				t.Fatal("occupied credential file changed")
			}
		})
	}
}

// An occupied fallback filename cannot be overwritten without verifying its key.
func TestMaterializeRejectsUnverifiableFallbackFilename(t *testing.T) {
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{Name: credentialID("key-a") + ".json"},
			}}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	if err := m.materializeAuthRecords(context.Background(), config.Config{APIKeys: []config.APIKey{{Value: "key-a"}}}); err == nil {
		t.Fatal("occupied fallback filename was accepted without verifying its key")
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("overwrote an unverifiable fallback credential")
	}
}
