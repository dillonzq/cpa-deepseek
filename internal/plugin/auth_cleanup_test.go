package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func writeCleanupAuth(t *testing.T, dir, name, key string) []byte {
	t.Helper()
	raw := mustJSON(map[string]any{"type": ProviderID, "id": credentialID(key), "api_key": key, "disabled": true, "proxy_url": "http://localhost:9090", "priority": 5, "custom": map[string]any{"keep": "value"}})
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return raw
}

// A disk-backed host double: runtime state is confirmed by the native host
// integration test; here listing the written disabled flag models its watcher.
func cleanupFixture(t *testing.T, dir string) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{responder: func(method string, data []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			files, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			entries := []pluginapi.HostAuthFileEntry{}
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".json") {
					continue
				}
				path := filepath.Join(dir, file.Name())
				raw, _ := os.ReadFile(path)
				var record authRecord
				_ = json.Unmarshal(raw, &record)
				entries = append(entries, pluginapi.HostAuthFileEntry{Name: file.Name(), Path: path, ID: file.Name(), Provider: record.Type, Type: record.Type, AuthIndex: "index-" + file.Name(), Disabled: record.Disabled})
			}
			return hostOK(hostAuthListResponse{Files: entries}), nil
		case pluginabi.MethodHostAuthSave:
			var req pluginapi.HostAuthSaveRequest
			if err := json.Unmarshal(data, &req); err != nil {
				return nil, err
			}
			return hostOK(struct{}{}), os.WriteFile(filepath.Join(dir, req.Name), req.JSON, 0600)
		case pluginabi.MethodHostHTTPDo:
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(fixtureCatalog)}), nil
		default:
			return hostOK(struct{}{}), nil
		}
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	return m, f
}

func TestCleanupThroughReconfigureAndKeylessLifecycle(t *testing.T) {
	dir := t.TempDir()
	writeCleanupAuth(t, dir, "A.json", "key-a")
	keep := writeCleanupAuth(t, dir, "B.json", "key-b")
	m, f := cleanupFixture(t, dir)
	apply := func(method, settings string) {
		t.Helper()
		raw, _ := m.HandleCall(method, mustJSON(lifecycleRequest{ConfigYAML: []byte(settings)}))
		if !decodeEnv(t, raw).OK {
			t.Fatalf("lifecycle failed: %s", raw)
		}
	}
	apply(pluginabi.MethodPluginRegister, fixtureConfig)
	bad, _ := m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(lifecycleRequest{ConfigYAML: []byte("api-keys: [{value: ''}]")}))
	if decodeEnv(t, bad).OK {
		t.Fatal("invalid configuration accepted")
	}
	if files, _ := os.ReadDir(dir); len(files) != 2 {
		t.Fatal("invalid configuration removed credentials")
	}
	apply(pluginabi.MethodPluginReconfigure, "api-keys: [{name: Renamed, value: key-b}]")
	if _, err := os.Stat(filepath.Join(dir, "A.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removed key's credential remains")
	}
	got, err := os.ReadFile(filepath.Join(dir, "B.json"))
	if err != nil || !bytes.Equal(got, keep) {
		t.Fatal("configured credential's disabled state or metadata changed")
	}
	requests := len(f.callsOf(pluginabi.MethodHostHTTPDo))
	apply(pluginabi.MethodPluginReconfigure, "api-keys: []\nmodels: [{name: manual}]")
	apply(pluginabi.MethodPluginReconfigure, "")
	files, _ := os.ReadDir(dir)
	if len(files) != 0 || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != requests {
		t.Fatal("empty key list retained files or requested upstream")
	}
	for _, method := range []string{pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth} {
		raw, _ := m.HandleCall(method, mustJSON(pluginapi.AuthModelRequest{AuthProvider: ProviderID, Attributes: map[string]string{"api_key": "key-b"}}))
		var models pluginapi.ModelResponse
		decodeResult(t, raw, &models)
		if len(models.Models) != 0 {
			t.Fatal("keyless plugin published models")
		}
	}
	apply(pluginabi.MethodPluginReconfigure, fixtureConfig)
	files, _ = os.ReadDir(dir)
	if len(files) != 2 {
		t.Fatal("adding keys after idle did not create credentials")
	}
	_, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil)
	if files, _ := os.ReadDir(dir); len(files) != 2 {
		t.Fatal("shutdown removed configured credentials")
	}
}

func TestKeylessColdStartCleansStaleRecords(t *testing.T) {
	for _, settings := range []string{"", "api-keys: []", "api-keys: null"} {
		t.Run(settings, func(t *testing.T) {
			dir := t.TempDir()
			writeCleanupAuth(t, dir, "stale.json", "old-key")
			m, f := cleanupFixture(t, dir)
			raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(lifecycleRequest{ConfigYAML: []byte(settings)}))
			if !decodeEnv(t, raw).OK || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
				t.Fatalf("keyless registration failed: %s", raw)
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 0 {
				t.Fatal("stale files remain")
			}
		})
	}
}

func TestCleanupOwnershipAndDiskOnlyRecords(t *testing.T) {
	dir := t.TempDir()
	writeCleanupAuth(t, dir, "managed.json", "old-key")
	foreign := map[string]string{
		"manual.json":   `{"type":"deepseek","api_key":"manual"}`,
		"mismatch.json": `{"type":"deepseek","id":"deepseek-key-forged","api_key":"secret"}`,
		"other.json":    string(mustJSON(map[string]any{"type": "codex", "id": credentialID("other"), "api_key": "other"})),
		"invalid.json":  `{`,
	}
	for name, raw := range foreign {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "managed.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	m, _ := cleanupFixture(t, dir)
	for _, name := range []string{"manual.json", "mismatch.json", "other.json", "invalid.json", "link.json", "managed.json"} {
		err := m.cleanupAuthFile(context.Background(), config.Config{}, pluginapi.HostAuthFileEntry{Name: name, Path: filepath.Join(dir, name)})
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range foreign {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("foreign credential changed: %s", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "link.json")); err != nil {
		t.Fatal("symlink removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "managed.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disk-only owned record not removed")
	}
}

func TestCleanupRequiresRuntimeConfirmation(t *testing.T) {
	dir := t.TempDir()
	writeCleanupAuth(t, dir, "pending.json", "removed")
	entry := pluginapi.HostAuthFileEntry{Name: "pending.json", Path: filepath.Join(dir, "pending.json"), AuthIndex: "pending-index"}
	bridge := NewHostBridge(func(string, []byte) ([]byte, error) {
		return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{entry}}), nil
	})
	m := NewManager(bridge)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := m.cleanupAuthFile(ctx, config.Config{}, entry); err == nil {
		t.Fatal("unconfirmed runtime disable reported success")
	}
	if _, err := os.Stat(entry.Path); err != nil {
		t.Fatal("file removed before runtime confirmation")
	}
}

func TestTimedOutAuthWriteBlocksCleanupUntilSettled(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	bridge := NewHostBridge(func(string, []byte) ([]byte, error) {
		close(started)
		<-release
		return hostOK(struct{}{}), nil
	})
	defer func() { close(release); bridge.inFlight.Wait() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: "pending.json", JSON: []byte(`{}`)})
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled save reported success")
	}
	wait, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := bridge.waitAuthWrite(wait); err == nil {
		t.Fatal("cleanup allowed while timed-out write was still running")
	}
}

func TestCleanupPreservesReplacementFile(t *testing.T) {
	dir := t.TempDir()
	writeCleanupAuth(t, dir, "changed.json", "removed")
	entry := pluginapi.HostAuthFileEntry{Name: "changed.json", Path: filepath.Join(dir, "changed.json"), AuthIndex: "changed-index", Disabled: true}
	foreign := []byte(`{"type":"deepseek","api_key":"operator-owned"}`)
	bridge := NewHostBridge(func(string, []byte) ([]byte, error) {
		// Replace the file just as CPA confirms runtime disable.
		if err := os.WriteFile(entry.Path, foreign, 0600); err != nil {
			return nil, err
		}
		return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{entry}}), nil
	})
	if err := NewManager(bridge).cleanupAuthFile(context.Background(), config.Config{}, entry); err == nil {
		t.Fatal("replacement file reported as removed")
	}
	got, err := os.ReadFile(entry.Path)
	if err != nil || !bytes.Equal(got, foreign) {
		t.Fatal("replacement file was lost")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatal("temporary credential file was not restored")
	}
}

func TestKeylessStartupIgnoresUnmanagedInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(foreign, []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	m, f := cleanupFixture(t, dir)
	raw, _ := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(lifecycleRequest{ConfigYAML: []byte("api-keys: []")}))
	if !decodeEnv(t, raw).OK {
		t.Fatalf("idle startup rejected an unmanaged file: %s", raw)
	}
	got, err := os.ReadFile(foreign)
	if err != nil || string(got) != "{" || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("idle startup changed an unmanaged credential")
	}
}

func TestCleanupRestoreDoesNotOverwriteNewFile(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	staged := "old.json.deepseek-cleanup-test"
	old := writeCleanupAuth(t, dir, staged, "old-key")
	newFile := writeCleanupAuth(t, dir, "old.json", "new-key")
	if err := restoreCleanupFile(root, staged, "old.json"); err == nil {
		t.Fatal("restoration reported success over an occupied filename")
	}
	for name, want := range map[string][]byte{staged: old, "old.json": newFile} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("restoration lost credential %s", name)
		}
	}
}
