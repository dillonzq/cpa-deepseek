//go:build unix

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// FIFOs, including symlink targets, must fail without waiting for a writer.
func TestMaterializeRejectsNonRegularAuthRecords(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed bool
		link   bool
	}{
		{"unlisted FIFO", false, false},
		{"listed FIFO", true, false},
		{"unlisted symlink to FIFO", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "Personal.json")
			fifo := path
			if tc.link {
				fifo = filepath.Join(dir, "pipe")
			}
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.link {
				if err := os.Symlink(fifo, path); err != nil {
					t.Fatal(err)
				}
			}
			f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
				if method == pluginabi.MethodHostAuthList {
					entries := []pluginapi.HostAuthFileEntry{{Name: "Other.json", Provider: "codex", Path: filepath.Join(dir, "Other.json")}}
					if tc.listed {
						entries = append(entries, pluginapi.HostAuthFileEntry{ID: "Personal.json", Name: "Personal.json", Provider: ProviderID, Path: path})
					}
					return hostOK(hostAuthListResponse{Files: entries}), nil
				}
				return hostOK(struct{}{}), nil
			}}
			m := NewManager(NewHostBridge(f.call))
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- m.materializeAuthRecords(ctx, config.Config{APIKeys: []config.APIKey{{Name: "Personal", Value: "key-a"}}})
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("nonregular auth entry was accepted")
				}
				if strings.Contains(err.Error(), path) {
					t.Fatal("materialization error exposes the auth path")
				}
			case <-time.After(time.Second):
				// Release a blocking implementation before reporting the failure.
				writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
				if err == nil {
					_ = writer.Close()
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("FIFO reader did not drain")
				}
				t.Fatal("auth initialization blocked on a FIFO past its deadline")
			}
			if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
				t.Fatal("materialization saved a credential after encountering a FIFO")
			}
		})
	}
}

// Directory and file symlinks remain usable when the opened file is regular.
func TestMaterializeReusesSymlinkedRegularCredential(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "auth")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	storedPath := filepath.Join(t.TempDir(), "Original.json")
	original := []byte(`{"type":"deepseek","api_key":"key-a","disabled":true,"custom":"keep"}`)
	if err := os.WriteFile(storedPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(storedPath, filepath.Join(dir, "nested", "Personal.json")); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{Name: "Other.json", Provider: "codex", Path: filepath.Join(alias, "Other.json")},
			}}), nil
		}
		return hostOK(struct{}{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	if err := m.materializeAuthRecords(context.Background(), config.Config{APIKeys: []config.APIKey{{Name: "Renamed", Value: "key-a"}}}); err != nil {
		t.Fatal(err)
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("saved a duplicate of a symlinked regular credential")
	}
	if stored, err := os.ReadFile(storedPath); err != nil || string(stored) != string(original) {
		t.Fatal("symlink target's credential state changed")
	}
}
