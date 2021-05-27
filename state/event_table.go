package state

import (
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/tidwall/gjson"
)

type Event struct {
	NID  int    `db:"event_nid"`
	ID   string `db:"event_id"`
	JSON []byte `db:"event"`
}

// EventTable stores events. A unique numeric ID is associated with each event.
type EventTable struct {
}

// NewEventTable makes a new EventTable
func NewEventTable(db *sqlx.DB) *EventTable {
	// make sure tables are made
	db.MustExec(`
	CREATE SEQUENCE IF NOT EXISTS syncv3_event_nids_seq;
	CREATE TABLE IF NOT EXISTS syncv3_events (
		event_nid BIGINT PRIMARY KEY NOT NULL DEFAULT nextval('syncv3_event_nids_seq'),
		event_id TEXT NOT NULL UNIQUE,
		event JSONB NOT NULL
	);
	`)
	return &EventTable{}
}

// Insert events into the event table. Returns the number of rows added. If the number of rows is >0,
// and the list of events is in sync stream order, it can be inferred that the last element(s) are new.
func (t *EventTable) Insert(txn *sqlx.Tx, events []Event) (int, error) {
	// ensure event_id is set
	for i := range events {
		ev := events[i]
		if ev.ID != "" {
			continue
		}
		eventIDResult := gjson.GetBytes(ev.JSON, "event_id")
		if !eventIDResult.Exists() || eventIDResult.Str == "" {
			return 0, fmt.Errorf("event JSON missing event_id key")
		}
		ev.ID = eventIDResult.Str
		events[i] = ev
	}
	result, err := txn.NamedExec(`INSERT INTO syncv3_events (event_id, event)
        VALUES (:event_id, :event) ON CONFLICT (event_id) DO NOTHING`, events)
	if err != nil {
		return 0, err
	}
	ra, err := result.RowsAffected()
	return int(ra), err
}

func (t *EventTable) SelectByNIDs(txn *sqlx.Tx, nids []int64) (events []Event, err error) {
	query, args, err := sqlx.In("SELECT event_nid, event_id, event FROM syncv3_events WHERE event_nid IN (?) ORDER BY event_nid ASC;", nids)
	query = txn.Rebind(query)
	if err != nil {
		return nil, err
	}
	err = txn.Select(&events, query, args...)
	return
}

func (t *EventTable) SelectByIDs(txn *sqlx.Tx, ids []string) (events []Event, err error) {
	query, args, err := sqlx.In("SELECT event_nid, event_id, event FROM syncv3_events WHERE event_id IN (?);", ids)
	query = txn.Rebind(query)
	if err != nil {
		return nil, err
	}
	err = txn.Select(&events, query, args...)
	return
}

func (t *EventTable) SelectNIDsByIDs(txn *sqlx.Tx, ids []string) (nids []int64, err error) {
	query, args, err := sqlx.In("SELECT event_nid FROM syncv3_events WHERE event_id IN (?);", ids)
	query = txn.Rebind(query)
	if err != nil {
		return nil, err
	}
	err = txn.Select(&nids, query, args...)
	return
}