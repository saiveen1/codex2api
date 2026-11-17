package database

import "context"

// GetChatGPTWebExecutionForAdmin is a cross-key lookup for the authenticated admin route only.
func (db *DB) GetChatGPTWebExecutionForAdmin(ctx context.Context, id string) (*ChatGPTWebExecution, error) {
	return scanChatGPTWebExecution(db.conn.QueryRowContext(ctx, `SELECT `+chatGPTWebExecutionColumns+` FROM chatgpt_web_executions WHERE id=$1`, id))
}
