package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"akkvstore/internal/auth"
)

const maxTokenNameBytes = 80

// ListProjectTokens 返回 project 的全部 Token 及其统计，不包含明文。
func (s *Store) ListProjectTokens(name string) ([]ProjectToken, error) {
	if !ValidateProjectName(name) {
		return nil, ErrNotFound
	}
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
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

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	p, err := s.getProjectLocked(name)
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	p, err := s.getProjectLocked(name)
	if err != nil {
		return err
	}
	tx, err := s.systemDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	return tx.Commit()
}

// VerifyProjectToken 校验 project 名称与 token，并返回匹配的 Token 统计记录。
func (s *Store) VerifyProjectToken(name, token string) (Project, ProjectToken, error) {
	if !ValidateProjectName(name) || token == "" {
		return Project{}, ProjectToken{}, ErrUnauthorized
	}
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	p, err := s.getProjectLocked(name)
	if errors.Is(err, ErrNotFound) {
		return Project{}, ProjectToken{}, ErrUnauthorized
	}
	if err != nil {
		return Project{}, ProjectToken{}, err
	}
	rows, err := s.systemDB.Query(`SELECT id, project_id, name, token_hash,
		request_count, read_count, write_count, created_at, last_used_at
		FROM project_tokens WHERE project_id = ? ORDER BY id`, p.ID)
	if err != nil {
		return Project{}, ProjectToken{}, err
	}
	defer rows.Close()
	var matched ProjectToken
	found := false
	for rows.Next() {
		tokenRow, err := scanProjectTokenAuth(rows)
		if err != nil {
			return Project{}, ProjectToken{}, err
		}
		if auth.VerifyToken(token, tokenRow.TokenHash) {
			matched = tokenRow
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return Project{}, ProjectToken{}, err
	}
	if !found {
		return Project{}, ProjectToken{}, ErrUnauthorized
	}
	return p, matched, nil
}

// RecordTokenAccess 记录一次认证请求；读取类请求同时增加 read_count。
func (s *Store) RecordTokenAccess(projectID, tokenID int64, read bool) error {
	if projectID <= 0 || tokenID <= 0 {
		return nil
	}
	now := time.Now().Unix()
	readDelta := 0
	if read {
		readDelta = 1
	}
	tx, err := s.systemDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE project_tokens SET
		request_count = request_count + 1,
		read_count = read_count + ?,
		last_used_at = ?
		WHERE id = ? AND project_id = ?`, readDelta, now, tokenID, projectID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO project_usage(project_id, request_count)
		VALUES(?, 1)
		ON CONFLICT(project_id) DO UPDATE SET
			request_count = request_count + 1`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO project_token_access(project_id, token_id, second_start, request_count)
		VALUES(?, ?, ?, 1)
		ON CONFLICT(project_id, token_id, second_start) DO UPDATE SET
			request_count = request_count + 1`, projectID, tokenID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.cleanupTokenAccess(now)
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
	_, _ = s.systemDB.Exec(`DELETE FROM project_token_access WHERE second_start < ?`, now-600)
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
	_, err := s.systemDB.Exec(`UPDATE project_tokens
		SET write_count = write_count + ? WHERE id = ?`, count, tokenID)
	return err
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
