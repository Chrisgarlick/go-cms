// Package devdb runs a real Postgres from embedded binaries for `cms dev` and
// tests. Production never uses it: servers run a native Postgres (spec 9.7).
package devdb

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
)

const (
	superUser    = "postgres"
	superPass    = "postgres"
	MigratorRole = "cms_migrator"
	AppRole      = "cms_app"
	devPassword  = "dev-only-password"
)

type Cluster struct {
	pg   *embeddedpostgres.EmbeddedPostgres
	Port uint32
	dir  string
}

// Start launches Postgres 18 with its data under dir. Binaries are cached in
// ~/.embedded-postgres-go after the first download.
func Start(dir string, port uint32, logw io.Writer) (*Cluster, error) {
	if port == 0 {
		p, err := freePort()
		if err != nil {
			return nil, err
		}
		port = p
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if logw == nil {
		logw = io.Discard
	}
	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V18).
		Port(port).
		Username(superUser).Password(superPass).Database("postgres").
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		BinariesPath(filepath.Join(dir, "bin")).
		StartTimeout(60 * time.Second).
		Logger(logw).
		StartParameters(map[string]string{
			"shared_buffers":     "64MB",
			"max_connections":    "100",
			"fsync":              "off", // dev/test only
			"synchronous_commit": "off",
		})
	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		return nil, fmt.Errorf("devdb: start postgres: %w", err)
	}
	return &Cluster{pg: pg, Port: port, dir: dir}, nil
}

func (c *Cluster) Stop() error { return c.pg.Stop() }

func (c *Cluster) url(user, pass, db string) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, pass, c.Port, db)
}

func (c *Cluster) SuperURL(db string) string    { return c.url(superUser, superPass, db) }
func (c *Cluster) MigratorURL(db string) string { return c.url(MigratorRole, devPassword, db) }
func (c *Cluster) AppURL(db string) string      { return c.url(AppRole, devPassword, db) }

// EnsureDatabase creates the two roles (as production does) and a database
// owned by the migrator. Idempotent.
func (c *Cluster) EnsureDatabase(ctx context.Context, name string) error {
	return EnsureRolesAndDatabase(ctx, c.SuperURL("postgres"), name, MigratorRole, AppRole, devPassword, devPassword)
}

// CreateFromTemplate makes a new database by copying a migrated template.
func (c *Cluster) CreateFromTemplate(ctx context.Context, name, template string) error {
	conn, err := pgx.Connect(ctx, c.SuperURL("postgres"))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s OWNER %s",
		pgx.Identifier{name}.Sanitize(), pgx.Identifier{template}.Sanitize(), MigratorRole))
	return err
}

func (c *Cluster) DropDatabase(ctx context.Context, name string) error {
	conn, err := pgx.Connect(ctx, c.SuperURL("postgres"))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	return err
}

// EnsureRolesAndDatabase is shared with `cms install`, which receives a
// superuser URL once and never stores it.
func EnsureRolesAndDatabase(ctx context.Context, superURL, dbName, migrator, app, migratorPass, appPass string) error {
	conn, err := pgx.Connect(ctx, superURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for _, r := range []struct{ name, pass string }{{migrator, migratorPass}, {app, appPass}} {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", r.name).Scan(&exists); err != nil {
			return err
		}
		verb := "CREATE"
		if exists {
			verb = "ALTER"
		}
		// Passwords cannot be bound parameters in DDL; quote as a literal.
		stmt := fmt.Sprintf("%s ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD %s",
			verb, pgx.Identifier{r.name}.Sanitize(), quoteLiteral(r.pass))
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("role %s: %w", r.name, err)
		}
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", dbName).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s OWNER %s",
			pgx.Identifier{dbName}.Sanitize(), pgx.Identifier{migrator}.Sanitize())); err != nil {
			return err
		}
	}
	_, err = conn.Exec(ctx, fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM PUBLIC; GRANT CONNECT ON DATABASE %s TO %s",
		pgx.Identifier{dbName}.Sanitize(), pgx.Identifier{dbName}.Sanitize(), pgx.Identifier{app}.Sanitize()))
	return err
}

func quoteLiteral(s string) string {
	out := []rune{'\''}
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'')
		}
		out = append(out, r)
	}
	return string(append(out, '\''))
}

func freePort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}
