package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestImportUsageProbeSettingsSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	read := func(want bool) *SystemSettings {
		t.Helper()
		settings, err := db.GetSystemSettings(ctx)
		if err != nil || settings == nil {
			t.Fatalf("GetSystemSettings returned %v, want a settings row", err)
		}
		if settings.ImportUsageProbeEnabled == nil {
			t.Fatal("GetSystemSettings returned a nil import probe setting")
		}
		if got := *settings.ImportUsageProbeEnabled; got != want {
			t.Fatalf("import_usage_probe_enabled = %t, want %t", got, want)
		}
		return settings
	}
	save := func(settings *SystemSettings) {
		t.Helper()
		if err := db.UpdateSystemSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	read(true)
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM system_settings WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	disabled, enabled := false, true
	save(&SystemSettings{ImportUsageProbeEnabled: &disabled})
	settings := read(false)
	settings.SiteName = "Unrelated settings save"
	save(settings)
	if err := db.UpdateCodexSyncedCLIVersion(ctx, "9.9.9"); err != nil {
		t.Fatal(err)
	}
	settings = read(false)
	if settings.SiteName != "Unrelated settings save" || settings.CodexSyncedCLIVersion != "9.9.9" {
		t.Fatal("unrelated settings did not persist")
	}
	settings.ImportUsageProbeEnabled = &enabled
	save(settings)
	read(true)
	settings.ImportUsageProbeEnabled = &disabled
	save(settings)
	settings.ImportUsageProbeEnabled = nil
	save(settings)
	read(true)

	if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings DROP COLUMN import_usage_probe_enabled`); err != nil {
		t.Fatalf("simulate legacy schema: %v", err)
	}
	if err := db.migrateSQLite(ctx); err != nil {
		t.Fatalf("migrate legacy settings: %v", err)
	}
	settings = read(true)
	if settings.SiteName != "Unrelated settings save" {
		t.Fatal("migration did not preserve unrelated settings")
	}
	settings.ImportUsageProbeEnabled = &disabled
	save(settings)
	if err := db.migrateSQLite(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	read(false)
}
