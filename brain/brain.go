package brain

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/zephyrtronium/crazy"

	_ "github.com/mattn/go-sqlite3" // for driver
)

// Brain learns Markov chains over arbitrary and generates text from them. It
// also manages channel configuration, history, and quotes.
type Brain struct {
	// db is the database connection.
	db *sql.DB
	// order is the number of words per chain, including prefix and suffix.
	order int
	// me is the bot's Twitch username.
	me string
	// lme is the bot's Twitch username converted to lower case.
	lme string

	// cfgs is the in-memory store of per-channel configurations.
	cmu  sync.Mutex
	cfgs map[string]*chancfg

	// emotes is the in-memory store of per-tag emotes.
	emu    sync.Mutex
	emotes map[string]emopt

	// rng is the randomness source for the brain.
	rmu sync.Mutex
	rng *crazy.MT64

	stmts brainStmts
	opts  sync.Pool
}

type brainStmts struct {
	// learn is the statement to add a single tuple to the DB. First parameter
	// is the tag, then (order+1) more for the tuple and suffix. This statement
	// should be used with Exec in a Tx with record.
	learn *sql.Stmt
	// record is the statement to add a message to the history. Parameters are,
	// in order, id, time, sender, channel, tag, message. This statement should
	// be used with Exec in a Tx with learn.
	record *sql.Stmt
	// think is the statements to match a tuple and retrieve suffixes. First
	// parameter is the tag, then up to (order) more for the tuple. This
	// statement should be used with Query. There may be any number of results,
	// each a single text, or null to indicate end of walk. The elements of
	// think are for queries of descending size, starting with a full chain of
	// size (order), then (order-1), down to 2.
	think []*sql.Stmt
	// thought is the statement to register a generated message. Parameters are
	// tag and message. This statement should be used with Exec.
	thought *sql.Stmt
	// familiar is the statement to select messages that seem familiar.
	// Parameters are tag and message. This statement should be used with
	// QueryRow. The result is the number of distinct matching messages.
	familiar *sql.Stmt
	// historyID is the statement to select a message from history by Twitch
	// message ID. The only parameter is the ID. This statement should be used
	// with QueryRow. The result is the rowid, tag, and message. Generally this
	// statement would be paired with forgets and an expunge in a Tx.
	historyID *sql.Stmt
	// historyName is the statement to select all messages from history by
	// sender name. The parameters are channel and name. This statement should
	// be used with Query. The results are rowid, tag, and message. Generally
	// this statement would be paired with forgets and expunges in a Tx.
	historyName *sql.Stmt
	// historyPattern is the statement to select all messages from history by
	// partial message text. The parameters are the channel and message
	// pattern. This statement should be used with Query. The results are
	// rowid, tag, and the full matched message text. Generally this statement
	// would be paired with forgets and expunges in a TX.
	historyPattern *sql.Stmt
	// forget is the statement to delete tuples from the DB. First parameter is
	// is the tag, then (order+1) more for the tuple and suffix. This statement
	// should be used with Exec in a Tx with expunge.
	forget *sql.Stmt
	// expunge is the statement to delete messages by rowid from
	// the history. The only parameter is the ID. This statement should be used
	// with Exec.
	expunge *sql.Stmt
}

type optfreq struct {
	n int64
	w sql.NullString
}

type emopt struct {
	s int64
	e []optfreq
}

// Open loads a brain database by connecting to source, using the last settings
// set by Configure for username and order. If no such settings have been set,
// then the returned error is sql.ErrNoRows. The source must have all tables
// initialized, as by Configure.
func Open(ctx context.Context, source string) (*Brain, error) {
	db, err := sql.Open("sqlite3", source)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	row := db.QueryRowContext(ctx, `SELECT me, pfix FROM config WHERE id=1`)
	var me string
	var order int
	if err := row.Scan(&me, &order); err != nil {
		return nil, err
	}
	br := &Brain{
		db:    db,
		order: order,
		me:    me,
		lme:   strings.ToLower(me),
		stmts: prepStmts(ctx, db, order),
		opts:  sync.Pool{New: func() interface{} { return []optfreq{} }},
	}
	if err := br.UpdateAll(ctx); err != nil {
		return nil, err
	}
	rng := crazy.NewMT64()
	crazy.CryptoSeeded(rng, 8)
	br.rng = rng
	return br, nil
}

// Configure loads a brain database with the given username and order by
// connecting to source. In the current implementation, source should be the
// path of an SQLite database.
func Configure(ctx context.Context, source, me string, order int) (*Brain, error) {
	db, err := sql.Open("sqlite3", source)
	// TODO: wrap errors
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=wal`); err != nil {
		// This is not a failure condition, esp. if we eventually stop using sqlite.
		// TODO: still complain tho
	}
	if _, err := db.ExecContext(ctx, makeTables(order)); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO config(id, me, pfix) VALUES (1, ?, ?)`, me, order); err != nil {
		return nil, err
	}
	br := &Brain{
		db:    db,
		order: order,
		me:    me,
		lme:   strings.ToLower(me),
		stmts: prepStmts(ctx, db, order),
		opts:  sync.Pool{New: func() interface{} { return []optfreq{} }},
	}
	if err := br.UpdateAll(ctx); err != nil {
		return nil, err
	}
	rng := crazy.NewMT64()
	crazy.CryptoSeeded(rng, 8)
	br.rng = rng
	return br, nil
}

// Name returns the username associated with the brain.
func (b *Brain) Name() string {
	return b.me
}

func (b *Brain) intn(s int64) int64 {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	bad := ^uint64(0) - ^uint64(0)%uint64(s)
	x := b.rng.Uint64()
	for x > bad {
		x = b.rng.Uint64()
	}
	return int64(x % uint64(s))
}

func (b *Brain) unifm() float64 {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	x := b.rng.Uint64() & 0x1fffffffffffff
	return float64(x) * 1.11022302462515654042e-16
}

const sqlSetup = `
CREATE TABLE IF NOT EXISTS config (
	id		INTEGER PRIMARY KEY ASC,
	me		TEXT NOT NULL, -- bot nickname
	pfix	INTEGER NOT NULL, -- Markov chain prefix length
	block	TEXT -- global default 
);
-------------------------------------------------------------------
-- If chans or privs change, update hupChans, hupPrivs, hup1Chan --
-------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS chans (
	name	TEXT PRIMARY KEY, -- channel name to join
	learn	TEXT, -- tag for learned messages, or don't learn if null
	send	TEXT, -- tag for talking, or don't talk if null
	lim		INTEGER NOT NULL DEFAULT 511, -- limit on size of messages in bytes
	prob	REAL NOT NULL DEFAULT 0 CHECK (prob BETWEEN 0 AND 1), -- probability of talking
	rate	REAL NOT NULL DEFAULT 0.5 CHECK (rate >= 0), -- average messages per second
	burst	INTEGER NOT NULL DEFAULT 1, -- message burst size
	block	TEXT, -- regex for messages to ignore, if non-null
	respond	BOOLEAN NOT NULL DEFAULT FALSE, -- whether to always respond when addressed
	silence	DATETIME -- never try to talk before this time if non-null
);
CREATE TABLE IF NOT EXISTS privs (
	user	TEXT NOT NULL, -- username to which this priv applies
	chan	TEXT, -- null means this priv applies everywhere
	priv	TEXT NOT NULL, -- "owner", "admin", "bot", or "ignore"
	UNIQUE(user, chan)
);
CREATE TABLE IF NOT EXISTS history (
	id		INTEGER PRIMARY KEY ASC,
	tid		TEXT, -- message id from Twitch tags
	time	DATETIME NOT NULL, -- message timestamp
	sender	TEXT NOT NULL, -- name of sender converted to lowercase
	chan	TEXT NOT NULL,
	tag		TEXT NOT NULL, -- tag used to learn this message
	msg		TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS generated (
	time	DATETIME NOT NULL, -- timestamp of generated message
	tag		TEXT NOT NULL, -- send tag sent to
	msg		TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS quotes (
	id		INTEGER PRIMARY KEY ASC, -- quote id number
	time	DATETIME NOT NULL, -- quoted timestamp
	msg		TEXT NOT NULL,
	blame	TEXT NOT NULL -- who added the quote
);
CREATE TABLE IF NOT EXISTS emotes (
	id		INTEGER PRIMARY KEY ASC,
	tag		TEXT, -- send tag where used, or everywhere if null
	emote	TEXT,
	weight	INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS history_id_index ON history(tid);
CREATE INDEX IF NOT EXISTS history_sender_index ON history(chan, sender);
CREATE TRIGGER IF NOT EXISTS history_limit AFTER INSERT ON history BEGIN
	DELETE FROM history WHERE strftime('%s', time) < strftime('%s', 'now', '-15 minutes');
END;
CREATE TRIGGER IF NOT EXISTS generated_limit AFTER INSERT ON generated BEGIN
	DELETE FROM generated WHERE strftime('%s', time) < strftime('%s', 'now', '-15 minutes');
END;
`

func makeTables(order int) string {
	var b strings.Builder
	b.WriteString(sqlSetup)
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS tuples%d (id INTEGER PRIMARY KEY ASC, tag TEXT NOT NULL,", order)
	writeCols(&b, order, "TEXT")
	b.WriteString("suffix TEXT);")
	fmt.Fprintf(&b, `CREATE INDEX IF NOT EXISTS tuples%[1]d_tag_index ON tuples%[1]d(tag);`, order)
	fmt.Fprintf(&b, `CREATE INDEX IF NOT EXISTS tuples%[1]d_pn_index ON tuples%[1]d(p%[2]d);`, order, order-1)
	return b.String()
}

func makeLearn(order int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "INSERT INTO tuples%d (tag,", order)
	writeCols(&b, order, "")
	b.WriteString("suffix) VALUES (?,")
	b.WriteString(strings.Repeat("?,", order))
	b.WriteString("?);")
	return b.String()
}

func makeForget(order int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DELETE FROM tuples%[1]d WHERE id IN (SELECT id FROM tuples%[1]d WHERE tag=?", order)
	writeTupleMatch(&b, order, 0)
	b.WriteString(" AND suffix=? LIMIT 1);")
	return b.String()
}

func writeTupleMatch(b *strings.Builder, order, start int) {
	for j := 0; j < start; j++ {
		fmt.Fprintf(b, " AND p%d IS NOT ?", j)
	}
	for j := start; j < order; j++ {
		fmt.Fprintf(b, " AND p%d IS ?", j)
	}
}

func writeCols(b *strings.Builder, order int, suf string) {
	for i := 0; i < order; i++ {
		fmt.Fprintf(b, "p%d %s,", i, suf)
	}
}

func prepStmts(ctx context.Context, db *sql.DB, order int) brainStmts {
	stmts := brainStmts{}
	var err error
	stmts.learn, err = db.PrepareContext(ctx, makeLearn(order))
	if err != nil {
		panic(err)
	}
	stmts.record, err = db.PrepareContext(ctx, `INSERT INTO history (tid, time, sender, chan, tag, msg) VALUES (?, ?, ?, ?, ?, ?);`)
	if err != nil {
		panic(err)
	}
	stmts.think = make([]*sql.Stmt, 0, order)
	for i := 0; i < order-1; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, "SELECT DISTINCT suffix, COUNT(*)+%d FROM tuples%d WHERE tag=?", order*order-i*i, order)
		writeTupleMatch(&b, order, i)
		b.WriteString(" GROUP BY suffix;")
		s, err := db.PrepareContext(ctx, b.String())
		if err != nil {
			panic(err)
		}
		stmts.think = append(stmts.think, s)
	}
	stmts.thought, err = db.PrepareContext(ctx, `INSERT INTO generated(time, tag, msg) VALUES (strftime('%s', 'now'), ?, ?)`)
	if err != nil {
		panic(err)
	}
	stmts.familiar, err = db.PrepareContext(ctx, `SELECT COUNT(*) FROM generated WHERE tag=? AND ? GLOB msg || '*'`)
	if err != nil {
		panic(err)
	}
	stmts.historyID, err = db.PrepareContext(ctx, `SELECT id, tag, msg FROM history WHERE tid=?`)
	if err != nil {
		panic(err)
	}
	stmts.historyName, err = db.PrepareContext(ctx, `SELECT id, tag, msg FROM history WHERE chan=? AND sender=?`)
	if err != nil {
		panic(err)
	}
	stmts.historyPattern, err = db.PrepareContext(ctx, `SELECT id, tag, msg FROM history WHERE chan=? AND msg LIKE '%' || ? || '%'`)
	if err != nil {
		panic(err)
	}
	stmts.forget, err = db.PrepareContext(ctx, makeForget(order))
	if err != nil {
		panic(err)
	}
	stmts.expunge, err = db.PrepareContext(ctx, `DELETE FROM history WHERE id=?`)
	if err != nil {
		panic(err)
	}
	return stmts
}

// Close closes the brain's database connection.
func (b *Brain) Close() error {
	return b.db.Close()
}

// Exec executes a generic SQL statement on the brain's database.
func (b *Brain) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return b.db.ExecContext(ctx, query, args...)
}

// Query executes a generic SQL query on the brain's database.
func (b *Brain) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return b.db.QueryContext(ctx, query, args...)
}

// QueryRow executes a generic SQL query on the brain's database, expecting at
// most one resulting row.
func (b *Brain) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return b.db.QueryRowContext(ctx, query, args...)
}
