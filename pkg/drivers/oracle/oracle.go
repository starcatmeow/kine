package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/k3s-io/kine/pkg/drivers"
	"github.com/k3s-io/kine/pkg/drivers/generic"
	"github.com/k3s-io/kine/pkg/logstructured"
	"github.com/k3s-io/kine/pkg/logstructured/sqllog"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/k3s-io/kine/pkg/util"
	_ "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/network"
	"github.com/sirupsen/logrus"
	"os"
	"regexp"
	"strconv"
)

var (
	schema = []string{
		`CREATE TABLE kine
			(
				id INTEGER GENERATED BY DEFAULT AS IDENTITY,
				name VARCHAR2(630),
				created INTEGER,
				deleted INTEGER,
				create_revision INTEGER,
				prev_revision INTEGER,
				lease INTEGER,
				value BLOB,
				old_value BLOB,
				CONSTRAINT kine_pk PRIMARY KEY (id)
			)`,
		`CREATE INDEX kine_name_index ON kine (name)`,
		`CREATE INDEX kine_name_id_index ON kine (name,id)`,
		`CREATE INDEX kine_id_deleted_index ON kine (id,deleted)`,
		`CREATE INDEX kine_prev_revision_index ON kine (prev_revision)`,
		`CREATE UNIQUE INDEX kine_name_prev_revision_uindex ON kine (name, prev_revision)`,
	}
	schemaMigrations = []string{
		``,
		// Creating an empty migration to ensure that postgresql and mysql migrations match up
		// with each other for a give value of KINE_SCHEMA_MIGRATION env var
		``,
	}
)

var (
	columns = "kv.id AS theid, kv.name AS thename, kv.created, kv.deleted, kv.create_revision, kv.prev_revision, kv.lease, kv.value, kv.old_value"
	revSQL  = `
		SELECT MAX(rkv.id) AS id
		FROM kine rkv`

	compactRevSQL = `
		SELECT MAX(crkv.prev_revision) AS prev_revision
		FROM kine crkv
		WHERE crkv.name = 'compact_rev_key'`

	listSQL = fmt.Sprintf(`
		SELECT *
		FROM (
			SELECT (%s), (%s), %s
			FROM kine kv
			JOIN (
				SELECT MAX(mkv.id) AS id
				FROM kine mkv
				WHERE
					mkv.name LIKE ?
					%%s
				GROUP BY mkv.name) maxkv
				ON maxkv.id = kv.id
			WHERE
				kv.deleted = 0 OR
				kv.deleted = ?
		) lkv
		ORDER BY lkv.thename ASC
		`, revSQL, compactRevSQL, columns)
)

func New(ctx context.Context, cfg *drivers.Config) (bool, server.Backend, error) {
	dialect, err := generic.Open(ctx, "oracle", cfg.Endpoint, cfg.ConnectionPoolConfig, ":", true, cfg.MetricsRegisterer)
	if err != nil {
		return false, nil, err
	}
	dialect.GetRevisionSQL = q(fmt.Sprintf(`
			SELECT
			0, 0, %s
			FROM kine kv
			WHERE kv.id = ?`, columns))
	dialect.GetCurrentSQL = q(fmt.Sprintf(listSQL, "AND mkv.name > NVL(?, CHR(1))"))
	dialect.ListRevisionStartSQL = q(fmt.Sprintf(listSQL, "AND mkv.id <= ?"))
	dialect.GetRevisionAfterSQL = q(fmt.Sprintf(listSQL, "AND mkv.name > ? AND mkv.id <= ?"))
	dialect.CountCurrentSQL = q(fmt.Sprintf(`
			SELECT (%s), (SELECT COUNT(c.theid)
			FROM (
				%s
			) c) FROM dual`, revSQL, fmt.Sprintf(listSQL, "AND mkv.name > NVL(?, CHR(1))")))
	dialect.CountRevisionSQL = q(fmt.Sprintf(`
			SELECT (%s), (SELECT COUNT(c.theid)
			FROM (
				%s
			) c) FROM dual`, revSQL, fmt.Sprintf(listSQL, "AND mkv.name > NVL(?, CHR(1)) AND mkv.id <= ?")))
	dialect.AfterSQL = q(fmt.Sprintf(`
			SELECT (%s), (%s), %s
			FROM kine kv
			WHERE
				kv.name LIKE ? AND
				kv.id > ?
			ORDER BY kv.id ASC`, revSQL, compactRevSQL, columns))
	dialect.DeleteSQL = q(`
			DELETE FROM kine kv
			WHERE kv.id = ?`)
	dialect.LimitSQL = "%s FETCH FIRST %d ROWS ONLY"
	dialect.RevisionSQL = revSQL
	dialect.CompactRevisionSQL = compactRevSQL
	dialect.TranslateErr = func(err error) error {
		if err, ok := err.(*network.OracleError); ok && err.ErrCode == 1 {
			return server.ErrKeyExists
		}
		return err
	}
	dialect.ErrCode = func(err error) string {
		if err == nil {
			return ""
		}
		if err, ok := err.(*network.OracleError); ok {
			return fmt.Sprint(err.ErrCode)
		}
		return err.Error()
	}
	dialect.InsertReturningInto = true
	dialect.IsolationLevel = sql.LevelDefault
	if err := setup(dialect.DB); err != nil {
		return false, nil, err
	}
	dialect.Migrate(context.Background())
	return true, logstructured.New(sqllog.New(dialect)), nil
}

func setup(db *sql.DB) error {
	logrus.Infof("Configuring database table schema and indexes, this may take a moment...")
	var exists bool
	err := db.QueryRow("SELECT 1 FROM USER_TABLES WHERE table_name = :1", "KINE").Scan(&exists)
	if err != nil && err != sql.ErrNoRows {
		logrus.Warnf("Failed to check existence of database table %s, going to attempt create: %v", "kine", err)
	}

	if !exists {
		for _, stmt := range schema {
			logrus.Tracef("SETUP EXEC : %v", util.Stripped(stmt))
			if _, err := db.Exec(stmt); err != nil {
				return err
			}
		}
	}

	// Run enabled schama migrations.
	// Note that the schema created by the `schema` var is always the latest revision;
	// migrations should handle deltas between prior schema versions.
	schemaVersion, _ := strconv.ParseUint(os.Getenv("KINE_SCHEMA_MIGRATION"), 10, 64)
	for i, stmt := range schemaMigrations {
		if i >= int(schemaVersion) {
			break
		}
		if stmt == "" {
			continue
		}
		logrus.Tracef("SETUP EXEC MIGRATION %d: %v", i, util.Stripped(stmt))
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}

	logrus.Infof("Database tables and indexes are up to date")
	return nil
}

func q(sql string) string {
	regex := regexp.MustCompile(`\?`)
	pref := ":"
	n := 0
	return regex.ReplaceAllStringFunc(sql, func(string) string {
		n++
		return pref + strconv.Itoa(n)
	})
}

func init() {
	drivers.Register("oracle", New)
}
