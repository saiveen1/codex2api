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
	if (exclusive && len(ids) == 0) || (len(ids) > 0 && ids[0] <= 0) {
		return nil, fmt.Errorf("%w: exclusive requires nonempty positive account IDs", ErrProxyAccessInvalid)
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
