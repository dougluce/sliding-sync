package state

import (
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestEventTable(t *testing.T) {
	db, err := sqlx.Open("postgres", "user=kegan dbname=syncv3 sslmode=disable")
	if err != nil {
		t.Fatalf("failed to open SQL db: %s", err)
	}
	txn, err := db.Beginx()
	if err != nil {
		t.Fatalf("failed to start txn: %s", err)
	}
	defer txn.Rollback()
	table := NewEventTable(db)
	events := []Event{
		{
			ID:   "100",
			JSON: []byte(`{"event_id":"100", "foo":"bar"}`),
		},
		{
			ID:   "101",
			JSON: []byte(`{"event_id":"101",  "foo":"bar"}`),
		},
		{
			// ID is optional, it will pull event_id out if it's missing
			JSON: []byte(`{"event_id":"102", "foo":"bar"}`),
		},
	}
	numNew, err := table.Insert(txn, events)
	if err != nil {
		t.Fatalf("Insert failed: %s", err)
	}
	if numNew != len(events) {
		t.Fatalf("wanted %d new events, got %d", len(events), numNew)
	}
	// duplicate insert is ok
	numNew, err = table.Insert(txn, events)
	if err != nil {
		t.Fatalf("Insert failed: %s", err)
	}
	if numNew != 0 {
		t.Fatalf("wanted 0 new events, got %d", numNew)
	}

	// pulling non-existent ids returns no error but a zero slice
	events, err = table.SelectByIDs(txn, []string{"101010101010"})
	if err != nil {
		t.Fatalf("SelectByIDs failed: %s", err)
	}
	if len(events) != 0 {
		t.Fatalf("SelectByIDs returned %d events, wanted none", len(events))
	}

	// pulling events by event_id is ok
	events, err = table.SelectByIDs(txn, []string{"100", "101", "102"})
	if err != nil {
		t.Fatalf("SelectByIDs failed: %s", err)
	}
	if len(events) != 3 {
		t.Fatalf("SelectByIDs returned %d events, want 3", len(events))
	}

	// pulling nids by event_id is ok
	gotnids, err := table.SelectNIDsByIDs(txn, []string{"100", "101", "102"})
	if err != nil {
		t.Fatalf("SelectNIDsByIDs failed: %s", err)
	}
	if len(gotnids) != 3 {
		t.Fatalf("SelectNIDsByIDs returned %d events, want 3", len(gotnids))
	}

	var nids []int64
	for _, ev := range events {
		nids = append(nids, int64(ev.NID))
	}
	// pulling events by event nid is ok
	events, err = table.SelectByNIDs(txn, nids)
	if err != nil {
		t.Fatalf("SelectByNIDs failed: %s", err)
	}
	if len(events) != 3 {
		t.Fatalf("SelectByNIDs returned %d events, want 3", len(events))
	}

	// pulling non-existent nids returns no error but a zero slice
	events, err = table.SelectByNIDs(txn, []int64{9999999})
	if err != nil {
		t.Fatalf("SelectByNIDs failed: %s", err)
	}
	if len(events) != 0 {
		t.Fatalf("SelectByNIDs returned %d events, wanted none", len(events))
	}

}