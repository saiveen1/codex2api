package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var (
	ErrProxyAccessInvalid  = errors.New("invalid proxy access policy")
	ErrProxyAccessConflict = errors.New("proxy is bound to unauthorized accounts")
	ErrProxyAccessDenied   = errors.New("proxy is exclusive to other accounts")
)

const proxySelectColumns = `id, url, label, enabled, created_at, COALESCE(test_ip,''), COALESCE(test_location,''), COALESCE(test_timezone,''), COALESCE(test_latency_ms,0), COALESCE(test_status,'untested'), exclusive, allowed_account_ids`

type proxyAccessScanner interface {
	Scan(...any) error
}

func scanProxyAccessRow(scanner proxyAccessScanner) (*ProxyRow, error) {
	p := &ProxyRow{AllowedAccountIDs: []int64{}}
	var createdAt any
	var allowed string
	if err := scanner.Scan(&p.ID, &p.URL, &p.Label, &p.Enabled, &createdAt, &p.TestIP, &p.TestLocation, &p.TestTimezone, &p.TestLatencyMs, &p.TestStatus, &p.Exclusive, &allowed); err != nil {
		return nil, err
	}
	var err error
	p.CreatedAt, err = parseDBTimeValue(createdAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(allowed), &p.AllowedAccountIDs); err != nil {
		return nil, fmt.Errorf("decode proxy %d access: %w", p.ID, err)
	}
	if p.AllowedAccountIDs == nil {
		p.AllowedAccountIDs = []int64{}
	}
	return p, nil
}

// AllowsAccount checks only access, not health. Exclusive proxies require an explicit pin at runtime.
func (p *ProxyRow) AllowsAccount(accountID int64) bool {
	return p != nil && (!p.Exclusive || (accountID > 0 && slices.Contains(p.AllowedAccountIDs, accountID)))
}

// UpdateProxyAccess validates and commits the policy without silently unbinding accounts.
// Runtime callers must use Store.UpdateProxyAccess to publish it atomically with selection.
func (db *DB) UpdateProxyAccess(ctx context.Context, id int64, exclusive bool, allowed []int64) (*ProxyRow, error) {
	ids := append([]int64{}, allowed...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > 0 && ids[0] <= 0 {
		return nil, fmt.Errorf("%w: account IDs must be positive", ErrProxyAccessInvalid)
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Take SQLite's writer lock before reading; PostgreSQL locks the proxy row.
	if db.isSQLite() {
		if _, err := tx.ExecContext(ctx, `UPDATE proxies SET exclusive = exclusive WHERE id = $1`, id); err != nil {
			return nil, err
		}
	}
	query := `SELECT ` + proxySelectColumns + ` FROM proxies WHERE id = $1`
	if !db.isSQLite() {
		query += ` FOR UPDATE`
	}
	p, err := scanProxyAccessRow(tx.QueryRowContext(ctx, query, id))
	if err != nil {
		return nil, err
	}
	for _, accountID := range ids {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message,'') <> 'deleted'`, accountID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: account %d does not exist", ErrProxyAccessInvalid, accountID)
		}
		if err != nil {
			return nil, err
		}
	}
	if !exclusive {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE TRIM(COALESCE(proxy_url,'')) = $1 AND status <> 'deleted' AND COALESCE(error_message,'') <> 'deleted'`, strings.TrimSpace(p.URL)).Scan(&count); err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, ErrProxyAccessConflict
		}
	}
	p.Exclusive, p.AllowedAccountIDs = exclusive, ids
	if exclusive {
		var globalProxy string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(proxy_url,'') FROM system_settings WHERE id = 1`).Scan(&globalProxy); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if strings.TrimSpace(globalProxy) == strings.TrimSpace(p.URL) {
			return nil, fmt.Errorf("%w: proxy is configured as the global proxy", ErrProxyAccessConflict)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE TRIM(COALESCE(proxy_url,'')) = $1 AND status <> 'deleted' AND COALESCE(error_message,'') <> 'deleted'`, strings.TrimSpace(p.URL))
		if err != nil {
			return nil, err
		}
		var conflict bool
		for rows.Next() {
			var accountID int64
			if err := rows.Scan(&accountID); err != nil {
				rows.Close()
				return nil, err
			}
			conflict = conflict || !p.AllowsAccount(accountID)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if conflict {
			return nil, ErrProxyAccessConflict
		}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proxies SET exclusive = $1, allowed_account_ids = $2 WHERE id = $3`, exclusive, string(encoded), id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

func checkProxyAccountAccessTx(ctx context.Context, tx *sql.Tx, sqlite bool, accountID int64, proxyURL string) error {
	query := `SELECT ` + proxySelectColumns + ` FROM proxies WHERE TRIM(url) = $1`
	if !sqlite {
		query += ` FOR UPDATE`
	}
	p, err := scanProxyAccessRow(tx.QueryRowContext(ctx, query, strings.TrimSpace(proxyURL)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Custom URLs retain their existing behavior.
	}
	if err != nil {
		return err
	}
	if !p.AllowsAccount(accountID) {
		return ErrProxyAccessDenied
	}
	return nil
}

// ValidateProxyAccountAccess is a preflight check; writes must also check within their transaction.
// accountID=0 rejects exclusive nodes for new accounts and accountless/global routes.
func (db *DB) ValidateProxyAccountAccess(ctx context.Context, accountID int64, proxyURL string) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return checkProxyAccountAccessTx(ctx, tx, db.isSQLite(), accountID, proxyURL)
}

// manualProxyBindingTx checks the complete assignment before reserving a managed proxy.
// The caller must roll back on any later failure. write=false is a read-only preflight.
func manualProxyBindingTx(ctx context.Context, tx *sql.Tx, sqlite bool, accountIDs []int64, proxyURL string, write, automatic bool) error {
	return applyProxyBindingTx(ctx, tx, sqlite, accountIDs, proxyURL, write, automatic, nil)
}

func applyProxyBindingTx(ctx context.Context, tx *sql.Tx, sqlite bool, accountIDs []int64, proxyURL string, write, automatic bool, promoted *ProxyRow) error {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" || len(accountIDs) == 0 {
		return nil
	}
	for _, id := range accountIDs {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message,'') <> 'deleted'`, id).Scan(&exists); err != nil {
			return err
		}
	}
	if sqlite && write {
		if _, err := tx.ExecContext(ctx, `UPDATE proxies SET exclusive = exclusive WHERE TRIM(url) = $1`, proxyURL); err != nil {
			return err
		}
	}
	query := `SELECT ` + proxySelectColumns + ` FROM proxies WHERE TRIM(url) = $1`
	if !sqlite && write {
		query += ` FOR UPDATE`
	}
	p, err := scanProxyAccessRow(tx.QueryRowContext(ctx, query, proxyURL))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if automatic && p.Exclusive {
		return ErrProxyAccessDenied
	}
	var globalProxy string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(proxy_url,'') FROM system_settings WHERE id = 1`).Scan(&globalProxy); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.TrimSpace(globalProxy) == proxyURL {
		return fmt.Errorf("%w: proxy is configured as the global proxy", ErrProxyAccessConflict)
	}
	if p.Exclusive && len(p.AllowedAccountIDs) > 0 {
		for _, id := range accountIDs {
			if !p.AllowsAccount(id) {
				return ErrProxyAccessDenied
			}
		}
		return nil
	}
	if !write {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE TRIM(COALESCE(proxy_url,'')) = $1 AND status <> 'deleted' AND COALESCE(error_message,'') <> 'deleted'`, proxyURL)
	if err != nil {
		return err
	}
	ids := append([]int64{}, p.AllowedAccountIDs...)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	ids = append(ids, accountIDs...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE proxies SET exclusive = $1, allowed_account_ids = $2 WHERE id = $3`, true, string(encoded), p.ID)
	if err == nil && promoted != nil {
		p.Exclusive, p.AllowedAccountIDs = true, ids
		*promoted = *p
	}
	return err
}

// ValidateManualProxyBinding is a non-mutating preflight; the write rechecks under lock.
func (db *DB) ValidateManualProxyBinding(ctx context.Context, accountIDs []int64, proxyURL string) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return manualProxyBindingTx(ctx, tx, db.isSQLite(), accountIDs, proxyURL, false, false)
}

// ReconcileBoundProxyAccess reserves shared managed proxies with existing active pins.
// Global proxies are deliberately left untouched; callers publish runtime changes.
func (db *DB) ReconcileBoundProxyAccess(ctx context.Context) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	aggregate := "json_agg(id)"
	if db.isSQLite() {
		aggregate = "json_group_array(id)"
		if _, err := tx.ExecContext(ctx, `UPDATE proxies SET exclusive = exclusive WHERE 1 = 0`); err != nil {
			return err
		}
	}
	query := `WITH pins AS (
		SELECT TRIM(proxy_url) AS url, ` + aggregate + ` AS ids FROM accounts
		WHERE TRIM(COALESCE(proxy_url,'')) <> '' AND status <> 'deleted' AND COALESCE(error_message,'') <> 'deleted'
		GROUP BY TRIM(proxy_url)
	) SELECT p.id, p.allowed_account_ids, pins.ids FROM proxies p JOIN pins ON pins.url = TRIM(p.url)
	WHERE NOT p.exclusive AND pins.url <> COALESCE((SELECT TRIM(proxy_url) FROM system_settings WHERE id = 1), '')
	ORDER BY p.id`
	if !db.isSQLite() {
		query += ` FOR UPDATE OF p`
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	type policy struct {
		id            int64
		allowed, pins []byte
	}
	var policies []policy
	for rows.Next() {
		var p policy
		if err := rows.Scan(&p.id, &p.allowed, &p.pins); err != nil {
			rows.Close()
			return err
		}
		policies = append(policies, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range policies {
		var allowed, pins []int64
		if err := json.Unmarshal(p.allowed, &allowed); err != nil {
			return err
		}
		if err := json.Unmarshal(p.pins, &pins); err != nil {
			return err
		}
		ids := append(allowed, pins...)
		slices.Sort(ids)
		encoded, err := json.Marshal(slices.Compact(ids))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proxies SET exclusive = true, allowed_account_ids = $1 WHERE id = $2`, string(encoded), p.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
