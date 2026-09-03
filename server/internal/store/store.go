package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type AccountRecord struct {
	ID                 int64      `json:"id"`
	Email              string     `json:"email"`
	Subject            string     `json:"subject"`
	Status             string     `json:"status"`
	CooldownUntil      *time.Time `json:"cooldown_until"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	LastUsedAt         *time.Time `json:"last_used_at"`
	SchedulingDisabled bool       `json:"scheduling_disabled"`
	SchedulingWeight   int        `json:"scheduling_weight"`
}

// PoolAccount 载入账号池用（含解密后的 refresh_token）。
type PoolAccount struct {
	ID                 int64
	Email              string
	Status             string
	RefreshToken       string
	SchedulingDisabled bool
	SchedulingWeight   int
}

type KeyRecord struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	KeyHash         string    `json:"-"`
	Prefix          string    `json:"prefix"`
	Revoked         bool      `json:"revoked"`
	HistoricalCalls int64     `json:"historical_calls"`
	TodayCalls      int64     `json:"today_calls"`
	CreatedAt       time.Time `json:"created_at"`
}

type UsageKeyOption struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

type MinuteUsage struct {
	Minute                      time.Time
	Model                       string
	Calls                       int64
	PromptTokens                int64
	CachedTokens                int64
	CompletionTokens            int64
	LongContextPromptTokens     int64
	LongContextCachedTokens     int64
	LongContextCompletionTokens int64
}

type CallLog struct {
	ID               int64     `json:"id"`
	KeyID            *int64    `json:"key_id"`
	AccountID        *int64    `json:"account_id"`
	Model            string    `json:"model"`
	Endpoint         string    `json:"endpoint"`
	Status           int       `json:"status"`
	ErrorReason      string    `json:"error_reason"`
	PromptTokens     int       `json:"prompt_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TTFTMs           int       `json:"ttft_ms"`
	LatencyMs        int       `json:"latency_ms"`
	Stream           bool      `json:"stream"`
	CreatedAt        time.Time `json:"created_at"`
	KeyName          string    `json:"key_name"`
	AccountEmail     string    `json:"account_email"`
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
	if err := s.EnsureCallLogPartitions(ctx, time.Now()); err != nil {
		pool.Close()
		return nil, fmt.Errorf("创建调用记录分区失败: %w", err)
	}
	return s, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

// EnsureCallLogPartitions 提前创建当天及未来 7 天的调用记录分区。
// 分区边界使用 Asia/Shanghai 自然日，表名格式为 call_logs_YYYYMMDD。
func (s *Store) EnsureCallLogPartitions(ctx context.Context, now time.Time) error {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return fmt.Errorf("加载分区时区失败: %w", err)
	}
	now = now.In(loc)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	for offset := 0; offset <= 7; offset++ {
		start := day.AddDate(0, 0, offset)
		end := start.AddDate(0, 0, 1)
		name := "call_logs_" + start.Format("20060102")
		query := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF call_logs FOR VALUES FROM ('%s') TO ('%s')",
			pgx.Identifier{name}.Sanitize(), start.Format(time.RFC3339), end.Format(time.RFC3339),
		)
		if _, err := s.pool.Exec(ctx, query); err != nil {
			return fmt.Errorf("创建分区 %s 失败: %w", name, err)
		}
	}
	return nil
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
		SELECT id, email, subject, status, cooldown_until, created_at, updated_at, last_used_at,
		       scheduling_disabled, scheduling_weight
		FROM accounts ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AccountRecord, 0)
	for rows.Next() {
		var a AccountRecord
		var email, subject *string
		if err := rows.Scan(
			&a.ID, &email, &subject, &a.Status, &a.CooldownUntil,
			&a.CreatedAt, &a.UpdatedAt, &a.LastUsedAt, &a.SchedulingDisabled, &a.SchedulingWeight,
		); err != nil {
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
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, status, refresh_token_enc, scheduling_disabled, scheduling_weight
		FROM accounts
		WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PoolAccount
	for rows.Next() {
		var p PoolAccount
		var email *string
		var enc []byte
		if err := rows.Scan(&p.ID, &email, &p.Status, &enc, &p.SchedulingDisabled, &p.SchedulingWeight); err != nil {
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

func (s *Store) CreateAccount(ctx context.Context, email, subject, refreshToken string) (int64, int, error) {
	enc, err := s.enc.Encrypt([]byte(refreshToken))
	if err != nil {
		return 0, 0, err
	}
	var id int64
	var weight int
	err = s.pool.QueryRow(ctx, `
		INSERT INTO accounts (email, subject, refresh_token_enc, status)
		VALUES ($1, $2, $3, 'active')
		ON CONFLICT (email) DO UPDATE SET
		    subject = EXCLUDED.subject,
		    refresh_token_enc = EXCLUDED.refresh_token_enc,
		    status = 'active',
		    scheduling_disabled = false,
		    updated_at = now()
		RETURNING id, scheduling_weight`, email, subject, enc).Scan(&id, &weight)
	return id, weight, err
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

func (s *Store) SetAccountSchedulingDisabled(ctx context.Context, id int64, disabled bool) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE accounts
		SET scheduling_disabled = $2, updated_at = now()
		WHERE id = $1`, id, disabled)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) SetAccountSchedulingWeight(ctx context.Context, id int64, weight int) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE accounts
		SET scheduling_weight = $2, updated_at = now()
		WHERE id = $1`, id, weight)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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
		SELECT k.id, k.name, k.key_hash, k.prefix, k.revoked,
		       COALESCE(usage.historical_calls, 0), COALESCE(usage.today_calls, 0),
		       k.created_at
		FROM api_keys k
		LEFT JOIN LATERAL (
			SELECT sum(s.calls) AS historical_calls,
			       sum(s.calls) FILTER (
				       WHERE s.minute >= date_trunc('day', now() AT TIME ZONE 'Asia/Shanghai') AT TIME ZONE 'Asia/Shanghai'
				         AND s.minute < (date_trunc('day', now() AT TIME ZONE 'Asia/Shanghai') + interval '1 day') AT TIME ZONE 'Asia/Shanghai'
			       ) AS today_calls
			FROM minute_usage_stats s
			WHERE s.key_id = k.id
		) usage ON true
		ORDER BY k.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]KeyRecord, 0)
	for rows.Next() {
		var k KeyRecord
		if err := rows.Scan(
			&k.ID, &k.Name, &k.KeyHash, &k.Prefix, &k.Revoked,
			&k.HistoricalCalls, &k.TodayCalls, &k.CreatedAt,
		); err != nil {
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
	// 原始日志和分钟统计在同一条 SQL 中提交，任一写入失败都会整体回滚。
	_, err := s.pool.Exec(ctx, `
		WITH inserted AS (
			INSERT INTO call_logs (
				key_id, account_id, model, endpoint, status, error_reason, prompt_tokens, cached_tokens,
				completion_tokens, ttft_ms, latency_ms, stream
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING created_at, key_id, model, prompt_tokens, cached_tokens, completion_tokens
		)
		INSERT INTO minute_usage_stats (
			minute, key_id, model_name, calls,
			input_tokens, cached_tokens, output_tokens,
			long_context_input_tokens, long_context_cached_tokens, long_context_output_tokens
		)
		SELECT
			date_trunc('minute', created_at), COALESCE(key_id, 0), COALESCE(model, ''), 1,
			prompt_tokens, cached_tokens, completion_tokens,
			CASE WHEN prompt_tokens > 200000 THEN prompt_tokens ELSE 0 END,
			CASE WHEN prompt_tokens > 200000 THEN cached_tokens ELSE 0 END,
			CASE WHEN prompt_tokens > 200000 THEN completion_tokens ELSE 0 END
		FROM inserted
		ON CONFLICT (minute, key_id, model_name) DO UPDATE SET
			calls = minute_usage_stats.calls + EXCLUDED.calls,
			input_tokens = minute_usage_stats.input_tokens + EXCLUDED.input_tokens,
			cached_tokens = minute_usage_stats.cached_tokens + EXCLUDED.cached_tokens,
			output_tokens = minute_usage_stats.output_tokens + EXCLUDED.output_tokens,
			long_context_input_tokens = minute_usage_stats.long_context_input_tokens + EXCLUDED.long_context_input_tokens,
			long_context_cached_tokens = minute_usage_stats.long_context_cached_tokens + EXCLUDED.long_context_cached_tokens,
			long_context_output_tokens = minute_usage_stats.long_context_output_tokens + EXCLUDED.long_context_output_tokens,
			updated_at = now()`,
		l.KeyID, l.AccountID, l.Model, l.Endpoint, l.Status, l.ErrorReason, l.PromptTokens, l.CachedTokens,
		l.CompletionTokens, l.TTFTMs, l.LatencyMs, l.Stream)
	return err
}

func (s *Store) CountCallLogs(ctx context.Context) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM call_logs`).Scan(&total)
	return total, err
}

func (s *Store) ListMinuteUsage(
	ctx context.Context,
	start, end time.Time,
	model string,
	keyID *int64,
) ([]MinuteUsage, error) {
	var keyFilter any
	if keyID != nil {
		keyFilter = *keyID
	}
	rows, err := s.pool.Query(ctx, `
		SELECT minute, model_name,
		       sum(calls), sum(input_tokens), sum(cached_tokens), sum(output_tokens),
		       sum(long_context_input_tokens), sum(long_context_cached_tokens),
		       sum(long_context_output_tokens)
		FROM minute_usage_stats
		WHERE minute >= $1 AND minute < $2
		  AND ($3 = '' OR model_name = $3)
		  AND ($4::bigint IS NULL OR key_id = $4)
		GROUP BY minute, model_name
		ORDER BY minute`, start, end, model, keyFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]MinuteUsage, 0)
	for rows.Next() {
		var usage MinuteUsage
		if err := rows.Scan(
			&usage.Minute, &usage.Model, &usage.Calls,
			&usage.PromptTokens, &usage.CachedTokens, &usage.CompletionTokens,
			&usage.LongContextPromptTokens, &usage.LongContextCachedTokens,
			&usage.LongContextCompletionTokens,
		); err != nil {
			return nil, err
		}
		out = append(out, usage)
	}
	return out, rows.Err()
}

func (s *Store) ListUsageModels(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT model_name
		FROM minute_usage_stats
		WHERE model_name <> ''
		ORDER BY model_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	models := make([]string, 0)
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, rows.Err()
}

func (s *Store) ListUsageKeyOptions(ctx context.Context) ([]UsageKeyOption, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, prefix
		FROM api_keys
		ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make([]UsageKeyOption, 0)
	for rows.Next() {
		var key UsageKeyOption
		if err := rows.Scan(&key.ID, &key.Name, &key.Prefix); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) ListCallLogs(ctx context.Context, limit, offset int) ([]CallLog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, l.key_id, l.account_id, l.model, l.endpoint, l.status, l.error_reason,
		       l.prompt_tokens, l.cached_tokens, l.completion_tokens,
		       l.ttft_ms, l.latency_ms, l.stream, l.created_at, k.name, a.email
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
		if err := rows.Scan(&l.ID, &l.KeyID, &l.AccountID, &model, &endpoint, &l.Status, &l.ErrorReason,
			&l.PromptTokens, &l.CachedTokens, &l.CompletionTokens, &l.TTFTMs, &l.LatencyMs,
			&l.Stream, &l.CreatedAt, &keyName, &accountEmail); err != nil {
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
