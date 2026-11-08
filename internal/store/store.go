package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"akkvstore/internal/auth"

	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound 表示 key 不存在。
	ErrNotFound = errors.New("not found")
	// ErrTableNotFound 表示 project 内的 table 不存在。
	ErrTableNotFound = errors.New("table not found")
	// ErrTableExists 表示 project 内已有同名 table。
	ErrTableExists = errors.New("table already exists")
	// ErrKeyExists 表示重命名后的 key 已存在。
	ErrKeyExists = errors.New("key already exists")
	// ErrQueryValueLimit 表示 records 查询请求的 value 总量超过内存保护上限。
	ErrQueryValueLimit = errors.New("query value limit exceeded")
	// ErrUnauthorized 表示 project 不存在或 token 不正确。
	ErrUnauthorized = errors.New("unauthorized")
	// ErrInvalid 表示输入不合法。
	ErrInvalid = errors.New("invalid input")
	// ErrTooLarge 表示 value 超过配置上限。
	ErrTooLarge = errors.New("value too large")
	// ErrPreconditionFailed 表示写入前置条件不成立。
	ErrPreconditionFailed = errors.New("precondition failed")
)

var (
	projectNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
	tableNameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	dataTableRe   = regexp.MustCompile(`^data_[0-9]+$`)
	blobFileRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const (
	maxKeyBytes        = 1024
	maxPatternBytes    = 256
	defaultQueryLimit  = 100
	maxQueryLimit      = 500
	maxQueryValueBytes = 32 << 20

	currentProjectSchemaVersion = 2
	currentSystemSchemaVersion  = 6
	systemSchemaV2              = 2
	systemSchemaV3              = 3
	systemSchemaV4              = 4
	systemSchemaV5              = 5
)

// Options 是打开存储所需的参数。
type Options struct {
	SystemDBPath    string
	DataDir         string
	InlineThreshold int64
	MaxValueBytes   int64
}

// Project 是 system.db 中 projects 表的一行。
type Project struct {
	ID        int64
	Name      string
	CreatedAt int64
	UpdatedAt int64
}

// ProjectToken 是 project 的一个访问 Token 及其使用统计。
type ProjectToken struct {
	ID           int64
	ProjectID    int64
	Name         string
	Token        string
	TokenHash    string
	Fingerprint  string
	RequestCount int64
	ReadCount    int64
	WriteCount   int64
	CreatedAt    int64
	LastUsedAt   int64
}

// ProjectSummary 附带 project 内的 table 数量和当前 key 数量。
type ProjectSummary struct {
	Project
	TableCount    int64
	KeyCount      int64
	RequestCount  int64
	RecentRequest int64
}

// Table 是 project.db 中 kv_tables 表的一行。
type Table struct {
	ID        int64
	Name      string
	TableName string
	KeyCount  int64
	CreatedAt int64
	UpdatedAt int64
}

// TableSummary 在 Table 基础上附带当前 key 数量。
type TableSummary struct {
	Table
}

// KeyMeta 是一个 key 的元数据。
type KeyMeta struct {
	Key         string
	ContentType string
	Size        int64
	CreatedAt   int64
	UpdatedAt   int64
	Deleted     bool
	ContentHash string
	External    bool
}

// WriteCondition 描述写入前必须全部满足的条件。
// 空条件表示无条件写入。
type WriteCondition struct {
	All []Condition `json:"all,omitempty"`
}

// PreconditionFailed 携带条件不成立时的当前记录状态。
type PreconditionFailed struct {
	Key           string `json:"key"`
	Exists        bool   `json:"exists"`
	Deleted       bool   `json:"deleted"`
	UpdatedAt     int64  `json:"updatedAt"`
	CreatedAt     int64  `json:"createdAt"`
	ContentType   string `json:"contentType,omitempty"`
	ContentHash   string `json:"contentHash,omitempty"`
	ContentLength int64  `json:"contentLength"`
}

func (e *PreconditionFailed) Error() string {
	return fmt.Sprintf("key %q precondition failed", e.Key)
}

// Unwrap 允许 errors.Is(err, ErrPreconditionFailed) 判断前置条件失败。
func (e *PreconditionFailed) Unwrap() error { return ErrPreconditionFailed }

// RecordQuery 描述通用记录查询条件。时间字段使用服务端 Unix 毫秒。
type RecordQuery struct {
	Prefix         string
	Pattern        string
	UpdatedAfter   int64
	UpdatedBefore  int64
	IncludeDeleted bool
	IncludeValues  bool
	Limit          int
	Offset         int
}

// Record 是查询结果中的一条记录。
type Record struct {
	KeyMeta
	Value []byte
}

// RecordQueryResult 是分页查询结果。
type RecordQueryResult struct {
	Records []Record
	Total   int64
}

// ReadSeekCloser 用于把内存或文件内容交给 http.ServeContent。
type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

type nopReadSeekCloser struct {
	*bytes.Reader
}

func (nopReadSeekCloser) Close() error { return nil }

// Store 封装 system.db、project.db 与外部大文件存储。
type Store struct {
	systemDB        *sql.DB
	dataDir         string
	projectsDir     string
	inlineThreshold int64
	maxValueBytes   int64

	// 文件与数据库之间的操作需要串行化；project 创建和删除使用写锁。
	writeMu sync.RWMutex
	// 访问压力分钟桶按分钟做一次清理，避免每次请求都执行 DELETE。
	lastUsageCleanup atomic.Int64
}

// Open 打开系统数据库并完成基础初始化。
func Open(opts Options) (*Store, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("%w: data dir is empty", ErrInvalid)
	}
	if opts.SystemDBPath == "" {
		return nil, fmt.Errorf("%w: system db path is empty", ErrInvalid)
	}
	if opts.InlineThreshold <= 0 {
		return nil, fmt.Errorf("%w: inline threshold must be positive", ErrInvalid)
	}

	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	systemDBPath, err := filepath.Abs(opts.SystemDBPath)
	if err != nil {
		return nil, fmt.Errorf("resolve system db path: %w", err)
	}
	opts.DataDir = dataDir
	opts.SystemDBPath = systemDBPath

	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	projectsDir := filepath.Join(opts.DataDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create projects dir: %w", err)
	}
	tmpDir := filepath.Join(opts.DataDir, "tmp")
	if err := os.RemoveAll(tmpDir); err != nil {
		return nil, fmt.Errorf("clean tmp dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}
	if dir := filepath.Dir(opts.SystemDBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create system db dir: %w", err)
		}
	}
	if err := syncDir(opts.DataDir); err != nil {
		return nil, err
	}
	if err := syncDir(projectsDir); err != nil {
		return nil, err
	}

	systemDB, err := openSQLite(opts.SystemDBPath)
	if err != nil {
		return nil, fmt.Errorf("open system db: %w", err)
	}
	s := &Store{
		systemDB:        systemDB,
		dataDir:         opts.DataDir,
		projectsDir:     projectsDir,
		inlineThreshold: opts.InlineThreshold,
		maxValueBytes:   opts.MaxValueBytes,
	}
	if err := s.initSystemSchema(); err != nil {
		systemDB.Close()
		return nil, err
	}
	if err := s.prepareProjects(); err != nil {
		systemDB.Close()
		return nil, err
	}
	if err := s.cleanupProjectDirs(); err != nil {
		systemDB.Close()
		return nil, err
	}
	return s, nil
}

func openSQLite(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(dbPath))
	if err != nil {
		return nil, err
	}
	// 单写连接：SQLite 写入串行化，避免 writer 竞争带来的 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	mustExec := func(q string) error {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
		return nil
	}
	if err := mustExec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	// FULL 保证每次提交都落到磁盘；进程崩溃/断电不会丢失已确认的写入。
	if err := mustExec(`PRAGMA synchronous = FULL`); err != nil {
		db.Close()
		return nil, err
	}
	if err := mustExec(`PRAGMA foreign_keys = OFF`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// sqliteDSN 把连接级 pragma 写进 DSN，保证连接池新建连接时也保留同步级别。
func sqliteDSN(dbPath string) string {
	u := &url.URL{Scheme: "file", Path: dbPath}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Store) initSystemSchema() error {
	if _, err := s.systemDB.Exec(`CREATE TABLE IF NOT EXISTS meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("init system meta: %w", err)
	}
	version, exists, err := readSchemaVersion(s.systemDB)
	if err != nil {
		return fmt.Errorf("read system schema version: %w", err)
	}
	if !exists {
		if err := createSystemSchemaV6(s.systemDB); err != nil {
			return err
		}
		_, err := s.systemDB.Exec(`INSERT INTO meta(key, value) VALUES('schema_version', ?)`,
			strconv.Itoa(currentSystemSchemaVersion))
		if err != nil {
			return fmt.Errorf("init system schema version: %w", err)
		}
		return nil
	}
	switch version {
	case currentSystemSchemaVersion:
		return ensureSystemSchemaV6(s.systemDB)
	case systemSchemaV5:
		if err := migrateSystemSchemaV5ToV6(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 5 to 6: %w", err)
		}
		return ensureSystemSchemaV6(s.systemDB)
	case systemSchemaV4:
		if err := migrateSystemSchemaV4ToV5(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 4 to 5: %w", err)
		}
		if err := migrateSystemSchemaV5ToV6(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 5 to 6: %w", err)
		}
		return ensureSystemSchemaV6(s.systemDB)
	case systemSchemaV3:
		if err := migrateSystemSchemaV3ToV4(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 3 to 4: %w", err)
		}
		if err := migrateSystemSchemaV4ToV5(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 4 to 5: %w", err)
		}
		if err := migrateSystemSchemaV5ToV6(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 5 to 6: %w", err)
		}
		return ensureSystemSchemaV6(s.systemDB)
	case systemSchemaV2:
		if err := migrateSystemSchemaV2ToV3(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 2 to 3: %w", err)
		}
		if err := migrateSystemSchemaV3ToV4(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 3 to 4: %w", err)
		}
		if err := migrateSystemSchemaV4ToV5(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 4 to 5: %w", err)
		}
		if err := migrateSystemSchemaV5ToV6(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 5 to 6: %w", err)
		}
		return ensureSystemSchemaV6(s.systemDB)
	case 1:
		if err := migrateSystemSchemaV1ToV2(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 1 to 2: %w", err)
		}
		if err := migrateSystemSchemaV2ToV3(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 2 to 3: %w", err)
		}
		if err := migrateSystemSchemaV3ToV4(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 3 to 4: %w", err)
		}
		if err := migrateSystemSchemaV4ToV5(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 4 to 5: %w", err)
		}
		if err := migrateSystemSchemaV5ToV6(s.systemDB); err != nil {
			return fmt.Errorf("migrate system schema from 5 to 6: %w", err)
		}
		return ensureSystemSchemaV6(s.systemDB)
	default:
		return fmt.Errorf("system schema version %d is incompatible with expected version %d",
			version, currentSystemSchemaVersion)
	}
}

func (s *Store) initProjectSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS kv_tables (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			table_name TEXT NOT NULL UNIQUE,
			key_count INTEGER NOT NULL DEFAULT 0 CHECK(key_count >= 0),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("init project schema: %w", err)
		}
	}
	version, exists, err := readSchemaVersion(db)
	if err != nil {
		return fmt.Errorf("read project schema version: %w", err)
	}
	if !exists {
		if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES('schema_version', ?)`,
			strconv.Itoa(currentProjectSchemaVersion)); err != nil {
			return fmt.Errorf("init project schema version: %w", err)
		}
		return nil
	}
	switch version {
	case currentProjectSchemaVersion:
		return nil
	case 1:
		if err := migrateProjectSchemaV1ToV2(db); err != nil {
			return fmt.Errorf("migrate project schema from 1 to 2: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("project schema version %d is incompatible with expected version %d",
			version, currentProjectSchemaVersion)
	}
}

func ensureSchemaVersion(db *sql.DB, scope string, expected int) error {
	version, exists, err := readSchemaVersion(db)
	if err != nil {
		return fmt.Errorf("read %s schema version: %w", scope, err)
	}
	if !exists {
		_, err = db.Exec(`INSERT INTO meta(key, value) VALUES('schema_version', ?)`, strconv.Itoa(expected))
		if err != nil {
			return fmt.Errorf("init %s schema version: %w", scope, err)
		}
		return nil
	}
	if version != expected {
		return fmt.Errorf("%s schema version %d is incompatible with expected version %d", scope, version, expected)
	}
	return nil
}

func readSchemaVersion(db *sql.DB) (int, bool, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	version, err := strconv.Atoi(value)
	if err != nil {
		return 0, true, fmt.Errorf("invalid schema version %q: %w", value, err)
	}
	return version, true, nil
}

func (s *Store) prepareProjects() error {
	projects, err := s.listProjectsLocked()
	if err != nil {
		return err
	}
	for _, p := range projects {
		db, err := s.openProjectDB(p)
		if err != nil {
			return fmt.Errorf("prepare project %q: %w", p.Name, err)
		}
		err = s.prepareProjectDB(db, p)
		closeErr := db.Close()
		if err != nil {
			return fmt.Errorf("prepare project %q: %w", p.Name, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close project %q: %w", p.Name, closeErr)
		}
	}
	return nil
}

func (s *Store) prepareProjectDB(db *sql.DB, p Project) error {
	if err := s.initProjectSchema(db); err != nil {
		return err
	}
	tables, err := s.listTablesLocked(db)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if err := validateDataTable(db, table); err != nil {
			return err
		}
		if err := s.cleanupTableBlobs(db, p, table); err != nil {
			return err
		}
	}
	if err := validateNoOrphanDataTables(db, tables); err != nil {
		return err
	}
	return s.cleanupOrphanTableBlobDirs(p, tables)
}

func (s *Store) cleanupProjectDirs() error {
	projects, err := s.listProjectsLocked()
	if err != nil {
		return err
	}
	known := make(map[int64]struct{}, len(projects))
	for _, p := range projects {
		known[p.ID] = struct{}{}
	}
	entries, err := os.ReadDir(s.projectsDir)
	if err != nil {
		return fmt.Errorf("read projects dir: %w", err)
	}
	changed := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.projectsDir, entry.Name())); err != nil {
			return fmt.Errorf("remove orphan project dir %s: %w", entry.Name(), err)
		}
		changed = true
	}
	if changed {
		return syncDir(s.projectsDir)
	}
	return nil
}

func (s *Store) cleanupOrphanTableBlobDirs(p Project, tables []Table) error {
	root := filepath.Join(s.projectDir(p.ID), "blobs")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read table blob root: %w", err)
	}
	known := make(map[int64]struct{}, len(tables))
	for _, table := range tables {
		known[table.ID] = struct{}{}
	}
	changed := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove orphan table blob dir %s: %w", entry.Name(), err)
		}
		changed = true
	}
	if changed {
		return syncDir(root)
	}
	return nil
}

func (s *Store) cleanupTableBlobs(db *sql.DB, p Project, table Table) error {
	if !dataTableRe.MatchString(table.TableName) {
		return fmt.Errorf("%w: invalid data table %q", ErrInvalid, table.TableName)
	}
	refs := make(map[string]struct{})
	rows, err := db.Query(fmt.Sprintf(`SELECT DISTINCT blob_hash FROM "%s" WHERE blob_hash IS NOT NULL`, table.TableName))
	if err != nil {
		return fmt.Errorf("list blob refs for %s: %w", table.TableName, err)
	}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return fmt.Errorf("scan blob ref for %s: %w", table.TableName, err)
		}
		if !blobFileRe.MatchString(hash) {
			rows.Close()
			return fmt.Errorf("%w: invalid blob hash %q", ErrInvalid, hash)
		}
		refs[hash] = struct{}{}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("list blob refs for %s: %w", table.TableName, err)
	}

	dir := s.tableBlobDir(p, table)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read table blob dir %s: %w", dir, err)
	}
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		// 只清理本程序生成的内容寻址文件，未知文件保持原样。
		if entry.IsDir() || !blobFileRe.MatchString(name) {
			continue
		}
		if _, ok := refs[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove orphan blob %s: %w", name, err)
		}
		changed = true
	}
	if changed {
		return syncDir(dir)
	}
	return nil
}

// Close 关闭系统数据库。
func (s *Store) Close() error {
	return s.systemDB.Close()
}

// Ping 用于健康检查。
func (s *Store) Ping(ctx context.Context) error {
	return s.systemDB.PingContext(ctx)
}

// CreateAdminTempFile 在受管理的 data/tmp 中创建上传暂存文件。
func (s *Store) CreateAdminTempFile(pattern string) (*os.File, error) {
	return os.CreateTemp(filepath.Join(s.dataDir, "tmp"), pattern)
}

// MaxValueBytes 返回单 value 上限；0 表示不限制。
func (s *Store) MaxValueBytes() int64 {
	return s.maxValueBytes
}

func (s *Store) projectDir(projectID int64) string {
	return filepath.Join(s.projectsDir, strconv.FormatInt(projectID, 10))
}

func (s *Store) projectDBPath(projectID int64) string {
	return filepath.Join(s.projectDir(projectID), "project.db")
}

func (s *Store) tableBlobDir(p Project, table Table) string {
	return filepath.Join(s.projectDir(p.ID), "blobs", strconv.FormatInt(table.ID, 10))
}

func dataTableName(tableID int64) string {
	return "data_" + strconv.FormatInt(tableID, 10)
}

func dataTableSQL(table string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s" (
		key TEXT NOT NULL PRIMARY KEY,
		value BLOB,
		blob_hash TEXT,
		content_hash TEXT NOT NULL,
		content_type TEXT NOT NULL,
		size INTEGER NOT NULL,
		created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL,
		deleted INTEGER NOT NULL DEFAULT 0,
		CHECK ((value IS NULL) <> (blob_hash IS NULL))
	) WITHOUT ROWID`, table)
}

type sqlExecer interface {
	Exec(string, ...any) (sql.Result, error)
}

func createDataTableSchema(execer sqlExecer, table string) error {
	if !dataTableRe.MatchString(table) {
		return fmt.Errorf("%w: invalid data table %q", ErrInvalid, table)
	}
	if _, err := execer.Exec(dataTableSQL(table)); err != nil {
		return fmt.Errorf("create data table %s: %w", table, err)
	}
	for _, column := range []string{"updated_at", "deleted"} {
		indexName := fmt.Sprintf("idx_%s_%s", table, column)
		q := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS "%s" ON "%s" (%s)`, indexName, table, column)
		if _, err := execer.Exec(q); err != nil {
			return fmt.Errorf("create data index %s: %w", indexName, err)
		}
	}
	return nil
}

func validateDataTable(db *sql.DB, table Table) error {
	if !dataTableRe.MatchString(table.TableName) {
		return fmt.Errorf("%w: invalid data table %q", ErrInvalid, table.TableName)
	}
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table.TableName).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("data table %s for table %q is missing", table.TableName, table.Name)
	}
	if err != nil {
		return fmt.Errorf("inspect data table %s: %w", table.TableName, err)
	}

	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, table.TableName))
	if err != nil {
		return fmt.Errorf("inspect data table %s columns: %w", table.TableName, err)
	}
	defer rows.Close()
	columns := make(map[string]struct{})
	for rows.Next() {
		var (
			cid          int
			columnName   string
			columnType   string
			notNull      int
			defaultValue sql.NullString
			primaryKey   int
		)
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan data table %s columns: %w", table.TableName, err)
		}
		columns[columnName] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect data table %s columns: %w", table.TableName, err)
	}
	for _, column := range []string{
		"key", "value", "blob_hash", "content_hash", "content_type", "size",
		"created_at", "updated_at", "deleted",
	} {
		if _, ok := columns[column]; !ok {
			return fmt.Errorf("data table %s is missing column %s", table.TableName, column)
		}
	}
	keyCount, err := countTableKeys(db, table)
	if err != nil {
		return fmt.Errorf("count data table %s: %w", table.TableName, err)
	}
	if keyCount != table.KeyCount {
		return fmt.Errorf("data table %s key_count mismatch: metadata=%d actual=%d", table.TableName, table.KeyCount, keyCount)
	}
	return nil
}

func validateNoOrphanDataTables(db *sql.DB, tables []Table) error {
	registered := make(map[string]struct{}, len(tables))
	for _, table := range tables {
		registered[table.TableName] = struct{}{}
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'data_%'`)
	if err != nil {
		return fmt.Errorf("list physical data tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan physical data table: %w", err)
		}
		if _, ok := registered[name]; !ok {
			return fmt.Errorf("physical data table %s has no kv_tables metadata", name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list physical data tables: %w", err)
	}
	return nil
}

func (s *Store) openProjectDB(p Project) (*sql.DB, error) {
	dbPath := s.projectDBPath(p.ID)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("project db missing: %w", err)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func (s *Store) createProjectDatabase(p Project) error {
	dir := s.projectDir(p.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create project dir: %w", err)
	}
	db, err := openSQLite(s.projectDBPath(p.ID))
	if err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("create project db: %w", err)
	}
	err = s.initProjectSchema(db)
	closeErr := db.Close()
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	if closeErr != nil {
		os.RemoveAll(dir)
		return closeErr
	}
	if err := syncDir(dir); err != nil {
		os.RemoveAll(dir)
		return err
	}
	if err := syncDir(s.projectsDir); err != nil {
		os.RemoveAll(dir)
		return err
	}
	return nil
}

// ValidateProjectName 检查 project 名称是否符合约束。
func ValidateProjectName(name string) bool {
	return projectNameRe.MatchString(name)
}

// ValidateTableName 检查 table 名称是否符合约束。
func ValidateTableName(name string) bool {
	return tableNameRe.MatchString(name)
}

// ValidateKey 检查 key 是否可用作 HTTP 路径与管理页输入。
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key 不能为空", ErrInvalid)
	}
	if len(key) > maxKeyBytes {
		return fmt.Errorf("%w: key 不能超过 %d 字节", ErrInvalid, maxKeyBytes)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("%w: key 必须是合法 UTF-8", ErrInvalid)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: key 不能包含控制字符", ErrInvalid)
		}
	}
	// key 会作为 HTTP 路径的一部分；拒绝会被 net/http 路径清理改写的形态，
	// 否则 /a 与 //a 可能在重定向后落到不同的 key 上。
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("%w: key 不能以 / 开头或结尾", ErrInvalid)
	}
	for _, segment := range strings.Split(key, "/") {
		switch segment {
		case "":
			return fmt.Errorf("%w: key 不能包含连续斜杠", ErrInvalid)
		case ".", "..":
			return fmt.Errorf("%w: key 不能包含 . 或 .. 路径段", ErrInvalid)
		}
	}
	return nil
}

// NormalizeContentType 校验并返回可存储的 Content-Type。
func NormalizeContentType(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "application/octet-stream", nil
	}
	if len(v) > 255 {
		return "", fmt.Errorf("%w: Content-Type 过长", ErrInvalid)
	}
	if _, _, err := mime.ParseMediaType(v); err != nil {
		return "", fmt.Errorf("%w: Content-Type 不合法", ErrInvalid)
	}
	return v, nil
}

// CreateProject 创建 system.db 记录和独立 project.db，不创建任何 table。
func (s *Store) CreateProject(name string) (Project, string, error) {
	if !ValidateProjectName(name) {
		return Project{}, "", fmt.Errorf("%w: project 名称只能包含字母、数字、下划线、短横线，且以字母或数字开头，最长 32 字符", ErrInvalid)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	token, err := auth.GenerateToken()
	if err != nil {
		return Project{}, "", err
	}
	now := time.Now().Unix()

	tx, err := s.systemDB.Begin()
	if err != nil {
		return Project{}, "", err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO projects(name, created_at, updated_at)
		VALUES(?, ?, ?)`, name, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Project{}, "", fmt.Errorf("%w: project %q 已存在", ErrInvalid, name)
		}
		return Project{}, "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Project{}, "", err
	}
	p := Project{
		ID:        id,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := tx.Exec(`INSERT INTO project_tokens(project_id, name, token, token_hash, created_at)
		VALUES(?, ?, ?, ?, ?)`, id, "默认 Token", token, auth.HashToken(token), now); err != nil {
		return Project{}, "", err
	}
	if _, err := tx.Exec(`INSERT INTO project_usage(project_id, request_count) VALUES(?, 0)`, id); err != nil {
		return Project{}, "", err
	}
	if err := s.createProjectDatabase(p); err != nil {
		return Project{}, "", err
	}
	if err := tx.Commit(); err != nil {
		os.RemoveAll(s.projectDir(id))
		return Project{}, "", err
	}
	return p, token, nil
}

// GetProject 按名称查询 project。
func (s *Store) GetProject(name string) (Project, error) {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return s.getProjectLocked(name)
}

func (s *Store) getProjectLocked(name string) (Project, error) {
	p, err := scanProject(s.systemDB.QueryRow(`SELECT id, name, created_at, updated_at
		FROM projects WHERE name = ? COLLATE NOCASE`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

func (s *Store) listProjectsLocked() ([]Project, error) {
	rows, err := s.systemDB.Query(`SELECT id, name, created_at, updated_at
		FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return projects, nil
}

// ListProjects 返回全部 project 及 table、key 数量。
func (s *Store) ListProjects() ([]ProjectSummary, error) {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()

	projects, err := s.listProjectsLocked()
	if err != nil {
		return nil, err
	}
	requestTotals := make(map[int64]int64)
	rows, err := s.systemDB.Query(`SELECT project_id, request_count FROM project_usage`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var projectID, total int64
		if err := rows.Scan(&projectID, &total); err != nil {
			rows.Close()
			return nil, err
		}
		requestTotals[projectID] = total
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	recentTotals := make(map[int64]int64)
	recentCutoff := time.Now().Unix() - 5*60
	rows, err = s.systemDB.Query(`SELECT project_id, COALESCE(SUM(request_count), 0)
		FROM project_token_access WHERE second_start >= ?
		GROUP BY project_id`, recentCutoff)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var projectID, total int64
		if err := rows.Scan(&projectID, &total); err != nil {
			rows.Close()
			return nil, err
		}
		recentTotals[projectID] = total
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]ProjectSummary, 0, len(projects))
	for _, p := range projects {
		tableCount, keyCount, err := s.projectStats(p)
		if err != nil {
			return nil, fmt.Errorf("project %q stats: %w", p.Name, err)
		}
		out = append(out, ProjectSummary{
			Project:       p,
			TableCount:    tableCount,
			KeyCount:      keyCount,
			RequestCount:  requestTotals[p.ID],
			RecentRequest: recentTotals[p.ID],
		})
	}
	return out, nil
}

func (s *Store) projectStats(p Project) (int64, int64, error) {
	db, err := s.openProjectDB(p)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	tables, err := s.listTablesLocked(db)
	if err != nil {
		return 0, 0, err
	}
	var keyCount int64
	for _, table := range tables {
		keyCount += table.KeyCount
	}
	return int64(len(tables)), keyCount, nil
}

// DeleteProject 删除 system.db 记录和整个 project 目录。
func (s *Store) DeleteProject(name string) error {
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
	if _, err := tx.Exec(`DELETE FROM project_token_access WHERE project_id = ?`, p.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM project_tokens WHERE project_id = ?`, p.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM project_usage WHERE project_id = ?`, p.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM projects WHERE id = ?`, p.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// 项目记录已经删除；目录清理失败只留下无主文件，启动时会再次清理。
	_ = os.RemoveAll(s.projectDir(p.ID))
	return nil
}

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// CreateTable 在 project.db 中创建 table 元数据和一张独立 KV 物理表。
func (s *Store) CreateTable(p Project, name string) (Table, error) {
	if !ValidateTableName(name) {
		return Table{}, fmt.Errorf("%w: table 名称只能是小写字母、数字、下划线、短横线，且以字母或数字开头，最长 32 字符", ErrInvalid)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return Table{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return Table{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Table{}, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if _, err := getTableTx(tx, name); err == nil {
		return Table{}, ErrTableExists
	} else if !errors.Is(err, ErrTableNotFound) {
		return Table{}, err
	}
	table, err := createTableTx(tx, name, now)
	if err != nil {
		return Table{}, err
	}
	if err := tx.Commit(); err != nil {
		return Table{}, err
	}
	return table, nil
}

// ListTables 返回 project 内的全部 table 及 key 数量。
func (s *Store) ListTables(p Project) ([]TableSummary, error) {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return nil, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tables, err := s.listTablesLocked(db)
	if err != nil {
		return nil, err
	}
	out := make([]TableSummary, 0, len(tables))
	for _, table := range tables {
		out = append(out, TableSummary{Table: table})
	}
	return out, nil
}

func (s *Store) listTablesLocked(db *sql.DB) ([]Table, error) {
	rows, err := db.Query(`SELECT id, name, table_name, key_count, created_at, updated_at
		FROM kv_tables ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []Table
	for rows.Next() {
		table, err := scanTable(rows)
		if err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

func countTableKeys(db *sql.DB, table Table) (int64, error) {
	if !dataTableRe.MatchString(table.TableName) {
		return 0, fmt.Errorf("%w: invalid data table %q", ErrInvalid, table.TableName)
	}
	var n int64
	q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE deleted = 0`, table.TableName)
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// GetTable 按名称查询 project 内的 table。
func (s *Store) GetTable(p Project, name string) (Table, error) {
	if !ValidateTableName(name) {
		return Table{}, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return Table{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return Table{}, err
	}
	defer db.Close()
	return getTable(db, name)
}

// GetTableSummary 返回单个 table 及当前 key 数量。
func (s *Store) GetTableSummary(p Project, name string) (TableSummary, error) {
	if !ValidateTableName(name) {
		return TableSummary{}, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return TableSummary{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return TableSummary{}, err
	}
	defer db.Close()
	table, err := getTable(db, name)
	if err != nil {
		return TableSummary{}, err
	}
	return TableSummary{Table: table}, nil
}

// DeleteTable 删除 table 元数据、物理表与该 table 的外部大文件。
func (s *Store) DeleteTable(p Project, name string) error {
	if !ValidateTableName(name) {
		return fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	table, err := getTableTx(tx, name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, table.TableName)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM kv_tables WHERE id = ?`, table.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = os.RemoveAll(s.tableBlobDir(p, table))
	return nil
}

func getTable(db *sql.DB, name string) (Table, error) {
	table, err := scanTable(db.QueryRow(`SELECT id, name, table_name, key_count, created_at, updated_at
		FROM kv_tables WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Table{}, ErrTableNotFound
	}
	return table, err
}

func getTableTx(tx *sql.Tx, name string) (Table, error) {
	table, err := scanTable(tx.QueryRow(`SELECT id, name, table_name, key_count, created_at, updated_at
		FROM kv_tables WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Table{}, ErrTableNotFound
	}
	return table, err
}

func createTableTx(tx *sql.Tx, name string, now int64) (Table, error) {
	tmpTable := "data_creating_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	res, err := tx.Exec(`INSERT INTO kv_tables(name, table_name, key_count, created_at, updated_at)
		VALUES(?, ?, 0, ?, ?)`, name, tmpTable, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Table{}, ErrTableExists
		}
		return Table{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Table{}, err
	}
	tableName := dataTableName(id)
	if _, err := tx.Exec(`UPDATE kv_tables SET table_name = ? WHERE id = ?`, tableName, id); err != nil {
		return Table{}, err
	}
	if err := createDataTableSchema(tx, tableName); err != nil {
		return Table{}, err
	}
	return Table{
		ID:        id,
		Name:      name,
		TableName: tableName,
		KeyCount:  0,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func ensureTableTx(tx *sql.Tx, name string, now int64, create bool) (Table, bool, error) {
	table, err := getTableTx(tx, name)
	if err == nil {
		return table, false, nil
	}
	if !errors.Is(err, ErrTableNotFound) {
		return Table{}, false, err
	}
	if !create {
		return Table{}, false, ErrTableNotFound
	}
	table, err = createTableTx(tx, name, now)
	return table, true, err
}

func scanTable(row interface{ Scan(...any) error }) (Table, error) {
	var table Table
	err := row.Scan(&table.ID, &table.Name, &table.TableName, &table.KeyCount, &table.CreatedAt, &table.UpdatedAt)
	return table, err
}

func (s *Store) ensureProjectExists(id int64) error {
	var one int
	err := s.systemDB.QueryRow(`SELECT 1 FROM projects WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for sync %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync dir %s: %w", dir, err)
	}
	return nil
}
