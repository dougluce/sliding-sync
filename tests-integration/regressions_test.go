package syncv3

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/matrix-org/sliding-sync/sync2"
	"github.com/matrix-org/sliding-sync/sync3"
	"github.com/matrix-org/sliding-sync/testutils"
	"github.com/matrix-org/sliding-sync/testutils/m"
)

// catch all file for any kind of regression test which doesn't fall into a unique category

// Regression test for https://github.com/matrix-org/sliding-sync/issues/192
//   - Bob on his server invites Alice to a room.
//   - Alice joins the room first over federation. Proxy does the right thing and sets her membership to join. There is no timeline though due to not having backfilled.
//   - Alice's client backfills in the room which pulls in the invite event, but the SS proxy doesn't see it as it's backfill, not /sync.
//   - Charlie joins the same room via SS, which makes the SS proxy see 50 timeline events, which includes the invite.
//     As the proxy has never seen this invite event before, it assumes it is newer than the join event and inserts it, corrupting state.
//
// Manually confirmed this can happen with 3x Element clients. We need to make sure we drop those earlier events.
// The first join over federation presents itself as a single join event in the timeline, with the create event, etc in state.
func TestBackfillInviteDoesntCorruptState(t *testing.T) {
	pqString := testutils.PrepareDBConnectionString()
	// setup code
	v2 := runTestV2Server(t)
	v3 := runTestServer(t, v2, pqString)
	defer v2.close()
	defer v3.close()

	fedBob := "@bob:over_federation"
	charlie := "@charlie:localhost"
	charlieToken := "CHARLIE_TOKEN"
	joinEvent := testutils.NewJoinEvent(t, alice)

	room := roomEvents{
		roomID: "!TestBackfillInviteDoesntCorruptState:localhost",
		events: []json.RawMessage{
			joinEvent,
		},
		state: createRoomState(t, fedBob, time.Now()),
	}
	v2.addAccount(t, alice, aliceToken)
	v2.queueResponse(alice, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(room),
		},
	})

	// alice syncs and should see the room.
	aliceRes := v3.mustDoV3Request(t, aliceToken, sync3.Request{
		Lists: map[string]sync3.RequestList{
			"a": {
				Ranges: sync3.SliceRanges{{0, 20}},
				RoomSubscription: sync3.RoomSubscription{
					TimelineLimit: 5,
				},
			},
		},
	})
	m.MatchResponse(t, aliceRes, m.MatchList("a", m.MatchV3Count(1), m.MatchV3Ops(m.MatchV3SyncOp(0, 0, []string{room.roomID}))))

	// Alice's client "backfills" new data in, meaning the next user who joins is going to see a different set of timeline events
	dummyMsg := testutils.NewMessageEvent(t, fedBob, "you didn't see this before joining")
	charlieJoinEvent := testutils.NewJoinEvent(t, charlie)
	backfilledTimelineEvents := append(
		room.state, []json.RawMessage{
			dummyMsg,
			testutils.NewStateEvent(t, "m.room.member", alice, fedBob, map[string]interface{}{
				"membership": "invite",
			}),
			joinEvent,
			charlieJoinEvent,
		}...,
	)

	// now charlie also joins the room, causing a different response from /sync v2
	v2.addAccount(t, charlie, charlieToken)
	v2.queueResponse(charlie, sync2.SyncResponse{
		Rooms: sync2.SyncRoomsResponse{
			Join: v2JoinTimeline(roomEvents{
				roomID: room.roomID,
				events: backfilledTimelineEvents,
			}),
		},
	})

	// and now charlie hits SS, which might corrupt membership state for alice.
	charlieRes := v3.mustDoV3Request(t, charlieToken, sync3.Request{
		Lists: map[string]sync3.RequestList{
			"a": {
				Ranges: sync3.SliceRanges{{0, 20}},
			},
		},
	})
	m.MatchResponse(t, charlieRes, m.MatchList("a", m.MatchV3Count(1), m.MatchV3Ops(m.MatchV3SyncOp(0, 0, []string{room.roomID}))))

	// alice should not see dummyMsg or the invite
	aliceRes = v3.mustDoV3RequestWithPos(t, aliceToken, aliceRes.Pos, sync3.Request{})
	m.MatchResponse(t, aliceRes, m.MatchNoV3Ops(), m.LogResponse(t), m.MatchRoomSubscriptionsStrict(
		map[string][]m.RoomMatcher{
			room.roomID: {
				m.MatchJoinCount(3), // alice, bob, charlie,
				m.MatchNoInviteCount(),
				m.MatchNumLive(1),
				m.MatchRoomTimeline([]json.RawMessage{charlieJoinEvent}),
			},
		},
	))
}