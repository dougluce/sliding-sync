package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/getsentry/sentry-go"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/matrix-org/sliding-sync/internal"
	"github.com/matrix-org/sliding-sync/sqlutil"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
)

var logger = zerolog.New(os.Stdout).With().Timestamp().Logger().Output(zerolog.ConsoleWriter{
	Out:        os.Stderr,
	TimeFormat: "15:04:05",
})

// Max number of parameters in a single SQL command
const MaxPostgresParameters = 65535

// StartupSnapshot represents a snapshot of startup data for the sliding sync HTTP API instances
type StartupSnapshot struct {
	GlobalMetadata   map[string]internal.RoomMetadata // room_id -> metadata
	AllJoinedMembers map[string][]string              // room_id -> [user_id]
}

type Storage struct {
	accumulator       *Accumulator
	EventsTable       *EventTable
	ToDeviceTable     *ToDeviceTable
	UnreadTable       *UnreadTable
	AccountDataTable  *AccountDataTable
	InvitesTable      *InvitesTable
	TransactionsTable *TransactionsTable
	DeviceDataTable   *DeviceDataTable
	ReceiptTable      *ReceiptTable
	DB                *sqlx.DB
}

func NewStorage(postgresURI string) *Storage {
	db, err := sqlx.Open("postgres", postgresURI)
	if err != nil {
		sentry.CaptureException(err)
		// TODO: if we panic(), will sentry have a chance to flush the event?
		logger.Panic().Err(err).Str("uri", postgresURI).Msg("failed to open SQL DB")
	}
	acc := &Accumulator{
		db:            db,
		roomsTable:    NewRoomsTable(db),
		eventsTable:   NewEventTable(db),
		snapshotTable: NewSnapshotsTable(db),
		spacesTable:   NewSpacesTable(db),
		entityName:    "server",
	}
	return &Storage{
		accumulator:       acc,
		ToDeviceTable:     NewToDeviceTable(db),
		UnreadTable:       NewUnreadTable(db),
		EventsTable:       acc.eventsTable,
		AccountDataTable:  NewAccountDataTable(db),
		InvitesTable:      NewInvitesTable(db),
		TransactionsTable: NewTransactionsTable(db),
		DeviceDataTable:   NewDeviceDataTable(db),
		ReceiptTable:      NewReceiptTable(db),
		DB:                db,
	}
}

func (s *Storage) LatestEventNID() (int64, error) {
	return s.accumulator.eventsTable.SelectHighestNID()
}

func (s *Storage) AccountData(userID, roomID string, eventTypes []string) (data []AccountData, err error) {
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		data, err = s.AccountDataTable.Select(txn, userID, eventTypes, roomID)
		return err
	})
	return
}

func (s *Storage) RoomAccountDatasWithType(userID, eventType string) (data []AccountData, err error) {
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		data, err = s.AccountDataTable.SelectWithType(txn, userID, eventType)
		return err
	})
	return
}

// Pull out all account data for this user. If roomIDs is empty, global account data is returned.
// If roomIDs is non-empty, all account data for these rooms are extracted.
func (s *Storage) AccountDatas(userID string, roomIDs ...string) (datas []AccountData, err error) {
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		datas, err = s.AccountDataTable.SelectMany(txn, userID, roomIDs...)
		return err
	})
	return
}

func (s *Storage) InsertAccountData(userID, roomID string, events []json.RawMessage) (data []AccountData, err error) {
	data = make([]AccountData, len(events))
	for i := range events {
		data[i] = AccountData{
			UserID: userID,
			RoomID: roomID,
			Data:   events[i],
			Type:   gjson.ParseBytes(events[i]).Get("type").Str,
		}
	}
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		data, err = s.AccountDataTable.Insert(txn, data)
		return err
	})
	return data, err
}

// Prepare a snapshot of the database for calling snapshot functions.
func (s *Storage) PrepareSnapshot(txn *sqlx.Tx) (tableName string, err error) {
	// create a temporary table with all the membership nids for the current snapshots for all rooms.
	// A temporary table will be deleted when the postgres session ends (this process quits).
	// We insert these into a temporary table to let the query planner make better decisions. In practice,
	// if we instead nest this SELECT as a subselect, we see very poor query times for large tables as
	// each event NID is queried using a btree index, rather than doing a seq scan as this query will pull
	// out ~50% of the rows in syncv3_events.
	tempTableName := "temp_snapshot"
	_, err = txn.Exec(
		`SELECT UNNEST(membership_events) AS membership_nid INTO TEMP ` + tempTableName + ` FROM syncv3_snapshots
		JOIN syncv3_rooms ON syncv3_snapshots.snapshot_id = syncv3_rooms.current_snapshot_id`,
	)
	return tempTableName, err
}

// GlobalSnapshot snapshots the entire database for the purposes of initialising
// a sliding sync instance. It will atomically grab metadata for all rooms and all joined members
// in a single transaction.
func (s *Storage) GlobalSnapshot() (ss StartupSnapshot, err error) {
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		tempTableName, err := s.PrepareSnapshot(txn)
		if err != nil {
			err = fmt.Errorf("GlobalSnapshot: failed to call PrepareSnapshot: %w", err)
			sentry.CaptureException(err)
			return err
		}
		var metadata map[string]internal.RoomMetadata
		ss.AllJoinedMembers, metadata, err = s.AllJoinedMembers(txn, tempTableName)
		if err != nil {
			err = fmt.Errorf("GlobalSnapshot: failed to call AllJoinedMembers: %w", err)
			sentry.CaptureException(err)
			return err
		}
		err = s.MetadataForAllRooms(txn, tempTableName, metadata)
		if err != nil {
			err = fmt.Errorf("GlobalSnapshot: failed to call MetadataForAllRooms: %w", err)
			sentry.CaptureException(err)
			return err
		}
		ss.GlobalMetadata = metadata
		return err
	})
	return
}

// Extract hero info for all rooms. Requires a prepared snapshot in order to be called.
func (s *Storage) MetadataForAllRooms(txn *sqlx.Tx, tempTableName string, result map[string]internal.RoomMetadata) error {
	// Select the invited member counts
	rows, err := txn.Query(`
	SELECT room_id, count(state_key) FROM syncv3_events INNER JOIN ` + tempTableName + ` ON membership_nid=event_nid
		WHERE (membership='_invite' OR membership = 'invite') AND event_type='m.room.member' GROUP BY room_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var roomID string
		var inviteCount int
		if err := rows.Scan(&roomID, &inviteCount); err != nil {
			return err
		}
		metadata := result[roomID]
		metadata.InviteCount = inviteCount
		result[roomID] = metadata
	}

	// work out latest timestamps
	events, err := s.accumulator.eventsTable.selectLatestEventByTypeInAllRooms(txn)
	if err != nil {
		return err
	}
	for _, ev := range events {
		metadata, ok := result[ev.RoomID]
		metadata.LastMessageTimestamp = gjson.ParseBytes(ev.JSON).Get("origin_server_ts").Uint()
		if !ok {
			metadata = *internal.NewRoomMetadata(ev.RoomID)
		}
		parsed := gjson.ParseBytes(ev.JSON)
		eventMetadata := internal.EventMetadata{
			NID:       ev.NID,
			Timestamp: parsed.Get("origin_server_ts").Uint(),
		}
		metadata.LatestEventsByType[parsed.Get("type").Str] = eventMetadata
		// it's possible the latest event is a brand new room not caught by the first SELECT for joined
		// rooms e.g when you're invited to a room so we need to make sure to set the metadata again here
		metadata.RoomID = ev.RoomID
		result[ev.RoomID] = metadata
	}

	// Select the name / canonical alias for all rooms
	roomIDToStateEvents, err := s.currentNotMembershipStateEventsInAllRooms(txn, []string{
		"m.room.name", "m.room.canonical_alias",
	})
	if err != nil {
		return fmt.Errorf("failed to load state events for all rooms: %s", err)
	}
	for roomID, stateEvents := range roomIDToStateEvents {
		metadata := result[roomID]
		for _, ev := range stateEvents {
			if ev.Type == "m.room.name" && ev.StateKey == "" {
				metadata.NameEvent = gjson.ParseBytes(ev.JSON).Get("content.name").Str
			} else if ev.Type == "m.room.canonical_alias" && ev.StateKey == "" {
				metadata.CanonicalAlias = gjson.ParseBytes(ev.JSON).Get("content.alias").Str
			}
		}
		result[roomID] = metadata
	}

	// Select the most recent members for each room to serve as Heroes. The spec is ambiguous here:
	// "This should be the first 5 members of the room, ordered by stream ordering, which are joined or invited."
	// Unclear if this is the first 5 *most recent* (backwards) or forwards. For now we'll use the most recent
	// ones, and select 6 of them so we can always use 5 no matter who is requesting the room name.
	rows, err = txn.Query(`
	SELECT rf.* FROM (
		SELECT room_id, event, rank() OVER (
			PARTITION BY room_id ORDER BY event_nid DESC
		) FROM syncv3_events INNER JOIN ` + tempTableName + ` ON membership_nid=event_nid WHERE (
			membership='join' OR membership='invite' OR membership='_join'
		) AND event_type='m.room.member'
	) rf WHERE rank <= 6;`)
	if err != nil {
		return fmt.Errorf("failed to query heroes: %s", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var roomID string
		var event json.RawMessage
		var rank int
		if err := rows.Scan(&roomID, &event, &rank); err != nil {
			return err
		}
		ev := gjson.ParseBytes(event)
		targetUser := ev.Get("state_key").Str
		key := roomID + " " + targetUser
		if seen[key] {
			continue
		}
		seen[key] = true
		metadata := result[roomID]
		metadata.Heroes = append(metadata.Heroes, internal.Hero{
			ID:   targetUser,
			Name: ev.Get("content.displayname").Str,
		})
		result[roomID] = metadata
	}
	roomInfos, err := s.accumulator.roomsTable.SelectRoomInfos(txn)
	if err != nil {
		return fmt.Errorf("failed to select room infos: %s", err)
	}
	var spaceRoomIDs []string
	for _, info := range roomInfos {
		metadata := result[info.ID]
		metadata.Encrypted = info.IsEncrypted
		metadata.UpgradedRoomID = info.UpgradedRoomID
		metadata.PredecessorRoomID = info.PredecessorRoomID
		metadata.RoomType = info.Type
		result[info.ID] = metadata
		if metadata.IsSpace() {
			spaceRoomIDs = append(spaceRoomIDs, info.ID)
		}
	}

	// select space children
	spaceRoomToRelations, err := s.accumulator.spacesTable.SelectChildren(txn, spaceRoomIDs)
	if err != nil {
		return fmt.Errorf("failed to select space children: %s", err)
	}
	for roomID, relations := range spaceRoomToRelations {
		metadata := result[roomID]
		metadata.ChildSpaceRooms = make(map[string]struct{}, len(relations))
		for _, r := range relations {
			// For now we only honour child state events, but we store all the mappings just in case.
			if r.Relation == RelationMSpaceChild {
				metadata.ChildSpaceRooms[r.Child] = struct{}{}
			}
		}
		result[roomID] = metadata
	}
	return nil
}

// Returns all current NOT MEMBERSHIP state events matching the event types given in all rooms. Returns a map of
// room ID to events in that room.
func (s *Storage) currentNotMembershipStateEventsInAllRooms(txn *sqlx.Tx, eventTypes []string) (map[string][]Event, error) {
	query, args, err := sqlx.In(
		`SELECT syncv3_events.room_id, syncv3_events.event_type, syncv3_events.state_key, syncv3_events.event FROM syncv3_events
		WHERE syncv3_events.event_type IN (?)
		AND syncv3_events.event_nid IN (
			SELECT unnest(events) FROM syncv3_snapshots WHERE syncv3_snapshots.snapshot_id IN (SELECT current_snapshot_id FROM syncv3_rooms)
		)`,
		eventTypes,
	)
	if err != nil {
		return nil, err
	}
	rows, err := txn.Query(txn.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]Event)
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.RoomID, &ev.Type, &ev.StateKey, &ev.JSON); err != nil {
			return nil, err
		}
		result[ev.RoomID] = append(result[ev.RoomID], ev)
	}
	return result, nil
}

func (s *Storage) Accumulate(roomID, prevBatch string, timeline []json.RawMessage) (numNew int, timelineNIDs []int64, err error) {
	return s.accumulator.Accumulate(roomID, prevBatch, timeline)
}

func (s *Storage) Initialise(roomID string, state []json.RawMessage) (InitialiseResult, error) {
	return s.accumulator.Initialise(roomID, state)
}

func (s *Storage) EventNIDs(eventNIDs []int64) ([]json.RawMessage, error) {
	events, err := s.EventsTable.SelectByNIDs(nil, true, eventNIDs)
	if err != nil {
		return nil, err
	}
	e := make([]json.RawMessage, len(events))
	for i := range events {
		e[i] = events[i].JSON
	}
	return e, nil
}

func (s *Storage) StateSnapshot(snapID int64) (state []json.RawMessage, err error) {
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		snapshotRow, err := s.accumulator.snapshotTable.Select(txn, snapID)
		if err != nil {
			return err
		}
		events, err := s.accumulator.eventsTable.SelectByNIDs(txn, true, append(snapshotRow.MembershipEvents, snapshotRow.OtherEvents...))
		if err != nil {
			return fmt.Errorf("failed to select state snapshot %v: %s", snapID, err)
		}
		state = make([]json.RawMessage, len(events))
		for i := range events {
			state[i] = events[i].JSON
		}
		return nil
	})
	return
}

// Look up room state after the given event position and no further. eventTypesToStateKeys is a map of event type to a list of state keys for that event type.
// If the list of state keys is empty then all events matching that event type will be returned. If the map is empty entirely, then all room state
// will be returned.
func (s *Storage) RoomStateAfterEventPosition(ctx context.Context, roomIDs []string, pos int64, eventTypesToStateKeys map[string][]string) (roomToEvents map[string][]Event, err error) {
	_, span := internal.StartSpan(ctx, "RoomStateAfterEventPosition")
	defer span.End()
	roomToEvents = make(map[string][]Event, len(roomIDs))
	roomIndex := make(map[string]int, len(roomIDs))
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		// we have 2 ways to pull the latest events:
		//  - superfast rooms table (which races as it can be updated before the new state hits the dispatcher)
		//  - slower events table query
		// we will try to fulfill as many rooms as possible with the rooms table, only using the slower events table
		// query if we can prove we have races. We can prove this because the latest NIDs will be > pos, meaning the
		// database state is ahead of the in-memory state (which is normal as we update the DB first). This should
		// happen infrequently though, so we will warn about this behaviour.
		roomToLatestNIDs, err := s.accumulator.roomsTable.LatestNIDs(txn, roomIDs)
		if err != nil {
			return err
		}
		fastNIDs := make([]int64, 0, len(roomToLatestNIDs))
		var slowRooms []string
		for roomID, latestNID := range roomToLatestNIDs {
			if latestNID > pos {
				slowRooms = append(slowRooms, roomID)
			} else {
				fastNIDs = append(fastNIDs, latestNID)
			}
		}
		latestEvents, err := s.accumulator.eventsTable.SelectByNIDs(txn, true, fastNIDs)
		if err != nil {
			return fmt.Errorf("failed to select latest nids in rooms %v: %s", roomIDs, err)
		}
		if len(slowRooms) > 0 {
			logger.Warn().Int("slow_rooms", len(slowRooms)).Msg("RoomStateAfterEventPosition: pos value provided is far behind the database copy, performance degraded")
			latestSlowEvents, err := s.accumulator.eventsTable.LatestEventInRooms(txn, slowRooms, pos)
			if err != nil {
				return err
			}
			latestEvents = append(latestEvents, latestSlowEvents...)
		}
		for i, ev := range latestEvents {
			roomIndex[ev.RoomID] = i
			if ev.BeforeStateSnapshotID == 0 {
				// if there is no before snapshot then this last event NID is _part of_ the initial state,
				// ergo the state after this == the current state and we can safely ignore the lastEventNID
				ev.BeforeStateSnapshotID = 0
				ev.BeforeStateSnapshotID, err = s.accumulator.roomsTable.CurrentAfterSnapshotID(txn, ev.RoomID)
				if err != nil {
					return err
				}
				latestEvents[i] = ev
			}
		}

		if len(eventTypesToStateKeys) == 0 {
			for _, ev := range latestEvents {
				snapshotRow, err := s.accumulator.snapshotTable.Select(txn, ev.BeforeStateSnapshotID)
				if err != nil {
					return err
				}
				allStateEventNIDs := append(snapshotRow.MembershipEvents, snapshotRow.OtherEvents...)
				// we need to roll forward if this event is state
				if gjson.ParseBytes(ev.JSON).Get("state_key").Exists() {
					if ev.ReplacesNID != 0 {
						// we determined at insert time of this event that this event replaces a nid in the snapshot.
						// find it and replace it
						for j := range allStateEventNIDs {
							if allStateEventNIDs[j] == ev.ReplacesNID {
								allStateEventNIDs[j] = ev.NID
								break
							}
						}
					} else {
						// the event is still state, but it doesn't replace anything, so just add it onto the snapshot,
						// but only if we haven't already
						alreadyExists := false
						for _, nid := range allStateEventNIDs {
							if nid == ev.NID {
								alreadyExists = true
								break
							}
						}
						if !alreadyExists {
							allStateEventNIDs = append(allStateEventNIDs, ev.NID)
						}
					}
				}
				events, err := s.accumulator.eventsTable.SelectByNIDs(txn, true, allStateEventNIDs)
				if err != nil {
					return fmt.Errorf("failed to select state snapshot %v for room %v: %s", ev.BeforeStateSnapshotID, ev.RoomID, err)
				}
				roomToEvents[ev.RoomID] = events
			}
		} else {
			// do an optimised query to pull out only the event types and state keys we care about.
			var args []interface{} // event type, state key, event type, state key, ....
			var wheres []string
			hasMembershipFilter := false
			hasOtherFilter := false
			for evType, skeys := range eventTypesToStateKeys {
				if evType == "m.room.member" {
					hasMembershipFilter = true
				} else {
					hasOtherFilter = true
				}
				for _, skey := range skeys {
					args = append(args, evType, skey)
					wheres = append(wheres, "(syncv3_events.event_type = ? AND syncv3_events.state_key = ?)")
				}
				if len(skeys) == 0 {
					args = append(args, evType)
					wheres = append(wheres, "syncv3_events.event_type = ?")
				}
			}
			snapIDs := make([]int64, len(latestEvents))
			for i := range latestEvents {
				snapIDs[i] = latestEvents[i].BeforeStateSnapshotID
			}
			args = append(args, pq.Int64Array(snapIDs))

			// figure out which state events to look at - if there is no m.room.member filter we can be super fast
			nidcols := "unnest(array_cat(events, membership_events))"
			if hasMembershipFilter && !hasOtherFilter {
				nidcols = "unnest(membership_events)"
			} else if !hasMembershipFilter && hasOtherFilter {
				nidcols = "unnest(events)"
			}
			// it's not possible for there to be no membership filter and no other filter, we wouldn't be executing this code
			// it is possible to have both, so neither if will execute.

			// Similar to CurrentStateEventsInAllRooms
			query, args, err := sqlx.In(
				`SELECT syncv3_events.event_nid, syncv3_events.room_id, syncv3_events.event_type, syncv3_events.state_key, syncv3_events.event FROM syncv3_events
				WHERE (`+strings.Join(wheres, " OR ")+`) AND syncv3_events.event_nid IN (
					SELECT `+nidcols+` FROM syncv3_snapshots WHERE syncv3_snapshots.snapshot_id = ANY(?)
				) ORDER BY syncv3_events.event_nid ASC`,
				args...,
			)
			if err != nil {
				return fmt.Errorf("failed to form sql query: %s", err)
			}
			rows, err := s.accumulator.db.Query(s.accumulator.db.Rebind(query), args...)
			if err != nil {
				return fmt.Errorf("failed to execute query: %s", err)
			}
			defer rows.Close()
			for rows.Next() {
				var ev Event
				if err := rows.Scan(&ev.NID, &ev.RoomID, &ev.Type, &ev.StateKey, &ev.JSON); err != nil {
					return err
				}
				i := roomIndex[ev.RoomID]
				if latestEvents[i].ReplacesNID == ev.NID {
					// this event is replaced by the last event
					ev = latestEvents[i]
				}
				roomToEvents[ev.RoomID] = append(roomToEvents[ev.RoomID], ev)
			}
			// handle the most recent events which won't be in the snapshot but may need to be.
			// we handle the replace case but don't handle brand new state events
			for i := range latestEvents {
				if latestEvents[i].ReplacesNID == 0 {
					// check if we should include it
					for evType, stateKeys := range eventTypesToStateKeys {
						if evType != latestEvents[i].Type {
							continue
						}
						if len(stateKeys) == 0 {
							roomToEvents[latestEvents[i].RoomID] = append(roomToEvents[latestEvents[i].RoomID], latestEvents[i])
						} else {
							for _, skey := range stateKeys {
								if skey == latestEvents[i].StateKey {
									roomToEvents[latestEvents[i].RoomID] = append(roomToEvents[latestEvents[i].RoomID], latestEvents[i])
									break
								}
							}
						}
					}
				}
			}
		}
		return nil
	})
	return
}

func (s *Storage) LatestEventsInRooms(userID string, roomIDs []string, to int64, limit int) (map[string][]json.RawMessage, map[string]string, error) {
	roomIDToRanges, err := s.visibleEventNIDsBetweenForRooms(userID, roomIDs, 0, to)
	if err != nil {
		return nil, nil, err
	}
	result := make(map[string][]json.RawMessage, len(roomIDs))
	prevBatches := make(map[string]string, len(roomIDs))
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		for roomID, ranges := range roomIDToRanges {
			var earliestEventNID int64
			var roomEvents []json.RawMessage
			// start at the most recent range as we want to return the most recent `limit` events
			for i := len(ranges) - 1; i >= 0; i-- {
				if len(roomEvents) >= limit {
					break
				}
				r := ranges[i]
				// the most recent event will be first
				events, err := s.EventsTable.SelectLatestEventsBetween(txn, roomID, r[0]-1, r[1], limit)
				if err != nil {
					return fmt.Errorf("room %s failed to SelectEventsBetween: %s", roomID, err)
				}
				// keep pushing to the front so we end up with A,B,C
				for _, ev := range events {
					roomEvents = append([]json.RawMessage{ev.JSON}, roomEvents...)
					earliestEventNID = ev.NID
					if len(roomEvents) >= limit {
						break
					}
				}
			}
			if earliestEventNID != 0 {
				// the oldest event needs a prev batch token, so find one now
				prevBatch, err := s.EventsTable.SelectClosestPrevBatch(roomID, earliestEventNID)
				if err != nil {
					return fmt.Errorf("failed to select prev_batch for room %s : %s", roomID, err)
				}
				prevBatches[roomID] = prevBatch
			}
			result[roomID] = roomEvents
		}
		return nil
	})
	return result, prevBatches, err
}

func (s *Storage) visibleEventNIDsBetweenForRooms(userID string, roomIDs []string, from, to int64) (map[string][][2]int64, error) {
	// load *THESE* joined rooms for this user at from (inclusive)
	var membershipEvents []Event
	var err error
	if from != 0 {
		// if from==0 then this query will return nothing, so optimise it out
		membershipEvents, err = s.accumulator.eventsTable.SelectEventsWithTypeStateKeyInRooms(roomIDs, "m.room.member", userID, 0, from)
		if err != nil {
			return nil, fmt.Errorf("VisibleEventNIDsBetweenForRooms.SelectEventsWithTypeStateKeyInRooms: %s", err)
		}
	}
	joinNIDsByRoomID, err := s.determineJoinedRoomsFromMemberships(membershipEvents)
	if err != nil {
		return nil, fmt.Errorf("failed to work out joined rooms for %s at pos %d: %s", userID, from, err)
	}

	// load membership deltas for *THESE* rooms for this user
	membershipEvents, err = s.accumulator.eventsTable.SelectEventsWithTypeStateKeyInRooms(roomIDs, "m.room.member", userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("failed to load membership events: %s", err)
	}

	return s.visibleEventNIDsWithData(joinNIDsByRoomID, membershipEvents, userID, from, to)
}

// Work out the NID ranges to pull events from for this user. Given a from and to event nid stream position,
// this function returns a map of room ID to a slice of 2-element from|to positions. These positions are
// all INCLUSIVE, and the client should be informed of these events at some point. For example:
//
//	                  Stream Positions
//	        1     2   3    4   5   6   7   8   9   10
//	Room A  Maj   E   E                E
//	Room B                 E   Maj E
//	Room C                                 E   Mal E   (a already joined to this room at position 0)
//
//	E=message event, M=membership event, followed by user letter, followed by 'i' or 'j' or 'l' for invite|join|leave
//
//	- For Room A: from=1, to=10, returns { RoomA: [ [1,10] ]}  (tests events in joined room)
//	- For Room B: from=1, to=10, returns { RoomB: [ [5,10] ]}  (tests joining a room starts events)
//	- For Room C: from=1, to=10, returns { RoomC: [ [0,9] ]}  (tests leaving a room stops events)
//
// Multiple slices can occur when a user leaves and re-joins the same room, and invites are same-element positions:
//
//	                   Stream Positions
//	         1     2   3    4   5   6   7   8   9   10  11  12  13  14  15
//	 Room D  Maj                E   Mal E   Maj E   Mal E
//	 Room E        E   Mai  E                               E   Maj E   E
//
//	- For Room D: from=1, to=15 returns { RoomD: [ [1,6], [8,10] ] } (tests multi-join/leave)
//	- For Room E: from=1, to=15 returns { RoomE: [ [3,3], [13,15] ] } (tests invites)
func (s *Storage) VisibleEventNIDsBetween(userID string, from, to int64) (map[string][][2]int64, error) {
	// load *ALL* joined rooms for this user at from (inclusive)
	joinNIDsByRoomID, err := s.JoinedRoomsAfterPosition(userID, from)
	if err != nil {
		return nil, fmt.Errorf("failed to work out joined rooms for %s at pos %d: %s", userID, from, err)
	}

	// load *ALL* membership deltas for all rooms for this user
	membershipEvents, err := s.accumulator.eventsTable.SelectEventsWithTypeStateKey("m.room.member", userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("failed to load membership events: %s", err)
	}

	return s.visibleEventNIDsWithData(joinNIDsByRoomID, membershipEvents, userID, from, to)
}

func (s *Storage) visibleEventNIDsWithData(joinNIDsByRoomID map[string]int64, membershipEvents []Event, userID string, from, to int64) (map[string][][2]int64, error) {
	// load membership events in order and bucket based on room ID
	roomIDToLogs := make(map[string][]membershipEvent)
	for _, ev := range membershipEvents {
		evJSON := gjson.ParseBytes(ev.JSON)
		roomIDToLogs[ev.RoomID] = append(roomIDToLogs[ev.RoomID], membershipEvent{
			Event:      ev,
			StateKey:   evJSON.Get("state_key").Str,
			Membership: evJSON.Get("content.membership").Str,
		})
	}

	// Performs the algorithm
	calculateVisibleEventNIDs := func(isJoined bool, fromIncl, toIncl int64, logs []membershipEvent) [][2]int64 {
		// short circuit when there are no membership deltas
		if len(logs) == 0 {
			return [][2]int64{
				{
					fromIncl, toIncl,
				},
			}
		}
		var result [][2]int64
		var startIndex int64 = -1
		if isJoined {
			startIndex = fromIncl
		}
		for _, memEvent := range logs {
			// check for a valid transition (join->leave|ban or leave|invite->join) - we won't always get valid transitions
			// e.g logs will be there for things like leave->ban which we don't care about
			isValidTransition := false
			if isJoined && (memEvent.Membership == "leave" || memEvent.Membership == "ban") {
				isValidTransition = true
			} else if !isJoined && memEvent.Membership == "join" {
				isValidTransition = true
			} else if !isJoined && memEvent.Membership == "invite" {
				// short-circuit: invites are sent on their own and don't affect ranges
				result = append(result, [2]int64{memEvent.NID, memEvent.NID})
				continue
			}
			if !isValidTransition {
				continue
			}
			if isJoined {
				// transitioning to leave, we get all events up to and including the leave event
				result = append(result, [2]int64{startIndex, memEvent.NID})
				isJoined = false
			} else {
				// transitioning to joined, we will get the join and some more events in a bit
				startIndex = memEvent.NID
				isJoined = true
			}
		}
		// if we are still joined to the room at this point, grab all events up to toIncl
		if isJoined {
			result = append(result, [2]int64{startIndex, toIncl})
		}
		return result
	}

	// For each joined room, perform the algorithm and delete the logs afterwards
	result := make(map[string][][2]int64)
	for joinedRoomID, _ := range joinNIDsByRoomID {
		roomResult := calculateVisibleEventNIDs(true, from, to, roomIDToLogs[joinedRoomID])
		result[joinedRoomID] = roomResult
		delete(roomIDToLogs, joinedRoomID)
	}

	// Handle rooms which we are not joined to but have logs for
	for roomID, logs := range roomIDToLogs {
		roomResult := calculateVisibleEventNIDs(false, from, to, logs)
		result[roomID] = roomResult
	}

	return result, nil
}

func (s *Storage) RoomMembershipDelta(roomID string, from, to int64, limit int) (eventJSON []json.RawMessage, upTo int64, err error) {
	err = sqlutil.WithTransaction(s.accumulator.db, func(txn *sqlx.Tx) error {
		nids, err := s.accumulator.eventsTable.SelectEventNIDsWithTypeInRoom(txn, "m.room.member", limit, roomID, from, to)
		if err != nil {
			return err
		}
		if len(nids) == 0 {
			return nil
		}
		upTo = nids[len(nids)-1]
		events, err := s.accumulator.eventsTable.SelectByNIDs(txn, true, nids)
		if err != nil {
			return err
		}
		eventJSON = make([]json.RawMessage, len(events))
		for i := range events {
			eventJSON[i] = events[i].JSON
		}
		return nil
	})
	return
}

// Extract all rooms with joined members, and include the joined user list. Requires a prepared snapshot in order to be called.
func (s *Storage) AllJoinedMembers(txn *sqlx.Tx, tempTableName string) (result map[string][]string, metadata map[string]internal.RoomMetadata, err error) {
	rows, err := txn.Query(
		`SELECT room_id, state_key from ` + tempTableName + ` INNER JOIN syncv3_events on membership_nid = event_nid WHERE membership='join' OR membership='_join' ORDER BY event_nid ASC`,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	result = make(map[string][]string)
	var roomID string
	var joinedUserID string
	for rows.Next() {
		if err := rows.Scan(&roomID, &joinedUserID); err != nil {
			return nil, nil, err
		}
		users := result[roomID]
		users = append(users, joinedUserID)
		result[roomID] = users
	}
	metadata = make(map[string]internal.RoomMetadata)
	for roomID, joinedMembers := range result {
		m := internal.NewRoomMetadata(roomID)
		m.JoinCount = len(joinedMembers)
		metadata[roomID] = *m
	}
	return result, metadata, nil
}

func (s *Storage) JoinedRoomsAfterPosition(userID string, pos int64) (
	joinedRoomsWithJoinNIDs map[string]int64, err error,
) {
	// fetch all the membership events up to and including pos
	membershipEvents, err := s.accumulator.eventsTable.SelectEventsWithTypeStateKey("m.room.member", userID, 0, pos)
	if err != nil {
		return nil, fmt.Errorf("JoinedRoomsAfterPosition.SelectEventsWithTypeStateKey: %s", err)
	}
	return s.determineJoinedRoomsFromMemberships(membershipEvents)
}

// determineJoinedRoomsFromMemberships scans a slice of membership events from multiple
// rooms, to determine which rooms a user is currently joined to. Those events MUST be
// - sorted by ascending NIDs, and
// - only memberships for the given user;
// neither of these preconditions are checked by this function.
//
// Returns a slice of joined room IDs and a slice of joined event NIDs, whose entries
// correspond to one another. Rooms appear in these slices in no particular order.
func (s *Storage) determineJoinedRoomsFromMemberships(membershipEvents []Event) (
	joinNIDsByRoomID map[string]int64, err error,
) {
	joinNIDsByRoomID = make(map[string]int64, len(membershipEvents))
	for _, ev := range membershipEvents {
		membership := gjson.GetBytes(ev.JSON, "content.membership").Str
		switch membership {
		// These are "join" and the only memberships that you can transition to after
		// a join: see e.g. the transition diagram in
		// https://spec.matrix.org/v1.7/client-server-api/#room-membership
		case "join":
			// Only remember a join NID if we are not joined to this room according to
			// the state before ev.
			if _, currentlyJoined := joinNIDsByRoomID[ev.RoomID]; !currentlyJoined {
				joinNIDsByRoomID[ev.RoomID] = ev.NID
			}
		case "ban":
			fallthrough
		case "leave":
			delete(joinNIDsByRoomID, ev.RoomID)
		}
	}

	return joinNIDsByRoomID, nil
}

func (s *Storage) Teardown() {
	err := s.accumulator.db.Close()
	if err != nil {
		panic("Storage.Teardown: " + err.Error())
	}
}
