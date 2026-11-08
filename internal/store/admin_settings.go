package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"akkvstore/internal/auth"
)

const (
	AdminLoginModePassword = "password"
	AdminLoginModeBark     = "bark"

	metaAdminPasswordHash    = "admin_password_hash"
	metaAdminPasswordLogin   = "admin_password_login_enabled"
	metaAdminBarkLogin       = "admin_bark_login_enabled"
	metaAdminBarkAddress     = "admin_bark_address"
	metaAdminAuthEpoch       = "admin_auth_epoch"
	metaLegacyAdminLoginMode = "admin_login_mode"
	metaLegacyBarkBaseURL    = "bark_base_url"
	metaLegacyBarkDeviceKey  = "bark_device_key"

	legacyDefaultAdminPassword = "change-this-to-a-long-password"
)

// AdminSettings 是持久化的管理员登录配置。
type AdminSettings struct {
	PasswordHash         string
	PasswordLoginEnabled bool
	BarkLoginEnabled     bool
	BarkAddress          string
	AuthEpoch            int64
}

func (s AdminSettings) BarkConfigured() bool {
	return s.BarkLoginEnabled && s.BarkAddress != ""
}

func IsLegacyDefaultAdminPassword(password string) bool {
	return password == legacyDefaultAdminPassword
}

func validateAdminPasswordHash(passwordHash string) error {
	if err := auth.ValidatePasswordHash(passwordHash); err != nil {
		return err
	}
	if auth.CheckPassword(passwordHash, legacyDefaultAdminPassword) {
		return fmt.Errorf("%w: 不能使用旧版默认占位密码", ErrInvalid)
	}
	return nil
}

// EnsureAdminSettings 初始化管理员配置，并迁移旧版二选一登录模式。
func (s *Store) EnsureAdminSettings() error {
	var passwordHash string
	err := s.systemDB.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaAdminPasswordHash).Scan(&passwordHash)
	needsPasswordReset := errors.Is(err, sql.ErrNoRows) ||
		(err == nil && (passwordHash == "" || auth.CheckPassword(passwordHash, legacyDefaultAdminPassword)))
	if needsPasswordReset {
		tx, err := s.systemDB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`DELETE FROM meta WHERE key = ?`, metaAdminPasswordHash); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES(?, '0')`, metaAdminAuthEpoch); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE meta SET value = CAST(value AS INTEGER) + 1 WHERE key = ?`, metaAdminAuthEpoch); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := s.systemDB.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES(?, '1')`, metaAdminAuthEpoch); err != nil {
		return err
	}

	legacyMode := AdminLoginModePassword
	legacyBaseURL := "https://api.day.app"
	legacyDeviceKey := ""
	if err := s.systemDB.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaLegacyAdminLoginMode).Scan(&legacyMode); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := s.systemDB.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaLegacyBarkBaseURL).Scan(&legacyBaseURL); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := s.systemDB.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaLegacyBarkDeviceKey).Scan(&legacyDeviceKey); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	barkAddress := legacyBarkAddress(legacyBaseURL, legacyDeviceKey)
	passwordEnabled := true
	barkEnabled := legacyMode == AdminLoginModeBark && barkAddress != ""
	settings := []struct {
		key   string
		value string
	}{
		{metaAdminPasswordLogin, boolMeta(passwordEnabled)},
		{metaAdminBarkLogin, boolMeta(barkEnabled)},
		{metaAdminBarkAddress, barkAddress},
	}
	for _, item := range settings {
		key, value := item.key, item.value
		if _, err := s.systemDB.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES(?, ?)`, key, value); err != nil {
			return err
		}
	}

	passwordEnabled, err = s.readAdminLoginBool(metaAdminPasswordLogin)
	if err != nil {
		return err
	}
	barkEnabled, err = s.readAdminLoginBool(metaAdminBarkLogin)
	if err != nil {
		return err
	}
	if !passwordEnabled && !barkEnabled {
		if _, err := s.systemDB.Exec(`UPDATE meta SET value = '1' WHERE key = ?`, metaAdminPasswordLogin); err != nil {
			return err
		}
	}
	if _, err := s.systemDB.Exec(`DELETE FROM meta WHERE key IN (?, ?, ?)`,
		metaLegacyAdminLoginMode, metaLegacyBarkBaseURL, metaLegacyBarkDeviceKey); err != nil {
		return err
	}
	return nil
}

// GetAdminSettings 读取管理员登录配置。
func (s *Store) GetAdminSettings() (AdminSettings, error) {
	var settings AdminSettings
	var passwordHash, passwordEnabledValue, barkEnabledValue, barkAddress, epochValue sql.NullString
	if err := s.systemDB.QueryRow(`SELECT
		(SELECT value FROM meta WHERE key = ?),
		(SELECT value FROM meta WHERE key = ?),
		(SELECT value FROM meta WHERE key = ?),
		(SELECT value FROM meta WHERE key = ?),
		(SELECT value FROM meta WHERE key = ?)`,
		metaAdminPasswordHash,
		metaAdminPasswordLogin,
		metaAdminBarkLogin,
		metaAdminBarkAddress,
		metaAdminAuthEpoch,
	).Scan(
		&passwordHash,
		&passwordEnabledValue,
		&barkEnabledValue,
		&barkAddress,
		&epochValue,
	); err != nil {
		return AdminSettings{}, err
	}
	if !passwordEnabledValue.Valid {
		return AdminSettings{}, fmt.Errorf("admin setting %s is missing", metaAdminPasswordLogin)
	}
	if !barkEnabledValue.Valid {
		return AdminSettings{}, fmt.Errorf("admin setting %s is missing", metaAdminBarkLogin)
	}
	if !barkAddress.Valid {
		return AdminSettings{}, fmt.Errorf("admin setting %s is missing", metaAdminBarkAddress)
	}
	if !epochValue.Valid {
		return AdminSettings{}, fmt.Errorf("admin setting %s is missing", metaAdminAuthEpoch)
	}
	settings.PasswordHash = passwordHash.String
	settings.BarkAddress = barkAddress.String
	passwordEnabled, err := parseBoolMeta(passwordEnabledValue.String)
	if err != nil {
		return AdminSettings{}, fmt.Errorf("invalid %s value %q: %w", metaAdminPasswordLogin, passwordEnabledValue.String, err)
	}
	barkEnabled, err := parseBoolMeta(barkEnabledValue.String)
	if err != nil {
		return AdminSettings{}, fmt.Errorf("invalid %s value %q: %w", metaAdminBarkLogin, barkEnabledValue.String, err)
	}
	settings.PasswordLoginEnabled = passwordEnabled
	settings.BarkLoginEnabled = barkEnabled
	if !settings.PasswordLoginEnabled && !settings.BarkLoginEnabled {
		return AdminSettings{}, errors.New("at least one admin login method must be enabled")
	}
	if settings.BarkLoginEnabled && settings.BarkAddress == "" {
		return AdminSettings{}, errors.New("Bark login is enabled without an address")
	}
	epoch, err := strconv.ParseInt(epochValue.String, 10, 64)
	if err != nil || epoch <= 0 {
		return AdminSettings{}, fmt.Errorf("invalid admin auth epoch %q", epochValue.String)
	}
	settings.AuthEpoch = epoch
	return settings, nil
}

// UpdateAdminPassword 保存新的管理员密码摘要并递增会话纪元。
func (s *Store) UpdateAdminPassword(passwordHash string) (int64, error) {
	if err := validateAdminPasswordHash(passwordHash); err != nil {
		return 0, err
	}
	tx, err := s.systemDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaAdminPasswordHash, passwordHash); err != nil {
		return 0, err
	}
	epoch, err := bumpAdminAuthEpoch(tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return epoch, nil
}

// InitializeAdminPassword 仅在尚未设置管理员密码时写入初始密码。
func (s *Store) InitializeAdminPassword(passwordHash string) (int64, error) {
	if err := validateAdminPasswordHash(passwordHash); err != nil {
		return 0, err
	}
	tx, err := s.systemDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
		WHERE meta.value = ''`, metaAdminPasswordHash, passwordHash)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected == 0 {
		return 0, fmt.Errorf("%w: 管理员初始密码已经设置", ErrInvalid)
	}
	epoch, err := bumpAdminAuthEpoch(tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return epoch, nil
}

// UpdateAdminLoginSettings 保存启用的登录方式和 Bark 地址并递增会话纪元。
func (s *Store) UpdateAdminLoginSettings(passwordEnabled, barkEnabled bool, barkAddress string) (int64, error) {
	if !passwordEnabled && !barkEnabled {
		return 0, fmt.Errorf("%w: 必须至少启用一种登录方式", ErrInvalid)
	}
	barkAddress = strings.TrimRight(strings.TrimSpace(barkAddress), "/")
	if barkEnabled && barkAddress == "" {
		return 0, fmt.Errorf("%w: 启用 Bark 登录前必须填写地址", ErrInvalid)
	}
	tx, err := s.systemDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for key, value := range map[string]string{
		metaAdminPasswordLogin: boolMeta(passwordEnabled),
		metaAdminBarkLogin:     boolMeta(barkEnabled),
		metaAdminBarkAddress:   barkAddress,
	} {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return 0, err
		}
	}
	epoch, err := bumpAdminAuthEpoch(tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return epoch, nil
}

func (s *Store) readAdminLoginBool(key string) (bool, error) {
	var value string
	if err := s.systemDB.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value); err != nil {
		return false, err
	}
	return parseBoolMeta(value)
}

func parseBoolMeta(value string) (bool, error) {
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New("must be 0 or 1")
	}
}

func boolMeta(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func legacyBarkAddress(baseURL, deviceKey string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	deviceKey = strings.TrimSpace(deviceKey)
	if baseURL == "" || deviceKey == "" {
		return ""
	}
	return baseURL + "/" + url.PathEscape(deviceKey)
}

func bumpAdminAuthEpoch(tx *sql.Tx) (int64, error) {
	if _, err := tx.Exec(`UPDATE meta SET value = CAST(value AS INTEGER) + 1 WHERE key = ?`,
		metaAdminAuthEpoch); err != nil {
		return 0, err
	}
	var epoch int64
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaAdminAuthEpoch).Scan(&epoch); err != nil {
		return 0, err
	}
	return epoch, nil
}
