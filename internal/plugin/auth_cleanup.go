package plugin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dillonzq/cpa-deepseek/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Cleanup uses physical records, not host IDs (which can be filenames). Only
// records with our type and the exact ID we generate from their key are owned.
// No keys, host error bodies or file contents are included in cleanup errors.
func (m *Manager) cleanupAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil {
		return fmt.Errorf("host bridge unavailable")
	}
	if err := m.bridge.waitAuthWrite(ctx); err != nil {
		return err
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return err
	}
	var failure error
	for _, entry := range entries {
		if entry.RuntimeOnly || entry.Provider != "" && entry.Provider != ProviderID || entry.Type != "" && entry.Type != ProviderID {
			continue
		}
		if err := m.cleanupAuthFile(ctx, cfg, entry); err != nil {
			failure = err
		}
	}
	return failure
}

func (m *Manager) cleanupAuthFile(ctx context.Context, cfg config.Config, entry pluginapi.HostAuthFileEntry) error {
	name := entry.Name
	if !filepath.IsAbs(entry.Path) || filepath.Base(entry.Path) != name || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".json") {
		return nil // No usable physical file, e.g. a runtime-only host listing.
	}
	root, err := os.OpenRoot(filepath.Dir(entry.Path))
	if err != nil {
		return fmt.Errorf("open credential directory failed")
	}
	defer root.Close()
	_, record, err := readCleanupRecord(root, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read credential failed")
	}
	if record == nil || cfg.HasAPIKey(record.APIKey) {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("credential cleanup canceled")
	}
	// The v8.0.0 host save callback does not apply the disabled flag to its
	// runtime Auth. Write the file and let CPA's normal directory watcher parse
	// it, then require the runtime listing to confirm before unlinking. During
	// cold start there is no runtime index yet, so there is no scheduler entry
	// that could rewrite the file.
	if entry.AuthIndex != "" {
		if err := disableCleanupFile(root, name, record.ID); err != nil {
			return err
		}
		if err := m.awaitDisabledAuth(ctx, entry); err != nil {
			return err
		}
	}
	// Move to a unique non-JSON name, then verify the exact file being deleted.
	// If another writer replaced it, preserve that file instead of unlinking it.
	staged := name + ".deepseek-cleanup-" + rand.Text()
	if err := root.Rename(name, staged); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stage credential deletion failed")
	}
	_, current, readErr := readCleanupRecord(root, staged)
	if readErr != nil || current == nil || current.ID != record.ID || entry.AuthIndex != "" && !current.Disabled {
		if err := restoreCleanupFile(root, staged, name); err != nil {
			return err
		}
		return fmt.Errorf("credential changed during cleanup; preserved")
	}
	if err := root.Remove(staged); err != nil {
		if err := restoreCleanupFile(root, staged, name); err != nil {
			return err
		}
		return fmt.Errorf("delete credential failed")
	}
	return nil
}

// Never rename back over a replacement file. On filesystems without hard
// links, preserve the staged file for manual recovery rather than overwrite.
func restoreCleanupFile(root *os.Root, staged, name string) error {
	if err := root.Link(staged, name); err != nil {
		return fmt.Errorf("credential preserved in a *.deepseek-cleanup-* file; restore it manually")
	}
	if err := root.Remove(staged); err != nil {
		return fmt.Errorf("credential restored; extra *.deepseek-cleanup-* file remains")
	}
	return nil
}

func disableCleanupFile(root *os.Root, name, id string) error {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("credential changed before disabling")
	}
	file, err := openCleanupRecord(root, name, os.O_RDWR)
	if err != nil {
		return fmt.Errorf("open credential for disabling failed")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("credential changed before disabling")
	}
	// Prove ownership again on the descriptor that will be written.
	previous, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	var record authRecord
	if err != nil || json.Unmarshal(previous, &record) != nil || record.Type != ProviderID || record.ID != id || credentialID(record.APIKey) != id {
		return fmt.Errorf("credential changed before disabling")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(previous, &fields) != nil {
		return fmt.Errorf("invalid credential")
	}
	fields["disabled"] = json.RawMessage("true")
	raw, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode disabled credential failed")
	}
	if _, err := file.WriteAt(raw, 0); err != nil {
		return fmt.Errorf("write disabled credential failed")
	}
	if err := file.Truncate(int64(len(raw))); err != nil {
		return fmt.Errorf("truncate disabled credential failed")
	}
	return nil
}

func (m *Manager) awaitDisabledAuth(ctx context.Context, entry pluginapi.HostAuthFileEntry) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		entries, err := m.bridge.AuthList(ctx)
		if err != nil {
			return fmt.Errorf("confirm disabled credential failed")
		}
		active := false
		for _, current := range entries {
			if current.AuthIndex == entry.AuthIndex || current.Path == entry.Path {
				if !current.Disabled && current.Status != "disabled" {
					active = true
				}
			}
		}
		if !active {
			return nil // All matching records are disabled or already removed.
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("credential disable not confirmed")
		case <-ticker.C:
		}
	}
}

// Reject links, non-regular files and oversized/foreign/malformed records.
// Root confines filesystem operations to the host-reported directory.
func readCleanupRecord(root *os.Root, name string) ([]byte, *authRecord, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, nil
	}
	file, err := openCleanupRecord(root, name, os.O_RDONLY)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, nil, fmt.Errorf("credential changed while reading")
	}
	const maxCleanupRecordBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(file, maxCleanupRecordBytes+1))
	if err != nil {
		return nil, nil, err
	}
	var record authRecord
	if len(raw) > maxCleanupRecordBytes || json.Unmarshal(raw, &record) != nil || record.Type != ProviderID || record.APIKey == "" || record.ID != credentialID(record.APIKey) {
		return raw, nil, nil
	}
	return raw, &record, nil
}
