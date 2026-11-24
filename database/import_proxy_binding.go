package database

import "strings"

// ImportProxyCommitter publishes the transaction's exact policy before routing resumes.
// A nil row means the insert did not change a managed proxy (e.g. a custom URL).
type ImportProxyCommitter func(write func() (*ProxyRow, error)) (*ProxyRow, error)

func (db *DB) SetImportProxyCommitter(commit ImportProxyCommitter) {
	db.importProxyCommit.Store(&commit)
}

func (db *DB) withImportedProxyBinding(proxyURL string, write func(*ProxyRow) error) error {
	commit := db.importProxyCommit.Load()
	if strings.TrimSpace(proxyURL) == "" || commit == nil || *commit == nil {
		return write(nil)
	}
	_, err := (*commit)(func() (*ProxyRow, error) {
		var promoted ProxyRow
		if err := write(&promoted); err != nil || !promoted.Exclusive {
			return nil, err
		}
		return &promoted, nil
	})
	return err
}
