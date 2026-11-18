package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrImageJobFinished = errors.New("image job already finished")

// A nil key is admin scope. Use the same ownership predicate for the update
// and the conflict lookup, so terminal jobs cannot leak across keys.
func (db *DB) CancelImageJob(ctx context.Context, id int64, keyID *int64) ([]string, error) {
	scope := `id=$1`
	args := []any{id}
	if keyID != nil {
		scope += ` AND api_key_id=$2`
		args = append(args, *keyID)
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `UPDATE image_generation_jobs SET status='cancelled', completed_at=CURRENT_TIMESTAMP, error_message='' WHERE `+scope+` AND status IN ('queued','running') RETURNING params_json`, args...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM image_generation_jobs WHERE `+scope, args...).Scan(&status); err != nil {
			return nil, err
		}
		if status != ImageJobCancelled {
			return nil, ErrImageJobFinished
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var params map[string]json.RawMessage
	var inputs []string
	if json.Unmarshal([]byte(raw), &params) == nil && params["input_images"] != nil {
		_ = json.Unmarshal(params["input_images"], &inputs)
		delete(params, "input_images")
		clean, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE image_generation_jobs SET params_json=$1 WHERE id=$2`, string(clean), id); err != nil {
			return nil, err
		}
	}
	return inputs, tx.Commit()
}

// Check the durable state before dispatch, even if the cancel request was
// served by a different process. Legacy installs need no queue columns.
func (db *DB) CheckRunningImageJob(ctx context.Context, id int64, owner string) error {
	q := `SELECT 1 FROM image_generation_jobs WHERE id=$1 AND status='running'`
	args := []any{id}
	if owner != "" {
		q += ` AND queue_owner=$2 AND queue_lease_until >= $3`
		args = append(args, owner, time.Now().Unix())
	}
	var one int
	return db.conn.QueryRowContext(ctx, q, args...).Scan(&one)
}
