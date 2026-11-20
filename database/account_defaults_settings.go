package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type AccountConcurrencyRule struct {
	Channel     string `json:"channel"`
	Plan        string `json:"plan"`
	Concurrency int    `json:"concurrency"`
}

type AccountDefaultsConfig struct {
	ConcurrencyRules      []AccountConcurrencyRule `json:"concurrency_rules"`
	DefaultTimezone       string                   `json:"default_timezone"`
	UseFixedProxyTimezone bool                     `json:"use_fixed_proxy_timezone"`
}

func (db *DB) LoadAccountDefaultsConfig(ctx context.Context) (AccountDefaultsConfig, error) {
	cfg := AccountDefaultsConfig{}
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(account_defaults_config, ''), '{}') FROM system_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return AccountDefaultsConfig{}, err
	}
	return cfg, nil
}

func (db *DB) SaveAccountDefaultsConfig(ctx context.Context, cfg AccountDefaultsConfig) error {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, account_defaults_config) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET account_defaults_config = EXCLUDED.account_defaults_config
		`, string(payload))
		return err
	})
}
