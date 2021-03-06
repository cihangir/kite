package kontrol

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	sq "github.com/lann/squirrel"
	_ "github.com/lib/pq"

	"github.com/koding/kite"
	kontrolprotocol "github.com/koding/kite/kontrol/protocol"
	"github.com/koding/kite/protocol"
)

// Postgres holds Postgresql database related configuration
type PostgresConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	DBName   string
}

type Postgres struct {
	DB  *sql.DB
	Log kite.Logger
}

func NewPostgres(conf *PostgresConfig, log kite.Logger) *Postgres {
	if conf == nil {
		conf = &PostgresConfig{}
	}

	if conf.Port == 0 {
		conf.Port = 5432
	}

	if conf.Host == "" {
		conf.Host = "localhost"
	}

	if conf.DBName == "" {
		conf.DBName = os.Getenv("KONTROL_POSTGRES_DBNAME")
		if conf.DBName == "" {
			panic("db name is not set for postgres kontrol storage")
		}
	}

	connString := fmt.Sprintf(
		"host=%s port=%d dbname=%s sslmode=disable",
		conf.Host, conf.Port, conf.DBName,
	)

	if conf.Password != "" {
		connString += " password=" + conf.Password
	}

	if conf.Username == "" {
		conf.Username = os.Getenv("KONTROL_POSTGRES_USERNAME")
		if conf.Username == "" {
			panic("username is not set for postgres kontrol storage")
		}
	}

	connString += " user=" + conf.Username

	db, err := sql.Open("postgres", connString)
	if err != nil {
		panic(err)
	}

	// create our initial kite table
	// * url is containing the kite's register url
	// * id is going to be kites' unique id. We are adding it as a primary key
	// so each kite with the full path can only exist once.
	// * created_at and updated_at are updated at creation and updating (like
	//  if the URL has changed)
	table := `CREATE TABLE IF NOT EXISTS kite (
		username text NOT NULL,
		environment text NOT NULL,
		kitename text NOT NULL,
		version text NOT NULL,
		region text NOT NULL,
		hostname text NOT NULL,
		id uuid PRIMARY KEY,
		url text NOT NULL,
		created_at timestamptz NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC'),
		updated_at timestamptz NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')
	);`

	if _, err := db.Exec(table); err != nil {
		panic(err)
	}

	// We enable index on the kite and updated_at columns. We don't return on
	// errors because the operator `IF NOT EXISTS` doesn't work for index
	// creation, therefore we assume the indexes might be already created.
	enableBtreeIndex := `CREATE INDEX kite_updated_at_btree_idx ON kite USING BTREE(updated_at)`
	if _, err := db.Exec(enableBtreeIndex); err != nil {
		log.Warning("postgres: enable btree index: %s", err)
	}

	p := &Postgres{
		DB:  db,
		Log: log,
	}

	cleanInterval := 30 * time.Second  // clean every 30 second
	expireInterval := 20 * time.Second // clean rows that are 20 second old
	go p.RunCleaner(cleanInterval, expireInterval)

	return p
}

// RunCleaner delets every "interval" duration rows which are older than
// "expire" duration based on the "updated_at" field. For more info check
// CleanExpireRows which is used to delete old rows.
func (p *Postgres) RunCleaner(interval, expire time.Duration) {
	cleanFunc := func() {
		affectedRows, err := p.CleanExpiredRows(expire)
		if err != nil {
			p.Log.Warning("postgres: cleaning old rows failed: %s", err)
		} else if affectedRows != 0 {
			p.Log.Info("postgres: cleaned up %d rows", affectedRows)
		}
	}

	cleanFunc() // run for the first time
	for _ = range time.Tick(interval) {
		cleanFunc()
	}
}

// CleanExpiredRows deletes rows that are at least "expire" duration old. So if
// say an expire duration of 10 second is given, it will delete all rows that
// were updated 10 seconds ago
func (p *Postgres) CleanExpiredRows(expire time.Duration) (int64, error) {
	// See: http://stackoverflow.com/questions/14465727/how-to-insert-things-like-now-interval-2-minutes-into-php-pdo-query
	// basically by passing an integer to INTERVAL is not possible, we need to
	// cast it. However there is a more simpler way, we can multiply INTERVAL
	// with an integer so we just declare a one second INTERVAL and multiply it
	// with the amount we want.
	cleanOldRows := `DELETE FROM kite WHERE updated_at < (now() at time zone 'utc') - ((INTERVAL '1 second') * $1)`

	rows, err := p.DB.Exec(cleanOldRows, int64(expire/time.Second))
	if err != nil {
		return 0, err
	}

	return rows.RowsAffected()
}

func (p *Postgres) Get(query *protocol.KontrolQuery) (Kites, error) {
	// only let query with usernames, otherwise the whole tree will be fetched
	// which is not good for us
	sqlQuery, args, err := selectQuery(query)
	if err != nil {
		return nil, err
	}

	var hasVersionConstraint bool // does query contains a constraint on version?
	var keyRest string            // query key after the version field
	var versionConstraint version.Constraints
	// NewVersion returns an error if it's a constraint, like: ">= 1.0, < 1.4"
	_, err = version.NewVersion(query.Version)
	if err != nil && query.Version != "" {
		// now parse our constraint
		versionConstraint, err = version.NewConstraint(query.Version)
		if err != nil {
			// version is a malformed, just return the error
			return nil, err
		}

		hasVersionConstraint = true
		nameQuery := &protocol.KontrolQuery{
			Username:    query.Username,
			Environment: query.Environment,
			Name:        query.Name,
		}

		// We will make a get request to all nodes under this name
		// and filter the result later.
		sqlQuery, args, err = selectQuery(nameQuery)
		if err != nil {
			return nil, err
		}

		// Rest of the key after version field
		keyRest = "/" + strings.TrimRight(
			query.Region+"/"+query.Hostname+"/"+query.ID, "/")
	}

	rows, err := p.DB.Query(sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		username    string
		environment string
		kitename    string
		version     string
		region      string
		hostname    string
		id          string
		url         string
		updated_at  time.Time
		created_at  time.Time
	)

	kites := make(Kites, 0)

	for rows.Next() {
		err := rows.Scan(
			&username,
			&environment,
			&kitename,
			&version,
			&region,
			&hostname,
			&id,
			&url,
			&updated_at,
			&created_at,
		)
		if err != nil {
			return nil, err
		}

		kites = append(kites, &protocol.KiteWithToken{
			Kite: protocol.Kite{
				Username:    username,
				Environment: environment,
				Name:        kitename,
				Version:     version,
				Region:      region,
				Hostname:    hostname,
				ID:          id,
			},
			URL: url,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	// if it's just single result there is no need to shuffle or filter
	// according to the version constraint
	if len(kites) == 1 {
		return kites, nil
	}

	// Filter kites by version constraint
	if hasVersionConstraint {
		kites.Filter(versionConstraint, keyRest)
	}

	// randomize the result
	kites.Shuffle()

	return kites, nil
}

func (p *Postgres) Upsert(kiteProt *protocol.Kite, value *kontrolprotocol.RegisterValue) (err error) {
	// check that the incoming URL is valid to prevent malformed input
	_, err = url.Parse(value.URL)
	if err != nil {
		return err
	}

	// we are going to try an UPDATE, if it's not successfull we are going to
	// INSERT the document, all ine one single transaction
	tx, err := p.DB.Begin()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			err = tx.Rollback()
		} else {
			// it calls Rollback inside if it fails again :)
			err = tx.Commit()
		}
	}()

	res, err := tx.Exec(`UPDATE kite SET url = $1, updated_at = (now() at time zone 'utc') 
	WHERE id = $2`, value.URL, kiteProt.ID)
	if err != nil {
		return err
	}

	rowAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}

	// we got an update! so this was successfull, just return without an error
	if rowAffected != 0 {
		return nil
	}

	insertSQL, args, err := insertQuery(kiteProt, value.URL)
	if err != nil {
		return err
	}

	_, err = tx.Exec(insertSQL, args...)
	return err
}

func (p *Postgres) Add(kiteProt *protocol.Kite, value *kontrolprotocol.RegisterValue) error {
	// check that the incoming URL is valid to prevent malformed input
	_, err := url.Parse(value.URL)
	if err != nil {
		return err
	}

	sqlQuery, args, err := insertQuery(kiteProt, value.URL)
	if err != nil {
		return err
	}

	_, err = p.DB.Exec(sqlQuery, args...)
	return err
}

func (p *Postgres) Update(kiteProt *protocol.Kite, value *kontrolprotocol.RegisterValue) error {
	// check that the incoming url is valid to prevent malformed input
	_, err := url.Parse(value.URL)
	if err != nil {
		return err
	}

	// TODO: also consider just using WHERE id = kiteProt.ID, see how it's
	// performs out
	_, err = p.DB.Exec(`UPDATE kite SET url = $1, updated_at = (now() at time zone 'utc') 
	WHERE id = $2`,
		value.URL, kiteProt.ID)

	return err
}

func (p *Postgres) Delete(kiteProt *protocol.Kite) error {
	deleteKite := `DELETE FROM kite WHERE id = $1`
	_, err := p.DB.Exec(deleteKite, kiteProt.ID)
	return err
}

// selectQuery returns a SQL query for the given query
func selectQuery(query *protocol.KontrolQuery) (string, []interface{}, error) {
	psql := sq.StatementBuilder.PlaceholderFormat(sq.Dollar)

	kites := psql.Select("*").From("kite")
	fields := query.Fields()
	andQuery := sq.And{}

	// we stop for the first empty value
	for _, key := range keyOrder {
		v := fields[key]
		if v == "" {
			continue
		}

		// we are using "kitename" as the columname
		if key == "name" {
			key = "kitename"
		}

		andQuery = append(andQuery, sq.Eq{key: v})
	}

	if len(andQuery) == 0 {
		return "", nil, errors.New("all query fields are empty")
	}

	return kites.Where(andQuery).ToSql()
}

// inseryQuery
func insertQuery(kiteProt *protocol.Kite, url string) (string, []interface{}, error) {
	psql := sq.StatementBuilder.PlaceholderFormat(sq.Dollar)

	kiteValues := kiteProt.Values()
	values := make([]interface{}, len(kiteValues))

	for i, kiteVal := range kiteValues {
		values[i] = kiteVal
	}

	values = append(values, url)

	return psql.Insert("kite").Columns(
		"username",
		"environment",
		"kitename",
		"version",
		"region",
		"hostname",
		"id",
		"url",
	).Values(values...).ToSql()
}
