package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/google/uuid"
)

var (
	ErrOAuthGatewayEncryption = errors.New("OAuth gateway requires CODEX_CRED_ENCRYPTION_KEY")
	ErrOAuthGatewayOwner      = errors.New("OAuth gateway state is owned by another instance or ownership was lost")
	ErrOAuthGatewayState      = errors.New("OAuth gateway encrypted state is unavailable; restart and reconcile original tasks")
)

const oauthGatewayStateLimit = 64 << 20
const oauthGatewayLeaseSeconds = int64(120)

// OAuthGatewayStore shares the application's database and encryption root, but
// never permits the optional plaintext fallback used by legacy account fields.
// A singleton lease fences other replicas; CAS failure poisons this instance.
// shortcut: only the owning replica serves this admin module; use sticky routing
// until a distributed job queue is required.
type OAuthGatewayStore struct {
	mu         sync.Mutex
	db         *DB
	cipher     cipher.AEAD
	owner      string
	revision   int64
	leaseUntil int64
	state      service.State
	closed     bool
	poisoned   bool
}

func (db *DB) OpenOAuthGatewayStore(ctx context.Context) (*OAuthGatewayStore, error) {
	root := credCipherKey()
	if len(root) == 0 {
		return nil, ErrOAuthGatewayEncryption
	}
	key := sha256.Sum256(append([]byte("codex2api/oauth-gateway/state/v1\x00"), root...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, ErrOAuthGatewayState
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrOAuthGatewayState
	}
	s := &OAuthGatewayStore{db: db, cipher: aead, owner: uuid.NewString(), state: service.State{Version: 1, Jobs: []service.Job{}}}
	initial, err := s.seal(s.state)
	if err != nil {
		return nil, err
	}
	err = db.withOAuthGatewayWrite(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS oauth_gateway_state (
			id INTEGER PRIMARY KEY CHECK (id=1), payload TEXT NOT NULL,
			revision BIGINT NOT NULL DEFAULT 0, owner TEXT NOT NULL DEFAULT '', lease_until BIGINT NOT NULL DEFAULT 0
		)`); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_gateway_state (id,payload) VALUES (1,$1) ON CONFLICT (id) DO NOTHING`, initial); err != nil {
			return err
		}
		query := `SELECT payload,revision,owner,lease_until FROM oauth_gateway_state WHERE id=1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var payload, owner string
		var until int64
		if err := tx.QueryRowContext(ctx, query).Scan(&payload, &s.revision, &owner, &until); err != nil {
			return err
		}
		now := time.Now().Unix()
		if owner != "" && until > now {
			return ErrOAuthGatewayOwner
		}
		if err := s.unseal(payload, &s.state); err != nil {
			return err
		}
		s.leaseUntil = now + oauthGatewayLeaseSeconds
		result, err := tx.ExecContext(ctx, `UPDATE oauth_gateway_state SET owner=$1,lease_until=$2 WHERE id=1 AND revision=$3 AND (owner='' OR lease_until <= $4)`, s.owner, s.leaseUntil, s.revision, now)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrOAuthGatewayOwner
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *OAuthGatewayStore) seal(state service.State) (string, error) {
	raw, err := json.Marshal(state)
	if err != nil || len(raw) > oauthGatewayStateLimit {
		return "", ErrOAuthGatewayState
	}
	nonce := make([]byte, s.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", ErrOAuthGatewayState
	}
	sealed := s.cipher.Seal(nonce, nonce, raw, []byte("oauth_gateway_state:1:v1"))
	return "ogw:v1:" + base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *OAuthGatewayStore) unseal(payload string, state *service.State) error {
	const prefix = "ogw:v1:"
	if len(payload) < len(prefix) || payload[:len(prefix)] != prefix || len(payload) > (oauthGatewayStateLimit+64)*4/3+16 {
		return ErrOAuthGatewayState
	}
	raw, err := base64.StdEncoding.DecodeString(payload[len(prefix):])
	if err != nil || len(raw) < s.cipher.NonceSize() {
		return ErrOAuthGatewayState
	}
	n := s.cipher.NonceSize()
	plain, err := s.cipher.Open(nil, raw[:n], raw[n:], []byte("oauth_gateway_state:1:v1"))
	if err != nil || len(plain) > oauthGatewayStateLimit || json.Unmarshal(plain, state) != nil {
		return ErrOAuthGatewayState
	}
	return nil
}

func cloneOAuthGatewayState(state service.State) (service.State, error) {
	var out service.State
	raw, err := json.Marshal(state)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

func (s *OAuthGatewayStore) ready() error {
	if s.closed || s.poisoned {
		return ErrOAuthGatewayState
	}
	return nil
}

// Every worker tick calls View, renewing at most once per 30 seconds. Any lost
// or uncertain lease stops this store rather than continuing from stale memory.
func (s *OAuthGatewayStore) View() (service.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return service.State{}, err
	}
	now := time.Now().Unix()
	if s.leaseUntil-now <= oauthGatewayLeaseSeconds-30 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		until := now + oauthGatewayLeaseSeconds
		err := s.db.withOAuthGatewayWrite(ctx, func(conn *sql.Conn) error {
			result, err := conn.ExecContext(ctx, `UPDATE oauth_gateway_state SET lease_until=$1 WHERE id=1 AND owner=$2 AND revision=$3 AND lease_until>$4`, until, s.owner, s.revision, now)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				return ErrOAuthGatewayOwner
			}
			return nil
		})
		if err != nil {
			s.poisoned = true
			return service.State{}, ErrOAuthGatewayOwner
		}
		s.leaseUntil = until
	}
	return cloneOAuthGatewayState(s.state)
}

func (s *OAuthGatewayStore) Update(change func(*service.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	next, err := cloneOAuthGatewayState(s.state)
	if err != nil {
		return ErrOAuthGatewayState
	}
	if err := change(&next); err != nil {
		return err
	}
	payload, err := s.seal(next)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now().Unix()
	until := now + oauthGatewayLeaseSeconds
	err = s.db.withOAuthGatewayWrite(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE oauth_gateway_state SET payload=$1,revision=revision+1,lease_until=$2 WHERE id=1 AND owner=$3 AND revision=$4 AND lease_until>$5`, payload, until, s.owner, s.revision, now)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return ErrOAuthGatewayOwner
		}
		return nil
	})
	if err != nil {
		s.poisoned = true
		return ErrOAuthGatewayState
	}
	s.state, s.revision, s.leaseUntil = next, s.revision+1, until
	return nil
}

func (s *OAuthGatewayStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.db.withSQLiteWriteLock(ctx, func() error {
		_, err := s.db.conn.ExecContext(ctx, `UPDATE oauth_gateway_state SET owner='',lease_until=0 WHERE id=1 AND owner=$1`, s.owner)
		return err
	})
}

var _ service.Store = (*OAuthGatewayStore)(nil)

// InsertOAuthGatewayAccount commits the new account and chosen groups together,
// before the caller publishes it into the runtime scheduler.
func (db *DB) InsertOAuthGatewayAccount(ctx context.Context, name string, credentials map[string]interface{}, proxyURL string, groupIDs []int64) (int64, error) {
	credJSON, err := json.Marshal(encryptSensitiveCredentials(credentials))
	if err != nil {
		return 0, err
	}
	return db.insertAccountRowWithFamilyGroups(ctx,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3) RETURNING id`,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3)`,
		credentials, normalizeIDSlice(groupIDs), name, credJSON, proxyURL,
	)
}

// NORMAL WAL commits can be lost on power failure. Pin a connection and use
// FULL for checkpoints that must precede OAuth submission or account insertion.
func (db *DB) withOAuthGatewayWrite(ctx context.Context, fn func(*sql.Conn) error) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		conn, err := db.conn.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		if db.isSQLite() {
			if _, err := conn.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
				return err
			}
		}
		return fn(conn)
	})
}
