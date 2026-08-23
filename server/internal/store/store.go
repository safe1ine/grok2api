package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type AccountRecord struct {
	ID            int64      `json:"id"`
	Email         string     `json:"email"`
	Subject       string     `json:"subject"`
	Status        string     `json:"status"`
	CooldownUntil *time.Time `json:"cooldown_until"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
}

// PoolAccount 载入账号池用（含解密后的 refresh_token）。
type PoolAccount struct {
	ID           int64
	Email        string
	Status       string
	RefreshToken string
}

type KeyRecord struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	KeyHash   string    `json:"-"`
	Prefix    string    `json:"prefix"`
	Revoked   bool      `json:"revoked"`
	CreatedAt time.Time `json:"created_at"`
}

type CallLog struct {
	ID               int64      `json:"id"`
	KeyID            *int64     `json:"key_id"`
	AccountID        *int64     `json:"account_id"`
	Model            string     `json:"model"`
	Endpoint         string     `json:"endpoint"`
	Status           int        `json:"status"`
	PromptTokens     int        `json:"prompt_tokens"`
	CompletionTokens int        `json:"completion_tokens"`
	LatencyMs        int        `json:"latency_ms"`
	CreatedAt        time.Time  `json:"created_at"`
	KeyName          string     `json:"key_name"`
	AccountEmail     string     `json:"account_email"`
}

type Store struct {
	pool *pgxpool.Pool
	enc  *encryptor
}

func New(ctx context.Context, dbURL string, encKey []byte) (*Store, error) {
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("数据库 ping 失败: %w", err)
	}
	enc, err := newEncryptor(encKey)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{pool: pool, enc: enc}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("执行迁移 %s 失败: %w", name, err)
		}
	}
	return nil
}

// ---------- 账号 ----------

func (s *Store) ListAccounts(ctx context.Context) ([]AccountRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, subject, status, cooldown_until, created_at, updated_at, last_used_at
		FROM accounts ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AccountRecord, 0)
	for rows.Next() {
		var a AccountRecord
		var email, subject *string
		if err := rows.Scan(&a.ID, &email, &subject, &a.Status, &a.CooldownUntil, &a.CreatedAt, &a.UpdatedAt, &a.LastUsedAt); err != nil {
			return nil, err
		}
		if email != nil {
			a.Email = *email
		}
		if subject != nil {
			a.Subject = *subject
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListPoolAccounts(ctx context.Context) ([]PoolAccount, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, email, status, refresh_token_enc FROM accounts WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PoolAccount
	for rows.Next() {
		var p PoolAccount
		var email *string
		var enc []byte
		if err := rows.Scan(&p.ID, &email, &p.Status, &enc); err != nil {
			return nil, err
		}
		if email != nil {
			p.Email = *email
		}
		plain, err := s.enc.Decrypt(enc)
		if err != nil {
			return nil, fmt.Errorf("账号 %d 的 refresh_token 解密失败: %w", p.ID, err)
		}
		p.RefreshToken = string(plain)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CreateAccount(ctx context.Context, email, subject, refreshToken string) (int64, error) {
	enc, err := s.enc.Encrypt([]byte(refreshToken))
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO accounts (email, subject, refresh_token_enc, status)
		VALUES ($1, $2, $3, 'active')
		ON CONFLICT (email) DO UPDATE SET
		    subject = EXCLUDED.subject,
		    refresh_token_enc = EXCLUDED.refresh_token_enc,
		    status = 'active',
		    updated_at = now()
		RETURNING id`, email, subject, enc).Scan(&id)
	return id, err
}

func (s *Store) UpdateRefreshToken(ctx context.Context, id int64, refreshToken string) error {
	enc, err := s.enc.Encrypt([]byte(refreshToken))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE accounts SET refresh_token_enc = $1, updated_at = now() WHERE id = $2`, enc, id)
	return err
}

func (s *Store) SetAccountStatus(ctx context.Context, id int64, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE accounts SET status = $1, updated_at = now() WHERE id = $2`, status, id)
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id)
	return err
}

func (s *Store) TouchLastUsed(ctx context.Context, id int64) {
	_, _ = s.pool.Exec(ctx, `UPDATE accounts SET last_used_at = now() WHERE id = $1`, id)
}

// ---------- OAuth state ----------

func (s *Store) CreateOAuthState(ctx context.Context, state, codeVerifier string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_states (state, code_verifier, expires_at)
		VALUES ($1, $2, now() + $3::interval)`, state, codeVerifier, ttl.String())
	return err
}

func (s *Store) ConsumeOAuthState(ctx context.Context, state string) (string, error) {
	var verifier string
	err := s.pool.QueryRow(ctx, `
		DELETE FROM oauth_states WHERE state = $1 AND expires_at > now() RETURNING code_verifier`, state).Scan(&verifier)
	return verifier, err
}

// CleanupExpiredOAuthStates 清理过期的授权状态。
func (s *Store) CleanupExpiredOAuthStates(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM oauth_states WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------- API Key ----------

func (s *Store) CreateKey(ctx context.Context, name, keyHash, prefix string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (name, key_hash, prefix) VALUES ($1, $2, $3) RETURNING id`,
		name, keyHash, prefix).Scan(&id)
	return id, err
}

func (s *Store) ListKeys(ctx context.Context) ([]KeyRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, key_hash, prefix, revoked, created_at FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]KeyRecord, 0)
	for rows.Next() {
		var k KeyRecord
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Prefix, &k.Revoked, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) DeleteKey(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	return err
}

// GetKeyByHash 供鉴权使用。
func (s *Store) GetKeyByHash(ctx context.Context, hash string) (*KeyRecord, error) {
	var k KeyRecord
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, key_hash, prefix, revoked, created_at
		FROM api_keys WHERE key_hash = $1`, hash).Scan(&k.ID, &k.Name, &k.KeyHash, &k.Prefix, &k.Revoked, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// ListActiveKeyHashes 返回 hash -> keyID，供内存缓存。
func (s *Store) ListActiveKeyHashes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, key_hash FROM api_keys WHERE revoked = false`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int64{}
	for rows.Next() {
		var id int64
		var hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		m[hash] = id
	}
	return m, rows.Err()
}

// ---------- 调用记录 ----------

func (s *Store) InsertCallLog(ctx context.Context, l CallLog) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO call_logs (key_id, account_id, model, endpoint, status, prompt_tokens, completion_tokens, latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		l.KeyID, l.AccountID, l.Model, l.Endpoint, l.Status, l.PromptTokens, l.CompletionTokens, l.LatencyMs)
	return err
}

func (s *Store) ListCallLogs(ctx context.Context, limit, offset int) ([]CallLog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, l.key_id, l.account_id, l.model, l.endpoint, l.status,
		       l.prompt_tokens, l.completion_tokens, l.latency_ms, l.created_at,
		       k.name, a.email
		FROM call_logs l
		LEFT JOIN api_keys k ON k.id = l.key_id
		LEFT JOIN accounts a ON a.id = l.account_id
		ORDER BY l.id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CallLog, 0)
	for rows.Next() {
		var l CallLog
		var model, endpoint, keyName, accountEmail *string
		if err := rows.Scan(&l.ID, &l.KeyID, &l.AccountID, &model, &endpoint, &l.Status,
			&l.PromptTokens, &l.CompletionTokens, &l.LatencyMs, &l.CreatedAt, &keyName, &accountEmail); err != nil {
			return nil, err
		}
		if model != nil {
			l.Model = *model
		}
		if endpoint != nil {
			l.Endpoint = *endpoint
		}
		if keyName != nil {
			l.KeyName = *keyName
		}
		if accountEmail != nil {
			l.AccountEmail = *accountEmail
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
