package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"akkvstore/internal/auth"
)

const maxTokenNameBytes = 80

type tokenAccessDelta struct {
	projectID  int64
	tokenID    int64
	requests   int64
	reads      int64
	lastUsedAt int64
	seconds    map[int64]int64
}

type authCacheKey struct {
	project   string
	tokenHash string
}

type authCacheEntry struct {
	project   Project
	token     ProjectToken
	expiresAt time.Time
}

// ListProjectTokens 返回 project 的全部 Token 及其统计，不包含明文。
func (s *Store) ListProjectTokens(name string) ([]ProjectToken, error) {
	if !ValidateProjectName(name) {
		return nil, ErrNotFound
	}
	if err := s.flushTokenStats(); err != nil {
		return nil, err
	}
	p, err := s.getProjectLocked(name)
	if err != nil {
		return nil, err
	}
	rows, err := s.systemDB.Query(`SELECT id, project_id, name, token, token_hash,
		request_count, read_count, write_count, created_at, last_used_at
		FROM project_tokens WHERE project_id = ? ORDER BY id`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []ProjectToken
	for rows.Next() {
		token, err := scanProjectToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tokens, nil
}

// CreateProjectToken 为 project 创建一个新的访问 Token。
func (s *Store) CreateProjectToken(name, tokenName string) (ProjectToken, string, error) {
	if !ValidateProjectName(name) {
		return ProjectToken{}, "", ErrNotFound
	}
	customName, err := normalizeTokenName(tokenName)
	if err != nil {
		return ProjectToken{}, "", err
	}

	token, err := auth.GenerateToken()
	if err != nil {
		return ProjectToken{}, "", err
	}
	now := time.Now().Unix()
	tx, err := s.systemDB.Begin()
	if err != nil {
		return ProjectToken{}, "", err
	}
	defer tx.Rollback()
	p, err := getProjectTx(tx, name)
	if err != nil {
		return ProjectToken{}, "", err
	}

	if customName == "" {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM project_tokens WHERE project_id = ?`,
			p.ID).Scan(&count); err != nil {
			return ProjectToken{}, "", err
		}
		customName = fmt.Sprintf("Token %d", count+1)
	}
	tokenHash := auth.HashToken(token)
	res, err := tx.Exec(`INSERT INTO project_tokens(project_id, name, token, token_hash, created_at)
		VALUES(?, ?, ?, ?, ?)`, p.ID, customName, token, tokenHash, now)
	if err != nil {
		return ProjectToken{}, "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ProjectToken{}, "", err
	}
	if _, err := tx.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`, now, p.ID); err != nil {
		return ProjectToken{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return ProjectToken{}, "", err
	}
	return ProjectToken{
		ID:          id,
		ProjectID:   p.ID,
		Name:        customName,
		Token:       token,
		TokenHash:   tokenHash,
		Fingerprint: tokenFingerprint(tokenHash),
		CreatedAt:   now,
	}, token, nil
}

// DeleteProjectToken 删除 project 下的一个 Token。
func (s *Store) DeleteProjectToken(name string, tokenID int64) error {
	if !ValidateProjectName(name) || tokenID <= 0 {
		return ErrNotFound
	}
	if err := s.flushTokenStats(); err != nil {
		return err
	}
	tx, err := s.systemDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := getProjectTx(tx, name)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM project_tokens WHERE id = ? AND project_id = ?`, tokenID, p.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`,
		time.Now().Unix(), p.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.clearAuthCache()
	return nil
}

// VerifyProjectToken 校验 project 名称与 token，并返回匹配的 Token 统计记录。
func (s *Store) VerifyProjectToken(name, token string) (Project, ProjectToken, error) {
	if !ValidateProjectName(name) || token == "" {
		return Project{}, ProjectToken{}, ErrUnauthorized
	}
	tokenHash := auth.HashToken(token)
	cacheKey := authCacheKey{project: strings.ToLower(name), tokenHash: tokenHash}
	now := time.Now()

	s.authCacheMu.RLock()
	entry, cached := s.authCache[cacheKey]
	generation := s.authCacheGeneration
	s.authCacheMu.RUnlock()
	if cached && now.Before(entry.expiresAt) {
		return entry.project, entry.token, nil
	}
	if cached {
		s.authCacheMu.Lock()
		if s.authCacheGeneration == generation {
			delete(s.authCache, cacheKey)
		}
		s.authCacheMu.Unlock()
	}

	p, err := s.getProjectLocked(name)
	if errors.Is(err, ErrNotFound) {
		return Project{}, ProjectToken{}, ErrUnauthorized
	}
	if err != nil {
		return Project{}, ProjectToken{}, err
	}
	row := s.systemDB.QueryRow(`SELECT id, project_id, name, token_hash,
		request_count, read_count, write_count, created_at, last_used_at
		FROM project_tokens WHERE project_id = ? AND token_hash = ?`, p.ID, tokenHash)
	matched, err := scanProjectTokenAuth(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, ProjectToken{}, ErrUnauthorized
		}
		return Project{}, ProjectToken{}, err
	}
	if !auth.VerifyToken(token, matched.TokenHash) {
		return Project{}, ProjectToken{}, ErrUnauthorized
	}

	s.authCacheMu.Lock()
	if s.authCacheGeneration == generation {
		if len(s.authCache) >= authCacheLimit {
			s.authCache = make(map[authCacheKey]authCacheEntry)
		}
		s.authCache[cacheKey] = authCacheEntry{
			project:   p,
			token:     matched,
			expiresAt: now.Add(authCacheTTL),
		}
	}
	s.authCacheMu.Unlock()
	return p, matched, nil
}

func (s *Store) clearAuthCache() {
	s.authCacheMu.Lock()
	s.authCacheGeneration++
	s.authCache = make(map[authCacheKey]authCacheEntry)
	s.authCacheMu.Unlock()
}

// RecordTokenAccess 记录一次认证请求；读取类请求同时增加 read_count。
func (s *Store) RecordTokenAccess(projectID, tokenID int64, read bool) error {
	if projectID <= 0 || tokenID <= 0 {
		return nil
	}
	now := time.Now().Unix()

	s.tokenStatsMu.Lock()
	delta := s.pendingAccess[tokenID]
	if delta == nil {
		delta = &tokenAccessDelta{
			projectID: projectID,
			tokenID:   tokenID,
			seconds:   make(map[int64]int64),
		}
		s.pendingAccess[tokenID] = delta
	}
	delta.requests++
	if read {
		delta.reads++
	}
	delta.lastUsedAt = now
	delta.seconds[now]++
	s.tokenStatsMu.Unlock()
	return nil
}

func (s *Store) cleanupTokenAccess(now int64) {
	last := s.lastUsageCleanup.Load()
	if last != 0 && now-last < 60 {
		return
	}
	if !s.lastUsageCleanup.CompareAndSwap(last, now) {
		return
	}
	_, _ = s.systemDB.Exec(`DELETE FROM project_token_access WHERE second_start < ?`,
		now-tokenStatsAccessRetentionSec)
}

// RecordTokenWrite 记录一次成功的写入或删除请求。
func (s *Store) RecordTokenWrite(tokenID int64) error {
	return s.RecordTokenWrites(tokenID, 1)
}

// RecordTokenWrites 为一次批量请求累计成功修改次数。
func (s *Store) RecordTokenWrites(tokenID int64, count int64) error {
	if tokenID <= 0 || count <= 0 {
		return nil
	}
	s.tokenStatsMu.Lock()
	s.pendingWrites[tokenID] += count
	s.tokenStatsMu.Unlock()
	return nil
}

func (s *Store) flushTokenStats() error {
	s.tokenStatsFlushMu.Lock()
	defer s.tokenStatsFlushMu.Unlock()

	s.tokenStatsMu.Lock()
	access := s.pendingAccess
	writes := s.pendingWrites
	s.pendingAccess = make(map[int64]*tokenAccessDelta)
	s.pendingWrites = make(map[int64]int64)
	s.tokenStatsMu.Unlock()

	if len(access) == 0 && len(writes) == 0 {
		return nil
	}
	if err := s.writeTokenStats(access, writes); err != nil {
		s.mergeTokenStats(access, writes)
		return err
	}
	s.cleanupTokenAccess(time.Now().Unix())
	return nil
}

func (s *Store) writeTokenStats(access map[int64]*tokenAccessDelta, writes map[int64]int64) error {
	tx, err := s.systemDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, delta := range access {
		res, err := tx.Exec(`UPDATE project_tokens SET
			request_count = request_count + ?,
			read_count = read_count + ?,
			last_used_at = CASE WHEN ? > last_used_at THEN ? ELSE last_used_at END
			WHERE id = ? AND project_id = ?`,
			delta.requests, delta.reads, delta.lastUsedAt, delta.lastUsedAt,
			delta.tokenID, delta.projectID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO project_usage(project_id, request_count)
			VALUES(?, ?)
			ON CONFLICT(project_id) DO UPDATE SET
				request_count = request_count + excluded.request_count`,
			delta.projectID, delta.requests); err != nil {
			return err
		}
		for second, count := range delta.seconds {
			if _, err := tx.Exec(`INSERT INTO project_token_access
				(project_id, token_id, second_start, request_count)
				VALUES(?, ?, ?, ?)
				ON CONFLICT(project_id, token_id, second_start) DO UPDATE SET
					request_count = request_count + excluded.request_count`,
				delta.projectID, delta.tokenID, second, count); err != nil {
				return err
			}
		}
	}
	for tokenID, count := range writes {
		if _, err := tx.Exec(`UPDATE project_tokens
			SET write_count = write_count + ? WHERE id = ?`, count, tokenID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) mergeTokenStats(access map[int64]*tokenAccessDelta, writes map[int64]int64) {
	s.tokenStatsMu.Lock()
	defer s.tokenStatsMu.Unlock()
	for tokenID, delta := range access {
		current := s.pendingAccess[tokenID]
		if current == nil {
			current = &tokenAccessDelta{
				projectID: delta.projectID,
				tokenID:   delta.tokenID,
				seconds:   make(map[int64]int64),
			}
			s.pendingAccess[tokenID] = current
		}
		current.requests += delta.requests
		current.reads += delta.reads
		if delta.lastUsedAt > current.lastUsedAt {
			current.lastUsedAt = delta.lastUsedAt
		}
		for second, count := range delta.seconds {
			current.seconds[second] += count
		}
	}
	for tokenID, count := range writes {
		s.pendingWrites[tokenID] += count
	}
}

func normalizeTokenName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if len(name) > maxTokenNameBytes || !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: Token 名称不能超过 %d 字节且必须是合法 UTF-8", ErrInvalid, maxTokenNameBytes)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: Token 名称不能包含控制字符", ErrInvalid)
		}
	}
	return name, nil
}

func scanProjectToken(row interface{ Scan(...any) error }) (ProjectToken, error) {
	var token ProjectToken
	err := row.Scan(
		&token.ID,
		&token.ProjectID,
		&token.Name,
		&token.Token,
		&token.TokenHash,
		&token.RequestCount,
		&token.ReadCount,
		&token.WriteCount,
		&token.CreatedAt,
		&token.LastUsedAt,
	)
	if err != nil {
		return ProjectToken{}, err
	}
	token.Fingerprint = tokenFingerprint(token.TokenHash)
	return token, nil
}

func scanProjectTokenAuth(row interface{ Scan(...any) error }) (ProjectToken, error) {
	var token ProjectToken
	err := row.Scan(
		&token.ID,
		&token.ProjectID,
		&token.Name,
		&token.TokenHash,
		&token.RequestCount,
		&token.ReadCount,
		&token.WriteCount,
		&token.CreatedAt,
		&token.LastUsedAt,
	)
	if err != nil {
		return ProjectToken{}, err
	}
	token.Fingerprint = tokenFingerprint(token.TokenHash)
	return token, nil
}

func tokenFingerprint(hash string) string {
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return hash
}
