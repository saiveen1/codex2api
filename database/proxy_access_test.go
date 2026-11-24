package database

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestProxyAccessSharedDefaultAndPersistence(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()
	id, err := db.InsertProxy(ctx, "http://exclusive.example:8080", "manual")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "allowed", "rt-a", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetProxy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if row.Exclusive || !row.AllowsAccount(999) || !strings.Contains(string(encoded), `"allowed_account_ids":[]`) {
		t.Fatalf("shared default: %s", encoded)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, []int64{a, a}); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.ListEnabledProxies(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("exclusive entered public candidates: %v %v", rows, err)
	}
	readers := map[string]func() ([]*ProxyRow, error){
		"list":    func() ([]*ProxyRow, error) { return db.ListProxies(ctx) },
		"enabled": func() ([]*ProxyRow, error) { return db.ListEnabledProxyInventory(ctx) },
		"ids":     func() ([]*ProxyRow, error) { return db.ListProxiesByIDs(ctx, []int64{id}) },
		"get":     func() ([]*ProxyRow, error) { row, err := db.GetProxy(ctx, id); return []*ProxyRow{row}, err },
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			rows, err := read()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || !rows[0].Exclusive || !slices.Equal(rows[0].AllowedAccountIDs, []int64{a}) || rows[0].AllowsAccount(a+1) {
				t.Fatalf("policy not scanned: %#v", rows)
			}
		})
	}
	newURL := "http://renamed.example:8080"
	if err := db.UpdateProxy(ctx, id, &newURL, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateProxyTestResult(ctx, id, newURL, ProxyTestStatusSuccess, "192.0.2.1", "test", 1); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil || !row.Exclusive || !row.AllowsAccount(a) {
		t.Fatalf("URL/test update lost policy: %#v %v", row, err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, false, nil); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil || row.Exclusive || row.AllowedAccountIDs == nil {
		t.Fatalf("shared reset: %#v %v", row, err)
	}
}

func TestProxyAccessValidationConflictAndAtomicAssignments(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()
	const proxyURL = "http://private.example:8080"
	id, err := db.InsertProxy(ctx, proxyURL, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "a", "rt-a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.InsertAccount(ctx, "b", "rt-b", "")
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic legacy shared pin, predating automatic reservation.
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET proxy_url=$1 WHERE id=$2`, " "+proxyURL+" ", b); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]int64{{0}, {-1}, {99999999}} {
		if _, err := db.UpdateProxyAccess(ctx, id, true, ids); !errors.Is(err, ErrProxyAccessInvalid) {
			t.Fatalf("IDs %v: %v", ids, err)
		}
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, []int64{a}); !errors.Is(err, ErrProxyAccessConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if row, err := db.GetProxy(ctx, id); err != nil || row.Exclusive {
		t.Fatalf("conflict changed row: %#v %v", row, err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, nil); !errors.Is(err, ErrProxyAccessConflict) {
		t.Fatalf("empty allowlist must not displace existing pin: %v", err)
	}
	if err := db.SetAccountProxyURLs(ctx, map[int64]string{b: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, nil); err != nil {
		t.Fatalf("reserve unbound proxy: %v", err)
	}
	if row, err := db.GetProxy(ctx, id); err != nil || !row.Exclusive || len(row.AllowedAccountIDs) != 0 || row.AllowsAccount(a) {
		t.Fatalf("empty reservation: %#v %v", row, err)
	}
	if rows, err := db.ListEnabledProxies(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("reserved proxy entered pool: %v %v", rows, err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, []int64{a}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountProxyURLs(ctx, map[int64]string{a: proxyURL, b: proxyURL}); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("unauthorized assignment: %v", err)
	}
	var bound string
	if err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(proxy_url,'') FROM accounts WHERE id = $1`, a).Scan(&bound); err != nil || bound != "" {
		t.Fatalf("assignment was not atomic: %q %v", bound, err)
	}
	if err := db.UpdateAccountProxyURL(ctx, a, proxyURL); err != nil {
		t.Fatal(err)
	}
	newURL := "http://new-private.example:8080"
	if err := db.UpdateProxy(ctx, id, &newURL, nil, nil); err != nil {
		t.Fatal(err)
	}
	ids, err := db.RebindAccountProxyURLs(ctx, proxyURL, newURL)
	if err != nil || !slices.Equal(ids, []int64{a}) {
		t.Fatalf("rebind: %v %v", ids, err)
	}
	if _, err := db.RetireProxiesByIDs(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(proxy_url,'') FROM accounts WHERE id = $1`, a).Scan(&bound); err != nil || bound != "" {
		t.Fatalf("retire left pin: %q %v", bound, err)
	}
}

func TestSQLiteProxyAccessMigrationKeepsExistingRowsShared(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `ALTER TABLE proxies DROP COLUMN exclusive`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `ALTER TABLE proxies DROP COLUMN allowed_account_ids`); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertProxy(ctx, "http://old.example:8080", "old")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.migrateSQLite(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetProxy(ctx, id)
	if err != nil || row.Exclusive || len(row.AllowedAccountIDs) != 0 {
		t.Fatalf("migration: %#v %v", row, err)
	}
}

func TestProxyAccessGlobalConflictAndPinSaveGuards(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()
	const url = "http://guarded.example:8080"
	id, err := db.InsertProxy(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "allowed", "rt-a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.InsertAccount(ctx, "denied", "rt-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id,proxy_url) VALUES (1,$1) ON CONFLICT(id) DO UPDATE SET proxy_url = excluded.proxy_url`, " "+url+" "); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, []int64{a}); !errors.Is(err, ErrProxyAccessConflict) {
		t.Fatalf("global proxy became exclusive: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET proxy_url = '' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, []int64{a}); err != nil {
		t.Fatal(err)
	}
	if err := db.ValidateProxyAccountAccess(ctx, b, url); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("preflight: %v", err)
	}
	if err := db.ValidateProxyAccountAccess(ctx, a, url); err != nil {
		t.Fatal(err)
	}
	inserts := []func() (int64, error){
		func() (int64, error) { return db.InsertAccount(ctx, "new", "rt-new", url) },
		func() (int64, error) { return db.InsertATAccount(ctx, "new", "at-new", url) },
		func() (int64, error) { return db.InsertAccountWithCredentials(ctx, "new", nil, url) },
		func() (int64, error) { return db.InsertOpenAIResponsesAccount(ctx, "new", nil, url) },
		func() (int64, error) { return db.InsertAccountWithUpstream(ctx, "new", "xai", "api", nil, url) },
	}
	for _, insert := range inserts {
		if _, err := insert(); !errors.Is(err, ErrProxyAccessDenied) {
			t.Fatalf("new unknown account pin accepted: %v", err)
		}
	}
	if count, err := db.CountAll(ctx); err != nil || count != 2 {
		t.Fatalf("denied insert left account: %d %v", count, err)
	}
	if err := db.UpdateOAuthAccountCredentials(ctx, b, nil, url); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("OAuth save accepted: %v", err)
	}
	if err := db.UpdateOpenAIResponsesAccount(ctx, b, "b", nil, url); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("responses save accepted: %v", err)
	}
	err = db.UpdateAccountSchedulerMetadata(ctx, b, OptionalNullInt64{}, OptionalNullInt64{}, OptionalBool{}, OptionalInt64Slice{}, OptionalStringSlice{}, OptionalInt64Slice{}, OptionalString{Set: true, Value: url}, nil)
	if !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("scheduler save accepted: %v", err)
	}
	if _, err := db.BatchUpdateAccountMetadata(ctx, []int64{a, b}, BatchAccountMetadataUpdate{ProxyURL: OptionalString{Set: true, Value: url}}); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("batch save accepted: %v", err)
	}
}

func TestManualProxyBindingReservationAndAtomicBatch(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()
	url := "http://manual-binding.example:8080"
	id, err := db.InsertProxy(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "a", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic legacy shared pin, predating automatic reservation.
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET proxy_url=$1 WHERE id=$2`, url, a); err != nil {
		t.Fatal(err)
	}
	b, err := db.InsertAccount(ctx, "b", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.InsertAccount(ctx, "c", "c", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ValidateManualProxyBinding(ctx, []int64{b}, url); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetProxy(ctx, id)
	if err != nil || row.Exclusive {
		t.Fatalf("preflight mutated policy: %#v %v", row, err)
	}
	if err := db.UpdateAccountProxyURL(ctx, b, url); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil || !row.Exclusive || !row.AllowsAccount(a) || !row.AllowsAccount(b) || row.AllowsAccount(c) {
		t.Fatalf("legacy pins lost: %#v %v", row, err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, false, nil); !errors.Is(err, ErrProxyAccessConflict) {
		t.Fatalf("reopened bound proxy: %v", err)
	}
	if err := db.ValidateManualProxyBinding(ctx, []int64{b, c}, url); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("preflight foreign: %v", err)
	}
	if _, err := db.BatchUpdateAccountMetadata(ctx, []int64{b, c}, BatchAccountMetadataUpdate{ProxyURL: OptionalString{Set: true, Value: url}}); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("batch foreign: %v", err)
	}
	if err := db.UpdateAccountProxyURL(ctx, b, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAccountProxyURL(ctx, a, ""); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil || !row.Exclusive {
		t.Fatalf("unbind reopened: %#v %v", row, err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateOAuthAccountCredentials(ctx, b, nil, url); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("credential merge claimed a reservation: %v", err)
	}
	if _, err := db.BatchUpdateAccountMetadata(ctx, []int64{b, c}, BatchAccountMetadataUpdate{ProxyURL: OptionalString{Set: true, Value: url}}); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil || !row.Exclusive || !row.AllowsAccount(b) || !row.AllowsAccount(c) || row.AllowsAccount(a) {
		t.Fatalf("reserved batch: %#v %v", row, err)
	}
}

func TestAutomaticProxyBindingAndReconcile(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()
	url := "http://auto-binding.example:8080"
	id, err := db.InsertProxy(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "a", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.InsertAccount(ctx, "b", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	reservedID, err := db.InsertProxy(ctx, "http://reserved-auto.example:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateProxyAccess(ctx, reservedID, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountProxyURLs(ctx, map[int64]string{a: "http://reserved-auto.example:8080", b: url}); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("auto claimed reservation: %v", err)
	}
	row, err := db.GetProxy(ctx, id)
	if err != nil || row.Exclusive {
		t.Fatalf("failed batch changed policy: %#v %v", row, err)
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER fail_proxy_assignment BEFORE UPDATE OF proxy_url ON accounts BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountProxyURLs(ctx, map[int64]string{a: url}); err == nil {
		t.Fatal("expected assignment failure")
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil || row.Exclusive {
		t.Fatalf("failed write changed policy: %#v %v", row, err)
	}
	if _, err := db.conn.ExecContext(ctx, `DROP TRIGGER fail_proxy_assignment`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountProxyURLs(ctx, map[int64]string{a: url, b: url}); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil || !row.Exclusive || !row.AllowsAccount(a) || !row.AllowsAccount(b) {
		t.Fatalf("auto policy: %#v %v", row, err)
	}
	if err := db.SetAccountProxyURLs(ctx, map[int64]string{a: url}); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("auto accepted exclusive: %v", err)
	}
	legacy := "http://legacy-binding.example:8080"
	legacyID, err := db.InsertProxy(ctx, legacy, "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := db.InsertAccount(ctx, "old", "old", "")
	if err != nil {
		t.Fatal(err)
	}
	// Reconciliation operates on synthetic historical shared pins.
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET proxy_url=$1 WHERE id=$2`, legacy, old); err != nil {
		t.Fatal(err)
	}
	global := "http://global-binding.example:8080"
	globalID, err := db.InsertProxy(ctx, global, "")
	if err != nil {
		t.Fatal(err)
	}
	globalAccount, err := db.InsertAccount(ctx, "global", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET proxy_url=$1 WHERE id=$2`, global, globalAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id,proxy_url) VALUES (1,$1) ON CONFLICT(id) DO UPDATE SET proxy_url = excluded.proxy_url`, global); err != nil {
		t.Fatal(err)
	}
	if err := db.ValidateManualProxyBinding(ctx, []int64{a}, global); !errors.Is(err, ErrProxyAccessConflict) {
		t.Fatalf("global preflight: %v", err)
	}
	if err := db.ReconcileBoundProxyAccess(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileBoundProxyAccess(ctx); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetProxy(ctx, legacyID)
	if err != nil || !row.Exclusive || !row.AllowsAccount(old) {
		t.Fatalf("reconcile: %#v %v", row, err)
	}
	row, err = db.GetProxy(ctx, globalID)
	if err != nil || row.Exclusive {
		t.Fatalf("global changed: %#v %v", row, err)
	}
}

func TestImportedProxyBinding(t *testing.T) {
	ctx := context.Background()
	inserts := map[string]func(*DB, string) (int64, error){
		"refresh":     func(db *DB, url string) (int64, error) { return db.InsertAccount(ctx, "new", "rt", url) },
		"access":      func(db *DB, url string) (int64, error) { return db.InsertATAccount(ctx, "new", "at", url) },
		"credentials": func(db *DB, url string) (int64, error) { return db.InsertAccountWithCredentials(ctx, "new", nil, url) },
		"pending": func(db *DB, url string) (int64, error) {
			return db.InsertPendingAccountWithCredentials(ctx, "new", nil, url, "", nil)
		},
		"responses": func(db *DB, url string) (int64, error) { return db.InsertOpenAIResponsesAccount(ctx, "new", nil, url) },
		"upstream": func(db *DB, url string) (int64, error) {
			return db.InsertAccountWithUpstream(ctx, "new", "xai", "api", nil, url)
		},
		"gateway": func(db *DB, url string) (int64, error) {
			return db.InsertOAuthGatewayAccount(ctx, "new", nil, url, nil)
		},
		"grok": func(db *DB, url string) (int64, error) {
			id, _, err := db.InsertGrokAccountIfAbsent(ctx, "new", map[string]any{"refresh_token": "rt"}, url, true)
			return id, err
		},
	}
	for name, insert := range inserts {
		t.Run(name, func(t *testing.T) {
			db := newProxyTestDB(t)
			const url = "http://import.example:8080"
			proxyID, err := db.InsertProxy(ctx, url, "")
			if err != nil {
				t.Fatal(err)
			}
			id, err := insert(db, url)
			if err != nil || id <= 0 {
				t.Fatalf("first import: id=%d err=%v", id, err)
			}
			row, err := db.GetProxy(ctx, proxyID)
			if err != nil || !row.Exclusive || !slices.Equal(row.AllowedAccountIDs, []int64{id}) {
				t.Fatalf("binding: %#v %v", row, err)
			}
			if _, err := insert(db, url); !errors.Is(err, ErrProxyAccessDenied) {
				t.Fatalf("second import: %v", err)
			}
			if count, err := db.CountAll(ctx); err != nil || count != 1 {
				t.Fatalf("rollback: %d %v", count, err)
			}
		})
	}
}

func TestImportedProxyBindingRollback(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()
	const url = "http://rollback-import.example:8080"
	id, err := db.InsertProxy(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER fail_import_family BEFORE UPDATE OF credential_family_id ON accounts BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertAccount(ctx, "new", "rt", url); err == nil {
		t.Fatal("expected failure")
	}
	row, err := db.GetProxy(ctx, id)
	if err != nil || row.Exclusive || len(row.AllowedAccountIDs) != 0 {
		t.Fatalf("proxy not rolled back: %#v %v", row, err)
	}
	if count, err := db.CountAll(ctx); err != nil || count != 0 {
		t.Fatalf("account not rolled back: %d %v", count, err)
	}
}

func TestImportedProxyBindingConflictsAndCustomURL(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()
	const reserved = "http://reserved-import.example:8080"
	const global = "http://global-import.example:8080"
	reservedID, err := db.InsertProxy(ctx, reserved, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateProxyAccess(ctx, reservedID, true, nil); err != nil {
		t.Fatal(err)
	}
	globalID, err := db.InsertProxy(ctx, global, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id,proxy_url) VALUES (1,$1) ON CONFLICT(id) DO UPDATE SET proxy_url=excluded.proxy_url`, global); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertAccount(ctx, "reserved", "rt1", reserved); !errors.Is(err, ErrProxyAccessDenied) {
		t.Fatalf("reserved: %v", err)
	}
	if _, err := db.InsertAccount(ctx, "global", "rt2", global); !errors.Is(err, ErrProxyAccessConflict) {
		t.Fatalf("global: %v", err)
	}
	if count, err := db.CountAll(ctx); err != nil || count != 0 {
		t.Fatalf("failed imports persisted: %d %v", count, err)
	}
	for _, url := range []string{"", "http://custom-import.example:8080"} {
		if _, err := db.InsertAccount(ctx, "custom", "rt", url); err != nil {
			t.Fatalf("custom %q: %v", url, err)
		}
	}
	row, err := db.GetProxy(ctx, globalID)
	if err != nil || row.Exclusive || len(row.AllowedAccountIDs) != 0 {
		t.Fatalf("global changed: %#v %v", row, err)
	}
}
