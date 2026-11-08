package store

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"akkvstore/internal/auth"
)

func openTestStore(t *testing.T, maxValue int64, inlineThreshold int64) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	s, err := Open(Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: inlineThreshold,
		MaxValueBytes:   maxValue,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dataDir
}

func mustProject(t *testing.T, s *Store, name string) (Project, string) {
	t.Helper()
	p, token, err := s.CreateProject(name)
	if err != nil {
		t.Fatalf("CreateProject(%q): %v", name, err)
	}
	return p, token
}

func mustTable(t *testing.T, s *Store, p Project, name string) Table {
	t.Helper()
	table, err := s.CreateTable(p, name)
	if err != nil {
		t.Fatalf("CreateTable(%q): %v", name, err)
	}
	return table
}

func mustRead(t *testing.T, rc ReadSeekCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestProjectLifecycleAndToken(t *testing.T) {
	s, dataDir := openTestStore(t, 1<<20, 32)
	p, token := mustProject(t, s, "demo")

	if !strings.HasPrefix(token, "kv_") {
		t.Fatalf("token prefix = %q", token)
	}
	if p.ID != 1 {
		t.Fatalf("project id = %d, want 1", p.ID)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "system.db")); err != nil {
		t.Fatalf("system.db missing: %v", err)
	}
	if _, err := os.Stat(s.projectDBPath(p.ID)); err != nil {
		t.Fatalf("project.db missing: %v", err)
	}
	tables, err := s.ListTables(p)
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("new project tables = %+v, want empty", tables)
	}

	got, defaultToken, err := s.VerifyProjectToken("demo", token)
	if err != nil {
		t.Fatalf("VerifyProjectToken: %v", err)
	}
	if got.ID != p.ID {
		t.Fatalf("project id = %d, want %d", got.ID, p.ID)
	}
	if defaultToken.Name != "默认 Token" || defaultToken.Fingerprint == "" {
		t.Fatalf("default token = %+v", defaultToken)
	}
	if _, _, err := s.VerifyProjectToken("demo", "kv_wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token err = %v, want ErrUnauthorized", err)
	}
	if _, _, err := s.VerifyProjectToken("missing", token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing project err = %v, want ErrUnauthorized", err)
	}

	second, newToken, err := s.CreateProjectToken("demo", "第二 Token")
	if err != nil {
		t.Fatalf("CreateProjectToken: %v", err)
	}
	if newToken == token {
		t.Fatal("new token is unchanged")
	}
	if _, tokenMeta, err := s.VerifyProjectToken("demo", token); err != nil || tokenMeta.ID != defaultToken.ID {
		t.Fatalf("original token after add: meta=%+v err=%v", tokenMeta, err)
	}
	if _, tokenMeta, err := s.VerifyProjectToken("demo", newToken); err != nil || tokenMeta.ID != second.ID {
		t.Fatalf("new token: %v", err)
	}
	if err := s.RecordTokenAccess(p.ID, defaultToken.ID, true); err != nil {
		t.Fatalf("RecordTokenAccess: %v", err)
	}
	if err := s.RecordTokenWrite(defaultToken.ID); err != nil {
		t.Fatalf("RecordTokenWrite: %v", err)
	}
	if err := s.RecordTokenAccess(p.ID, second.ID, false); err != nil {
		t.Fatalf("RecordTokenAccess second: %v", err)
	}
	tokens, err := s.ListProjectTokens("demo")
	if err != nil {
		t.Fatalf("ListProjectTokens: %v", err)
	}
	if len(tokens) != 2 || tokens[0].RequestCount != 1 || tokens[0].ReadCount != 1 ||
		tokens[0].WriteCount != 1 || tokens[1].RequestCount != 1 || tokens[1].ReadCount != 0 {
		t.Fatalf("token stats = %+v", tokens)
	}
	if tokens[0].Token != token || tokens[1].Token != newToken {
		t.Fatalf("stored token plaintext mismatch: %+v", tokens)
	}
	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects with token stats: %v", err)
	}
	var demoSummary ProjectSummary
	for _, project := range projects {
		if project.Name == "demo" {
			demoSummary = project
			break
		}
	}
	if demoSummary.RequestCount != 2 || demoSummary.RecentRequest != 2 {
		t.Fatalf("project token stats = %+v", demoSummary)
	}
	if err := s.DeleteProjectToken("demo", defaultToken.ID); err != nil {
		t.Fatalf("DeleteProjectToken: %v", err)
	}
	if _, _, err := s.VerifyProjectToken("demo", token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("deleted token err = %v, want ErrUnauthorized", err)
	}
	if _, _, err := s.VerifyProjectToken("demo", newToken); err != nil {
		t.Fatalf("remaining token: %v", err)
	}
	if err := s.DeleteProjectToken("demo", second.ID); err != nil {
		t.Fatalf("delete final token: %v", err)
	}
	if _, _, err := s.VerifyProjectToken("demo", newToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("token after delete-all err = %v, want ErrUnauthorized", err)
	}
	projects, err = s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects after token delete-all: %v", err)
	}
	for _, project := range projects {
		if project.Name == "demo" && (project.RequestCount != 2 || project.RecentRequest != 2) {
			t.Fatalf("project usage after token delete-all = %+v", project)
		}
	}
	if _, recoveryToken, err := s.CreateProjectToken("demo", "恢复 Token"); err != nil {
		t.Fatalf("create token after delete-all: %v", err)
	} else if _, _, err := s.VerifyProjectToken("demo", recoveryToken); err != nil {
		t.Fatalf("recovery token: %v", err)
	}
	_, otherToken := mustProject(t, s, "other")
	_, otherTokenMeta, err := s.VerifyProjectToken("other", otherToken)
	if err != nil {
		t.Fatalf("verify other token: %v", err)
	}
	if err := s.DeleteProjectToken("demo", otherTokenMeta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project token delete err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.VerifyProjectToken("other", otherToken); err != nil {
		t.Fatalf("other token after cross-project delete: %v", err)
	}

	if _, _, err := s.CreateProject("demo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate project err = %v, want ErrInvalid", err)
	}
	if _, _, err := s.CreateProject("Bad Name"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid project err = %v, want ErrInvalid", err)
	}
}

func TestAdminSettingsBootstrapAndUpdate(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	oldHash, err := auth.HashPassword("old-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := s.EnsureAdminSettings(); err != nil {
		t.Fatalf("EnsureAdminSettings: %v", err)
	}
	settings, err := s.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings: %v", err)
	}
	if settings.PasswordHash != "" || !settings.PasswordLoginEnabled || settings.BarkLoginEnabled ||
		settings.BarkAddress != "" || settings.AuthEpoch != 1 {
		t.Fatalf("initial admin settings = %+v", settings)
	}
	if _, err := s.systemDB.Exec(`INSERT INTO meta(key, value) VALUES(?, '')`, metaAdminPasswordHash); err != nil {
		t.Fatalf("insert empty password hash: %v", err)
	}
	epoch, err := s.InitializeAdminPassword(oldHash)
	if err != nil {
		t.Fatalf("InitializeAdminPassword: %v", err)
	}
	if epoch != 2 {
		t.Fatalf("initial password epoch = %d, want 2", epoch)
	}
	newHash, err := auth.HashPassword("new-password")
	if err != nil {
		t.Fatalf("HashPassword new: %v", err)
	}
	if _, err := s.InitializeAdminPassword(newHash); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second InitializeAdminPassword err = %v, want ErrInvalid", err)
	}
	epoch, err = s.UpdateAdminPassword(newHash)
	if err != nil {
		t.Fatalf("UpdateAdminPassword: %v", err)
	}
	if epoch != 3 {
		t.Fatalf("password epoch = %d, want 3", epoch)
	}
	epoch, err = s.UpdateAdminLoginSettings(true, true, "https://bark.example.com/device-key")
	if err != nil {
		t.Fatalf("UpdateAdminLoginSettings: %v", err)
	}
	if epoch != 4 {
		t.Fatalf("login settings epoch = %d, want 4", epoch)
	}
	settings, err = s.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings updated: %v", err)
	}
	if settings.PasswordHash != newHash || !settings.PasswordLoginEnabled || !settings.BarkLoginEnabled ||
		settings.BarkAddress != "https://bark.example.com/device-key" || settings.AuthEpoch != 4 {
		t.Fatalf("updated admin settings = %+v", settings)
	}
	if err := s.EnsureAdminSettings(); err != nil {
		t.Fatalf("EnsureAdminSettings second call: %v", err)
	}
	settings, _ = s.GetAdminSettings()
	if settings.PasswordHash != newHash {
		t.Fatal("bootstrap hash overwrote persisted admin password")
	}
	if _, err := s.UpdateAdminLoginSettings(false, false, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("disable all login methods err = %v, want ErrInvalid", err)
	}
	if _, err := s.UpdateAdminLoginSettings(false, true, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Bark login without address err = %v, want ErrInvalid", err)
	}
}

func TestEnsureAdminSettingsRemovesLegacyDefaultPassword(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	if err := s.EnsureAdminSettings(); err != nil {
		t.Fatalf("initial EnsureAdminSettings: %v", err)
	}
	before, err := s.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings before legacy insert: %v", err)
	}
	legacyHash, err := auth.HashPassword(legacyDefaultAdminPassword)
	if err != nil {
		t.Fatalf("HashPassword legacy default: %v", err)
	}
	if _, err := s.systemDB.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaAdminPasswordHash, legacyHash); err != nil {
		t.Fatalf("insert legacy password hash: %v", err)
	}
	if err := s.EnsureAdminSettings(); err != nil {
		t.Fatalf("EnsureAdminSettings: %v", err)
	}
	settings, err := s.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings: %v", err)
	}
	if settings.PasswordHash != "" {
		t.Fatalf("legacy default password hash was not removed: %q", settings.PasswordHash)
	}
	if settings.AuthEpoch != before.AuthEpoch+1 {
		t.Fatalf("legacy password removal epoch = %d, want %d", settings.AuthEpoch, before.AuthEpoch+1)
	}
	if !settings.PasswordLoginEnabled || settings.BarkLoginEnabled {
		t.Fatalf("default login methods = %+v", settings)
	}
}

func TestAdminPasswordRejectsLegacyPlaceholder(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	if err := s.EnsureAdminSettings(); err != nil {
		t.Fatalf("EnsureAdminSettings: %v", err)
	}
	legacyHash, err := auth.HashPassword(legacyDefaultAdminPassword)
	if err != nil {
		t.Fatalf("HashPassword legacy default: %v", err)
	}
	if _, err := s.InitializeAdminPassword(legacyHash); !errors.Is(err, ErrInvalid) {
		t.Fatalf("InitializeAdminPassword legacy placeholder err = %v, want ErrInvalid", err)
	}
	if _, err := s.UpdateAdminPassword(legacyHash); !errors.Is(err, ErrInvalid) {
		t.Fatalf("UpdateAdminPassword legacy placeholder err = %v, want ErrInvalid", err)
	}
	settings, err := s.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings: %v", err)
	}
	if settings.PasswordHash != "" {
		t.Fatalf("legacy placeholder was persisted: %q", settings.PasswordHash)
	}
}

func TestEnsureAdminSettingsMigratesLegacyLoginMode(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	for key, value := range map[string]string{
		metaLegacyAdminLoginMode: AdminLoginModeBark,
		metaLegacyBarkBaseURL:    "https://bark.example.com/",
		metaLegacyBarkDeviceKey:  "device-key",
	} {
		if _, err := s.systemDB.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)`, key, value); err != nil {
			t.Fatalf("insert legacy setting %s: %v", key, err)
		}
	}
	if err := s.EnsureAdminSettings(); err != nil {
		t.Fatalf("EnsureAdminSettings: %v", err)
	}
	settings, err := s.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings: %v", err)
	}
	if !settings.PasswordLoginEnabled || !settings.BarkLoginEnabled ||
		settings.BarkAddress != "https://bark.example.com/device-key" {
		t.Fatalf("migrated admin settings = %+v", settings)
	}
	var legacyCount int
	if err := s.systemDB.QueryRow(`SELECT COUNT(*) FROM meta WHERE key IN (?, ?, ?)`,
		metaLegacyAdminLoginMode, metaLegacyBarkBaseURL, metaLegacyBarkDeviceKey).Scan(&legacyCount); err != nil {
		t.Fatalf("count legacy settings: %v", err)
	}
	if legacyCount != 0 {
		t.Fatalf("legacy settings were not removed: %d", legacyCount)
	}
}

func TestTableLifecycleIsolationAndStats(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 16)
	p, _ := mustProject(t, s, "demo")
	users := mustTable(t, s, p, "users")
	settings := mustTable(t, s, p, "settings")

	if users.TableName != "data_1" || settings.TableName != "data_2" {
		t.Fatalf("physical tables = %q, %q", users.TableName, settings.TableName)
	}
	if _, err := s.CreateTable(p, "users"); !errors.Is(err, ErrTableExists) {
		t.Fatalf("duplicate table err = %v, want ErrTableExists", err)
	}
	if _, err := s.CreateTable(p, "Bad Name"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid table err = %v, want ErrInvalid", err)
	}

	first, err := s.PutKey(p, users.Name, "a", "text/plain", strings.NewReader("one"))
	if err != nil {
		t.Fatalf("put users: %v", err)
	}
	second, err := s.PutKey(p, settings.Name, "a", "text/plain", strings.NewReader("two"))
	if err != nil {
		t.Fatalf("put settings: %v", err)
	}
	if first.UpdatedAt <= 0 || second.UpdatedAt <= 0 {
		t.Fatalf("record timestamps should be set: users=%+v settings=%+v", first, second)
	}

	usersKeys, total, err := s.ListKeys(p, users.Name, "", 10, 0)
	if err != nil || total != 1 || len(usersKeys) != 1 || usersKeys[0].Key != "a" {
		t.Fatalf("users keys = %+v total=%d err=%v", usersKeys, total, err)
	}
	settingsRecords, err := s.QueryRecords(p, settings.Name, RecordQuery{IncludeValues: true})
	if err != nil || settingsRecords.Total != 1 || len(settingsRecords.Records) != 1 {
		t.Fatalf("settings records = %+v err=%v", settingsRecords, err)
	}
	if got := string(settingsRecords.Records[0].Value); got != "two" {
		t.Fatalf("settings value = %q", got)
	}

	p2, _ := mustProject(t, s, "other")
	otherUsers := mustTable(t, s, p2, "users")
	if otherUsers.TableName != "data_1" {
		t.Fatalf("other project users table = %q, want per-project data_1", otherUsers.TableName)
	}
	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects = %+v", projects)
	}
	stats := make(map[string]ProjectSummary)
	for _, project := range projects {
		stats[project.Name] = project
	}
	if stats["demo"].TableCount != 2 || stats["demo"].KeyCount != 2 {
		t.Fatalf("demo stats = %+v", stats["demo"])
	}
	if stats["other"].TableCount != 1 || stats["other"].KeyCount != 0 {
		t.Fatalf("other stats = %+v", stats["other"])
	}

	if err := s.DeleteTable(p, settings.Name); err != nil {
		t.Fatalf("DeleteTable: %v", err)
	}
	if _, err := s.GetTable(p, settings.Name); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("deleted table err = %v, want ErrTableNotFound", err)
	}
	db, err := openSQLite(s.projectDBPath(p.ID))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, settings.TableName).Scan(&name)
	db.Close()
	if err == nil {
		t.Fatalf("physical table %s still exists", settings.TableName)
	}
}

func TestProjectNamesAreCaseInsensitive(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	p, token, err := s.CreateProject("MiXeD-App")
	if err != nil {
		t.Fatalf("CreateProject mixed case: %v", err)
	}
	if p.Name != "MiXeD-App" {
		t.Fatalf("project display name = %q, want MiXeD-App", p.Name)
	}

	for _, lookup := range []string{"mixed-app", "MIXED-APP", "MiXeD-App"} {
		got, err := s.GetProject(lookup)
		if err != nil {
			t.Fatalf("GetProject(%q): %v", lookup, err)
		}
		if got.ID != p.ID || got.Name != "MiXeD-App" {
			t.Fatalf("GetProject(%q) = %+v", lookup, got)
		}
		verified, _, err := s.VerifyProjectToken(lookup, token)
		if err != nil {
			t.Fatalf("VerifyProjectToken(%q): %v", lookup, err)
		}
		if verified.ID != p.ID || verified.Name != "MiXeD-App" {
			t.Fatalf("VerifyProjectToken(%q) = %+v", lookup, verified)
		}
		if _, _, err := s.CreateProjectToken(lookup, "case token"); err != nil {
			t.Fatalf("CreateProjectToken(%q): %v", lookup, err)
		}
	}

	if _, _, err := s.CreateProject("mixed-app"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("case-insensitive duplicate err = %v, want ErrInvalid", err)
	}
	if _, _, err := s.CreateProject("MIXED-APP"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("uppercase duplicate err = %v, want ErrInvalid", err)
	}
}

func TestProjectRecentAccessWindow(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	p, _ := mustProject(t, s, "demo")
	tokens, err := s.ListProjectTokens("demo")
	if err != nil || len(tokens) != 1 {
		t.Fatalf("ListProjectTokens = %+v, err=%v", tokens, err)
	}
	now := time.Now().Unix()
	if _, err := s.systemDB.Exec(`UPDATE project_usage SET request_count = 100 WHERE project_id = ?`, p.ID); err != nil {
		t.Fatalf("set project usage: %v", err)
	}
	for _, row := range []struct {
		second int64
		count  int64
	}{
		{now, 7},
		{now - 200, 5},
		{now - 600, 11},
	} {
		if _, err := s.systemDB.Exec(`INSERT INTO project_token_access
			(project_id, token_id, second_start, request_count) VALUES(?, ?, ?, ?)`,
			p.ID, tokens[0].ID, row.second, row.count); err != nil {
			t.Fatalf("insert access bucket: %v", err)
		}
	}
	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].RequestCount != 100 || projects[0].RecentRequest != 12 {
		t.Fatalf("project access summary = %+v", projects)
	}
}

func TestPutAutoCreatesTableAndHandlesValues(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	p, _ := mustProject(t, s, "demo")

	meta, err := s.PutKey(p, "auto", "user:1", "application/json", strings.NewReader(`{"name":"张三"}`))
	if err != nil {
		t.Fatalf("PutKey inline: %v", err)
	}
	if meta.External {
		t.Fatal("small value should be inline")
	}
	if meta.Size == 0 || len(meta.ContentHash) != 64 {
		t.Fatalf("meta = %+v", meta)
	}
	table, err := s.GetTable(p, "auto")
	if err != nil {
		t.Fatalf("auto-created table missing: %v", err)
	}
	if table.TableName != "data_1" {
		t.Fatalf("auto table name = %q", table.TableName)
	}

	got, rc, err := s.GetKey(p, table.Name, "user:1")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if got.ContentType != "application/json" {
		t.Fatalf("content type = %q", got.ContentType)
	}
	if body := mustRead(t, rc); body != `{"name":"张三"}` {
		t.Fatalf("body = %q", body)
	}

	big := strings.Repeat("x", 100)
	meta, err = s.PutKey(p, table.Name, "user:1", "text/plain", strings.NewReader(big))
	if err != nil {
		t.Fatalf("PutKey external: %v", err)
	}
	if !meta.External {
		t.Fatal("large value should be external")
	}
	blobPath := filepath.Join(s.tableBlobDir(p, table), meta.ContentHash)
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("external blob missing: %v", err)
	}
	if _, rc, err := s.GetKey(p, table.Name, "user:1"); err != nil {
		t.Fatalf("GetKey external: %v", err)
	} else if body := mustRead(t, rc); body != big {
		t.Fatalf("external body length = %d", len(body))
	}

	meta, err = s.PutKey(p, table.Name, "user:1", "text/plain", strings.NewReader("small again"))
	if err != nil {
		t.Fatalf("PutKey inline again: %v", err)
	}
	if meta.External {
		t.Fatal("value should be inline again")
	}
	if _, err := os.Stat(blobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old blob still exists: %v", err)
	}

	if err := s.DeleteKey(p, table.Name, "user:1"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	if _, _, err := s.GetKey(p, table.Name, "user:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetKey after delete err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteKey(p, table.Name, "user:1"); err != nil {
		t.Fatalf("DeleteKey idempotent: %v", err)
	}
}

func TestRenameKey(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 16)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "state")
	body := strings.Repeat("rename-", 20)
	_, err := s.PutKey(p, table.Name, "old", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatalf("put old: %v", err)
	}

	renamed, err := s.RenameKey(p, table.Name, "old", "new")
	if err != nil {
		t.Fatalf("RenameKey: %v", err)
	}
	if renamed.Key != "new" || renamed.UpdatedAt <= 0 {
		t.Fatalf("renamed meta = %+v", renamed)
	}
	if _, _, err := s.GetKey(p, table.Name, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old key err = %v, want ErrNotFound", err)
	}
	if _, rc, err := s.GetKey(p, table.Name, "new"); err != nil {
		t.Fatalf("GetKey new: %v", err)
	} else if got := mustRead(t, rc); got != body {
		t.Fatalf("renamed value = %q", got)
	}
	deleted, err := s.QueryRecords(p, table.Name, RecordQuery{
		Prefix:         "old",
		IncludeDeleted: true,
	})
	if err != nil {
		t.Fatalf("query rename tombstone: %v", err)
	}
	if deleted.Total != 1 || len(deleted.Records) != 1 || !deleted.Records[0].Deleted {
		t.Fatalf("rename tombstone = %+v", deleted)
	}

	if _, err := s.PutKey(p, table.Name, "occupied", "text/plain", strings.NewReader("x")); err != nil {
		t.Fatalf("put occupied: %v", err)
	}
	if _, err := s.RenameKey(p, table.Name, "new", "occupied"); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("rename conflict err = %v, want ErrKeyExists", err)
	}
	if _, err := s.RenameKey(p, table.Name, "missing", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing err = %v, want ErrNotFound", err)
	}
	summary, err := s.GetTableSummary(p, table.Name)
	if err != nil {
		t.Fatalf("GetTableSummary: %v", err)
	}
	if summary.KeyCount != 2 {
		t.Fatalf("key count = %d, want 2", summary.KeyCount)
	}
}

func TestUpdateKeyContentTypePreservesValue(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 16)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "main")
	body := strings.Repeat("preserve-", 20)
	first, err := s.PutKey(p, table.Name, "k", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatalf("PutKey: %v", err)
	}

	updated, err := s.UpdateKeyContentType(p, table.Name, "k", "application/octet-stream", 1_760_000_001_000)
	if err != nil {
		t.Fatalf("UpdateKeyContentType: %v", err)
	}
	if updated.ContentType != "application/octet-stream" || updated.ContentHash != first.ContentHash ||
		updated.Size != first.Size || updated.UpdatedAt != 1_760_000_001_000 {
		t.Fatalf("updated meta = %+v", updated)
	}
	meta, rc, err := s.GetKey(p, table.Name, "k")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if meta.ContentType != "application/octet-stream" {
		t.Fatalf("content type = %q", meta.ContentType)
	}
	if got := mustRead(t, rc); got != body {
		t.Fatalf("value changed after content type update")
	}
	if _, err := s.UpdateKeyContentType(p, table.Name, "missing", "text/plain", 1_760_000_001_001); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key err = %v, want ErrNotFound", err)
	}
}

func TestPutKeyCreateRejectsDuplicate(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "main")

	if _, err := s.PutKeyCreate(p, table.Name, "k", "text/plain", strings.NewReader("first")); err != nil {
		t.Fatalf("first PutKeyCreate: %v", err)
	}
	if _, err := s.PutKeyCreate(p, table.Name, "k", "text/plain", strings.NewReader("second")); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("duplicate PutKeyCreate err = %v, want ErrKeyExists", err)
	}
	if _, rc, err := s.GetKey(p, table.Name, "k"); err != nil {
		t.Fatalf("GetKey: %v", err)
	} else if got := mustRead(t, rc); got != "first" {
		t.Fatalf("duplicate create overwrote value: %q", got)
	}
	if err := s.DeleteKey(p, table.Name, "k"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	if _, err := s.PutKeyCreate(p, table.Name, "k", "text/plain", strings.NewReader("recreated")); err != nil {
		t.Fatalf("recreate deleted key: %v", err)
	}
}

func TestLargeValueDedupAndTableDeleteCleanup(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 16)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "files")
	body := strings.Repeat("same-content-", 20)

	m1, err := s.PutKey(p, table.Name, "a", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatalf("put a: %v", err)
	}
	m2, err := s.PutKey(p, table.Name, "b", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatalf("put b: %v", err)
	}
	if m1.ContentHash != m2.ContentHash {
		t.Fatal("same content should have same hash")
	}
	blobPath := filepath.Join(s.tableBlobDir(p, table), m1.ContentHash)
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("blob missing: %v", err)
	}

	if _, err := s.PutKey(p, table.Name, "c", "text/plain", strings.NewReader(body)); err != nil {
		t.Fatalf("put c: %v", err)
	}
	if err := s.DeleteTable(p, table.Name); err != nil {
		t.Fatalf("DeleteTable: %v", err)
	}
	if _, err := os.Stat(s.tableBlobDir(p, table)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("table blob dir still exists: %v", err)
	}
}

func TestListKeysPrefixAndPagination(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "records")
	for _, k := range []string{"a:1", "a:2", "a:3", "b:1", "中文:键"} {
		if _, err := s.PutKey(p, table.Name, k, "text/plain", strings.NewReader("v")); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}

	keys, total, err := s.ListKeys(p, table.Name, "a:", 2, 1)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(keys) != 2 || keys[0].Key != "a:2" || keys[1].Key != "a:3" {
		t.Fatalf("keys = %+v", keys)
	}

	keys, total, err = s.ListKeys(p, table.Name, "中文", 10, 0)
	if err != nil {
		t.Fatalf("ListKeys unicode: %v", err)
	}
	if total != 1 || len(keys) != 1 || keys[0].Key != "中文:键" {
		t.Fatalf("unicode keys = %+v total=%d", keys, total)
	}
}

func TestMaxValueAndValidation(t *testing.T) {
	s, _ := openTestStore(t, 10, 4)
	p, _ := mustProject(t, s, "demo")

	if _, err := s.PutKey(p, "limits", "k", "text/plain", strings.NewReader("01234567890")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, err := s.GetTable(p, "limits"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("failed write should not create table: %v", err)
	}
	if _, err := s.PutKey(p, "limits", "", "text/plain", strings.NewReader("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty key err = %v, want ErrInvalid", err)
	}
	if _, err := s.PutKey(p, "Bad Table", "k", "text/plain", strings.NewReader("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad table err = %v, want ErrInvalid", err)
	}
	if _, err := s.PutKey(p, "limits", "k", "not a media type", strings.NewReader("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad content type err = %v, want ErrInvalid", err)
	}
	if _, err := s.PutKey(p, "limits", "k", "text/plain", strings.NewReader("0123456789")); err != nil {
		t.Fatalf("exact max: %v", err)
	}
}

func TestValidateKeyPathSafety(t *testing.T) {
	valid := []string{"k", "dir/sub/file", "中文:键", "a.b", "a b", "user:1"}
	for _, key := range valid {
		if err := ValidateKey(key); err != nil {
			t.Fatalf("ValidateKey(%q) = %v, want nil", key, err)
		}
	}
	invalid := []string{"", "/a", "a/", "a//b", ".", "..", "a/../b", "a/./b", "a\x00b"}
	for _, key := range invalid {
		if err := ValidateKey(key); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidateKey(%q) = %v, want ErrInvalid", key, err)
		}
	}
}

func TestPutAfterProjectDelete(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 16)
	p, _ := mustProject(t, s, "demo")
	if err := s.DeleteProject("demo"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	// 模拟一个已经通过鉴权、但在 project 删除后才完成上传的请求。
	_, err := s.PutKey(p, "files", "k", "text/plain", strings.NewReader(strings.Repeat("x", 100)))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("PutKey after delete err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(s.projectDir(p.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted project dir was recreated: %v", err)
	}
}

func TestStartupCleansTemporaryAndOrphanFiles(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "files")
	body := strings.Repeat("keep-", 20)
	meta, err := s.PutKey(p, table.Name, "keep", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	blobDir := s.tableBlobDir(p, table)
	orphan := strings.Repeat("a", 64)
	unknown := filepath.Join(blobDir, "not-managed")
	tmpFile := filepath.Join(dataDir, "tmp", "put-crash")
	orphanProject := filepath.Join(dataDir, "projects", "999")
	if err := os.WriteFile(filepath.Join(blobDir, orphan), []byte("orphan"), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	if err := os.WriteFile(unknown, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write unknown: %v", err)
	}
	if err := os.WriteFile(tmpFile, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.MkdirAll(orphanProject, 0o700); err != nil {
		t.Fatalf("mkdir orphan project: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := os.Stat(tmpFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temp file still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blobDir, orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan blob still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blobDir, meta.ContentHash)); err != nil {
		t.Fatalf("referenced blob missing: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unmanaged file was removed: %v", err)
	}
	if _, err := os.Stat(orphanProject); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan project dir still exists: %v", err)
	}
}

func TestDeleteProjectRemovesDirectory(t *testing.T) {
	s, dataDir := openTestStore(t, 1<<20, 32)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "files")
	if _, err := s.PutKey(p, table.Name, "k", "text/plain", strings.NewReader(strings.Repeat("x", 100))); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := s.DeleteProject("demo"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := s.GetProject("demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProject err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(s.projectDir(p.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project dir still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "system.db")); err != nil {
		t.Fatalf("system.db should remain: %v", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, token, err := s.CreateProject("demo")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	body := strings.Repeat("persist-", 10)
	if _, err := s.PutKey(p, "data", "k", "text/plain", strings.NewReader(body)); err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	p2, _, err := s2.VerifyProjectToken("demo", token)
	if err != nil {
		t.Fatalf("VerifyProjectToken after reopen: %v", err)
	}
	if p2.ID != p.ID {
		t.Fatalf("project id = %d, want %d", p2.ID, p.ID)
	}
	table, err := s2.GetTable(p2, "data")
	if err != nil {
		t.Fatalf("GetTable after reopen: %v", err)
	}
	_, rc, err := s2.GetKey(p2, table.Name, "k")
	if err != nil {
		t.Fatalf("GetKey after reopen: %v", err)
	}
	if got := mustRead(t, rc); got != body {
		t.Fatalf("body after reopen = %q", got)
	}
}

func TestConditionalWritesAndTombstones(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "state")

	const (
		t1 = int64(1_760_000_000_001)
		t2 = int64(1_760_000_000_002)
		t3 = int64(1_760_000_000_003)
		t4 = int64(1_760_000_000_004)
		t5 = int64(1_760_000_000_005)
	)
	first, err := s.PutKeyConditional(
		p, table.Name, "k", "application/json", strings.NewReader(`{"v":1}`), t1, WriteCondition{},
	)
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	if first.UpdatedAt != t1 {
		t.Fatalf("first meta = %+v", first)
	}

	second, err := s.PutKeyConditional(
		p,
		table.Name,
		"k",
		"application/json",
		strings.NewReader(`{"v":2}`),
		t2,
		WriteCondition{All: []Condition{
			{Field: "updatedAt", Op: "lt", Value: t2},
			{Field: "contentType", Op: "eq", Value: "application/json"},
			{Field: "value", Op: "startswith", Value: `{"v":`},
		}},
	)
	if err != nil {
		t.Fatalf("conditional put: %v", err)
	}
	if second.UpdatedAt != t2 {
		t.Fatalf("second meta = %+v", second)
	}
	if _, err := s.PutKeyConditional(
		p,
		table.Name,
		"k",
		"application/json",
		strings.NewReader(`{"v":3}`),
		t3,
		WriteCondition{All: []Condition{{Field: "updatedAt", Op: "lt", Value: t2}}},
	); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale put err = %v, want ErrPreconditionFailed", err)
	}
	if _, rc, err := s.GetKey(p, table.Name, "k"); err != nil {
		t.Fatalf("get after failed put: %v", err)
	} else if got := mustRead(t, rc); got != `{"v":2}` {
		t.Fatalf("failed put changed value to %q", got)
	}

	deleted, err := s.DeleteKeyConditional(
		p,
		table.Name,
		"k",
		t4,
		WriteCondition{All: []Condition{{Field: "updatedAt", Op: "eq", Value: t2}}},
	)
	if err != nil {
		t.Fatalf("conditional delete: %v", err)
	}
	if !deleted.Deleted || deleted.UpdatedAt != t4 {
		t.Fatalf("deleted meta = %+v", deleted)
	}
	if _, _, err := s.GetKey(p, table.Name, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get tombstone err = %v, want ErrNotFound", err)
	}

	query, err := s.QueryRecords(p, table.Name, RecordQuery{
		Prefix:         "k",
		IncludeDeleted: true,
		IncludeValues:  true,
	})
	if err != nil {
		t.Fatalf("query tombstone: %v", err)
	}
	if query.Total != 1 || len(query.Records) != 1 || !query.Records[0].Deleted {
		t.Fatalf("query = %+v", query)
	}

	recreated, err := s.PutKeyConditional(
		p,
		table.Name,
		"k",
		"text/plain",
		strings.NewReader("again"),
		t5,
		WriteCondition{All: []Condition{
			{Field: "deleted", Op: "eq", Value: true},
			{Field: "updatedAt", Op: "lt", Value: t5},
		}},
	)
	if err != nil {
		t.Fatalf("recreate after tombstone: %v", err)
	}
	if recreated.UpdatedAt != t5 || recreated.Deleted {
		t.Fatalf("recreated meta = %+v", recreated)
	}
}

func TestWriteConditionsOnMetadataAndExternalValue(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 4)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "conditions")

	const (
		t1 = int64(1_760_000_000_101)
		t2 = int64(1_760_000_000_102)
		t3 = int64(1_760_000_000_103)
	)
	first, err := s.PutKeyConditional(
		p, table.Name, "k", "text/plain", strings.NewReader("abcdefgh"), t1,
		WriteCondition{All: []Condition{{Field: "updatedAt", Op: "lt", Value: t1}}},
	)
	if err != nil {
		t.Fatalf("create with absent timestamp condition: %v", err)
	}
	if !first.External {
		t.Fatal("test value should be stored externally")
	}

	second, err := s.PutKeyConditional(
		p, table.Name, "k", "text/plain", strings.NewReader("zzzzzzzz"), t2,
		WriteCondition{All: []Condition{
			{Field: "exists", Op: "eq", Value: true},
			{Field: "deleted", Op: "eq", Value: false},
			{Field: "size", Op: "eq", Value: int64(8)},
			{Field: "contentType", Op: "startswith", Value: "text/"},
			{Field: "contentHash", Op: "eq", Value: first.ContentHash},
			{Field: "value", Op: "startswith", Value: "abc"},
			{Field: "value", Op: "endswith", Value: "fgh"},
		}},
	)
	if err != nil {
		t.Fatalf("metadata and external value conditions: %v", err)
	}
	if second.UpdatedAt != t2 {
		t.Fatalf("updatedAt = %d, want %d", second.UpdatedAt, t2)
	}

	_, err = s.PutKeyConditional(
		p, table.Name, "k", "text/plain", strings.NewReader("wrong"), t3,
		WriteCondition{All: []Condition{{Field: "value", Op: "lt", Value: "aaaa"}}},
	)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("value comparison err = %v, want ErrPreconditionFailed", err)
	}

	deleted, err := s.DeleteKeyConditional(
		p, table.Name, "k", t3,
		WriteCondition{All: []Condition{
			{Field: "value", Op: "startswith", Value: "zzz"},
			{Field: "updatedAt", Op: "eq", Value: t2},
		}},
	)
	if err != nil {
		t.Fatalf("delete with value condition: %v", err)
	}
	if !deleted.Deleted || deleted.UpdatedAt != t3 {
		t.Fatalf("deleted meta = %+v", deleted)
	}
	if err := ValidateWriteCondition(WriteCondition{All: []Condition{
		{Field: "unknown", Op: "eq", Value: true},
	}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid field err = %v, want ErrInvalid", err)
	}
}

func TestWriteConditionIsAtomicUnderConcurrency(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "atomic")
	condition := WriteCondition{All: []Condition{{Field: "exists", Op: "eq", Value: false}}}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, value := range []string{"first", "second"} {
		wg.Add(1)
		go func(i int, value string) {
			defer wg.Done()
			_, err := s.PutKeyConditional(
				p, table.Name, "k", "text/plain", strings.NewReader(value),
				int64(1_760_000_000_200+i), condition,
			)
			errs <- err
		}(i, value)
	}
	wg.Wait()
	close(errs)

	var succeeded, failed int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrPreconditionFailed):
			failed++
		default:
			t.Fatalf("unexpected concurrent put err = %v", err)
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("succeeded=%d failed=%d, want 1/1", succeeded, failed)
	}
}

func TestQueryRecordsByPrefixGlobAndTime(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 32)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "records")

	first, err := s.PutKey(p, table.Name, "watchLater/series:1", "application/json", strings.NewReader(`{"v":1}`))
	if err != nil {
		t.Fatalf("put first: %v", err)
	}
	if _, err := s.PutKey(p, table.Name, "watchLater/series:2", "application/json", strings.NewReader(`{"v":2}`)); err != nil {
		t.Fatalf("put second: %v", err)
	}
	if _, err := s.PutKey(p, table.Name, "progress/episode:1", "application/json", strings.NewReader(`{"p":1}`)); err != nil {
		t.Fatalf("put third: %v", err)
	}

	byPrefix, err := s.QueryRecords(p, table.Name, RecordQuery{
		Prefix:        "watchLater/",
		IncludeValues: true,
	})
	if err != nil {
		t.Fatalf("query prefix: %v", err)
	}
	if byPrefix.Total != 2 || len(byPrefix.Records) != 2 {
		t.Fatalf("prefix result = %+v", byPrefix)
	}
	if string(byPrefix.Records[0].Value) != `{"v":1}` {
		t.Fatalf("first value = %q", byPrefix.Records[0].Value)
	}

	byGlob, err := s.QueryRecords(p, table.Name, RecordQuery{Pattern: "watchLater/*"})
	if err != nil {
		t.Fatalf("query glob: %v", err)
	}
	if byGlob.Total != 2 {
		t.Fatalf("glob total = %d, want 2", byGlob.Total)
	}

	byTime, err := s.QueryRecords(p, table.Name, RecordQuery{UpdatedAfter: first.UpdatedAt})
	if err != nil {
		t.Fatalf("query time: %v", err)
	}
	if byTime.Total != 3 {
		t.Fatalf("time total = %d, want 3", byTime.Total)
	}

	if err := s.DeleteKey(p, table.Name, "watchLater/series:1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	live, err := s.QueryRecords(p, table.Name, RecordQuery{Prefix: "watchLater/"})
	if err != nil {
		t.Fatalf("query live: %v", err)
	}
	if live.Total != 1 {
		t.Fatalf("live total = %d, want 1", live.Total)
	}
	withDeleted, err := s.QueryRecords(p, table.Name, RecordQuery{
		Prefix:         "watchLater/",
		IncludeDeleted: true,
	})
	if err != nil {
		t.Fatalf("query with deleted: %v", err)
	}
	if withDeleted.Total != 2 {
		t.Fatalf("with deleted total = %d, want 2", withDeleted.Total)
	}
}

func TestQueryRecordsValueBudget(t *testing.T) {
	s, _ := openTestStore(t, 64<<20, 16)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "large")
	value := strings.Repeat("x", 17<<20)
	if _, err := s.PutKey(p, table.Name, "a", "text/plain", strings.NewReader(value)); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if _, err := s.PutKey(p, table.Name, "b", "text/plain", strings.NewReader(value)); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if _, err := s.QueryRecords(p, table.Name, RecordQuery{IncludeValues: true}); !errors.Is(err, ErrQueryValueLimit) {
		t.Fatalf("query value limit err = %v, want ErrQueryValueLimit", err)
	}
	result, err := s.QueryRecords(p, table.Name, RecordQuery{})
	if err != nil {
		t.Fatalf("metadata query: %v", err)
	}
	if result.Total != 2 {
		t.Fatalf("metadata total = %d, want 2", result.Total)
	}
}

func TestTableKeyCountTracksLiveRecords(t *testing.T) {
	s, _ := openTestStore(t, 1<<20, 16)
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "state")
	if _, err := s.PutKey(p, table.Name, "a", "text/plain", strings.NewReader("one")); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if _, err := s.PutKey(p, table.Name, "b", "text/plain", strings.NewReader("two")); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if _, err := s.PutKey(p, table.Name, "a", "text/plain", strings.NewReader("updated")); err != nil {
		t.Fatalf("overwrite a: %v", err)
	}
	summary, err := s.GetTableSummary(p, table.Name)
	if err != nil {
		t.Fatalf("GetTableSummary: %v", err)
	}
	if summary.KeyCount != 2 {
		t.Fatalf("key count after writes = %d, want 2", summary.KeyCount)
	}
	if err := s.DeleteKey(p, table.Name, "a"); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	summary, err = s.GetTableSummary(p, table.Name)
	if err != nil {
		t.Fatalf("GetTableSummary after delete: %v", err)
	}
	if summary.KeyCount != 1 {
		t.Fatalf("key count after delete = %d, want 1", summary.KeyCount)
	}
	if _, err := s.PutKey(p, table.Name, "a", "text/plain", strings.NewReader("again")); err != nil {
		t.Fatalf("recreate a: %v", err)
	}
	summary, err = s.GetTableSummary(p, table.Name)
	if err != nil {
		t.Fatalf("GetTableSummary after recreate: %v", err)
	}
	if summary.KeyCount != 2 {
		t.Fatalf("key count after recreate = %d, want 2", summary.KeyCount)
	}
}

func TestStartupRejectsMissingPhysicalTable(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "files")
	meta, err := s.PutKey(p, table.Name, "k", "text/plain", strings.NewReader(strings.Repeat("x", 100)))
	if err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	blobPath := filepath.Join(s.tableBlobDir(p, table), meta.ContentHash)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := openSQLite(s.projectDBPath(p.ID))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE data_1`); err != nil {
		db.Close()
		t.Fatalf("drop physical table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close project db: %v", err)
	}

	if _, err := Open(opts); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Open after physical table loss err = %v, want missing-table error", err)
	}
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("blob was deleted during failed startup: %v", err)
	}
}

func TestStartupRejectsUnregisteredPhysicalTable(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, _ := mustProject(t, s, "demo")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err := openSQLite(s.projectDBPath(p.ID))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	if err := createDataTableSchema(db, "data_99"); err != nil {
		db.Close()
		t.Fatalf("create orphan physical table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close project db: %v", err)
	}
	if _, err := Open(opts); err == nil || !strings.Contains(err.Error(), "no kv_tables metadata") {
		t.Fatalf("Open with unregistered physical table err = %v", err)
	}
}

func TestSystemSchemaMigratesV1ToCurrent(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, token, err := s.CreateProject("demo")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := openSQLite(opts.SystemDBPath)
	if err != nil {
		t.Fatalf("open system db: %v", err)
	}
	for _, q := range []string{
		`ALTER TABLE projects ADD COLUMN token_hash TEXT NOT NULL DEFAULT ''`,
		`UPDATE projects SET token_hash = (
			SELECT token_hash FROM project_tokens WHERE project_id = projects.id LIMIT 1
		)`,
		`DROP TABLE project_tokens`,
		`UPDATE meta SET value = '1' WHERE key = 'schema_version'`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			t.Fatalf("prepare system v1 schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close system db: %v", err)
	}

	migrated, err := Open(opts)
	if err != nil {
		t.Fatalf("Open migrated system: %v", err)
	}
	defer migrated.Close()
	got, tokenMeta, err := migrated.VerifyProjectToken("demo", token)
	if err != nil {
		t.Fatalf("VerifyProjectToken after migration: %v", err)
	}
	if got.ID != p.ID || tokenMeta.Name != "默认 Token" || tokenMeta.Token != "" {
		t.Fatalf("migrated project=%+v token=%+v", got, tokenMeta)
	}
	next, _, err := migrated.CreateProject("next")
	if err != nil {
		t.Fatalf("CreateProject after migration: %v", err)
	}
	if next.ID <= p.ID {
		t.Fatalf("next project id = %d, want > %d", next.ID, p.ID)
	}
}

func TestSystemSchemaResumesInterruptedV2Migration(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, token, err := s.CreateProject("demo")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := openSQLite(opts.SystemDBPath)
	if err != nil {
		t.Fatalf("open system db: %v", err)
	}
	for _, q := range []string{
		`ALTER TABLE projects ADD COLUMN token_hash TEXT NOT NULL DEFAULT ''`,
		`UPDATE projects SET token_hash = (
			SELECT token_hash FROM project_tokens WHERE project_id = projects.id LIMIT 1
		)`,
		`DROP TABLE project_tokens`,
		`UPDATE meta SET value = '1' WHERE key = 'schema_version'`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			t.Fatalf("prepare system v1 schema: %v", err)
		}
	}
	if err := migrateSystemSchemaV1ToV2(db); err != nil {
		db.Close()
		t.Fatalf("migrate to v2: %v", err)
	}
	if version, exists, err := readSchemaVersion(db); err != nil || !exists || version != 2 {
		db.Close()
		t.Fatalf("interrupted schema version = %d, exists=%v, err=%v", version, exists, err)
	}
	if _, ok := tableColumns(t, db, "project_tokens")["token"]; ok {
		db.Close()
		t.Fatal("v2 token table unexpectedly has token column")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v2 system db: %v", err)
	}

	migrated, err := Open(opts)
	if err != nil {
		t.Fatalf("Open after interrupted migration: %v", err)
	}
	defer migrated.Close()
	if _, tokenMeta, err := migrated.VerifyProjectToken("demo", token); err != nil {
		t.Fatalf("VerifyProjectToken after resumed migration: %v", err)
	} else if tokenMeta.Token != "" {
		t.Fatalf("legacy token plaintext = %q, want empty", tokenMeta.Token)
	}
}

func TestSystemSchemaRepairsV3MissingTokenColumn(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, token, err := s.CreateProject("demo")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := openSQLite(opts.SystemDBPath)
	if err != nil {
		t.Fatalf("open system db: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE project_tokens__broken (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			request_count INTEGER NOT NULL DEFAULT 0,
			read_count INTEGER NOT NULL DEFAULT 0,
			write_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			last_used_at INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO project_tokens__broken
			(id, project_id, name, token_hash, request_count, read_count, write_count, created_at, last_used_at)
			SELECT id, project_id, name, token_hash, request_count, read_count, write_count, created_at, last_used_at
			FROM project_tokens`,
		`DROP TABLE project_tokens`,
		`ALTER TABLE project_tokens__broken RENAME TO project_tokens`,
		`CREATE INDEX idx_project_tokens_project_id ON project_tokens(project_id)`,
		`UPDATE meta SET value = '3' WHERE key = 'schema_version'`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			t.Fatalf("prepare broken v3 schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close broken system db: %v", err)
	}

	repaired, err := Open(opts)
	if err != nil {
		t.Fatalf("Open broken v3 schema: %v", err)
	}
	defer repaired.Close()
	tokens, err := repaired.ListProjectTokens("demo")
	if err != nil {
		t.Fatalf("ListProjectTokens after repair: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Token != "" {
		t.Fatalf("repaired tokens = %+v", tokens)
	}
	if _, _, err := repaired.VerifyProjectToken("demo", token); err != nil {
		t.Fatalf("VerifyProjectToken after repair: %v", err)
	}
}

func TestProjectSchemaMigratesV1ToV2(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, _ := mustProject(t, s, "demo")
	table := mustTable(t, s, p, "main")
	if _, err := s.PutKey(p, table.Name, "keep", "text/plain", strings.NewReader("migrated value")); err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := openSQLite(s.projectDBPath(p.ID))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	for _, q := range []string{
		`ALTER TABLE kv_tables ADD COLUMN next_seq INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE data_1 ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE data_1 ADD COLUMN server_seq INTEGER NOT NULL DEFAULT 0`,
		`UPDATE meta SET value = '1' WHERE key = 'schema_version'`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			t.Fatalf("prepare v1 schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 project db: %v", err)
	}

	migrated, err := Open(opts)
	if err != nil {
		t.Fatalf("Open migrated store: %v", err)
	}
	defer migrated.Close()
	p2, err := migrated.GetProject("demo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if _, rc, err := migrated.GetKey(p2, "main", "keep"); err != nil {
		t.Fatalf("GetKey after migration: %v", err)
	} else if got := mustRead(t, rc); got != "migrated value" {
		t.Fatalf("value after migration = %q", got)
	}
	nextTable, err := migrated.CreateTable(p2, "next")
	if err != nil {
		t.Fatalf("CreateTable after migration: %v", err)
	}
	if nextTable.ID <= table.ID {
		t.Fatalf("new table id = %d, want > %d", nextTable.ID, table.ID)
	}

	db, err = openSQLite(migrated.projectDBPath(p2.ID))
	if err != nil {
		t.Fatalf("open migrated project db: %v", err)
	}
	defer db.Close()
	columns := tableColumns(t, db, "data_1")
	for _, removed := range []string{"revision", "server_seq"} {
		if _, ok := columns[removed]; ok {
			t.Fatalf("data table still has column %s", removed)
		}
	}
	columns = tableColumns(t, db, "kv_tables")
	if _, ok := columns["next_seq"]; ok {
		t.Fatal("kv_tables still has next_seq")
	}
	version, exists, err := readSchemaVersion(db)
	if err != nil || !exists || version != currentProjectSchemaVersion {
		t.Fatalf("schema version = %d, exists=%v, err=%v", version, exists, err)
	}
}

func tableColumns(t *testing.T, db *sql.DB, table string) map[string]struct{} {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info("` + table + `")`)
	if err != nil {
		t.Fatalf("table info %s: %v", table, err)
	}
	defer rows.Close()
	columns := make(map[string]struct{})
	for rows.Next() {
		var (
			cid          int
			name         string
			columnType   string
			notNull      int
			defaultValue sql.NullString
			primaryKey   int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan table info %s: %v", table, err)
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table info rows %s: %v", table, err)
	}
	return columns
}

func TestStartupRejectsIncompatibleProjectSchema(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	opts := Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   1 << 20,
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, _ := mustProject(t, s, "demo")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err := openSQLite(s.projectDBPath(p.ID))
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	if _, err := db.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		db.Close()
		t.Fatalf("set schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close project db: %v", err)
	}
	if _, err := Open(opts); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("Open with incompatible schema err = %v", err)
	}
}
