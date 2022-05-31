package sync3

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/matrix-org/sync-v3/internal"
	"github.com/matrix-org/sync-v3/sync3/extensions"
)

var (
	SortByName              = "by_name"
	SortByRecency           = "by_recency"
	SortByNotificationCount = "by_notification_count"
	SortByHighlightCount    = "by_highlight_count"
	SortBy                  = []string{SortByHighlightCount, SortByName, SortByNotificationCount, SortByRecency}

	DefaultTimelineLimit = int64(20)
	DefaultTimeoutMSecs  = 10 * 1000 // 10s
)

type Request struct {
	Lists             []RequestList               `json:"lists"`
	RoomSubscriptions map[string]RoomSubscription `json:"room_subscriptions"`
	UnsubscribeRooms  []string                    `json:"unsubscribe_rooms"`
	Extensions        extensions.Request          `json:"extensions"`

	// set via query params or inferred
	pos          int64
	timeoutMSecs int
}

type RequestList struct {
	RoomSubscription
	Ranges  SliceRanges     `json:"ranges"`
	Sort    []string        `json:"sort"`
	Filters *RequestFilters `json:"filters"`
}

func (rl *RequestList) SortOrderChanged(next *RequestList) bool {
	prevLen := 0
	if rl != nil {
		prevLen = len(rl.Sort)
	}
	if prevLen != len(next.Sort) {
		return true
	}
	for i := range rl.Sort {
		if rl.Sort[i] != next.Sort[i] {
			return true
		}
	}
	return false
}

func (rl *RequestList) FiltersChanged(next *RequestList) bool {
	var prev *RequestFilters
	if rl != nil {
		prev = rl.Filters
	}
	// easier to marshal as JSON rather than do a bazillion nil checks
	pb, err := json.Marshal(prev)
	if err != nil {
		panic(err)
	}
	nb, err := json.Marshal(next.Filters)
	if err != nil {
		panic(err)
	}
	return !bytes.Equal(pb, nb)
}

// Write a delete operation for this list. Can return nil for invalid indexes or if this index isn't being tracked.
func (rl *RequestList) WriteDeleteOp(deletedIndex int) *ResponseOpSingle {
	// update operations return -1 if nothing gets deleted
	if deletedIndex < 0 {
		return nil
	}
	// only notify if we are tracking this index
	if !rl.Ranges.Inside(int64(deletedIndex)) {
		return nil
	}
	return &ResponseOpSingle{
		Operation: OpDelete,
		Index:     &deletedIndex,
	}
}

// Calculate the real from -> to index positions for the two input index positions. This takes into
// account the ranges on the list. Return ok=false if move indexes are not valid e.g the index being
// moved from/to is outside all ranges and doesn't go over slices. TODO handle going over slices
func (rl *RequestList) CalculateMoveIndexes(fromIndex, toIndex int) (f, t int, ok bool) {
	// Given a range like the following there are several cases to consider:
	// 0  1  2  3  4  5  6  7  8  9  10
	//    |--------|        |-----|        [1,4],[7,9]                                           RESULT
	//       T  F                          move inside the same range                             3, 2
	//          T     F                    move from outside to inside the range                  4, 3
	//                F        T           move from outside to inside the range (higher)         7, 8
	//          F        T                 move from inside to outside the range (higher)         3, 4
	//                T        F           move from inside to outside the range                  8, 7
	//          T              F           move between two ranges                                8, 3
	//                F  T                 move outside the ranges, no jumps                      !ok
	// T              F                    move outside the ranges, jumping over a range          4, 1 (everything shift rights)       TODO
	// T                              F    move outside the ranges, jumping over multiple ranges  4,1 + 7, 9 (everything shifts right) TODO

	// This can be summarised with the following rules:
	//  A- If BOTH from/to are inside ranges: return those indexes.
	//  B- If ONE index is inside a range:
	//     * Use the index inside the range
	//     * Find the direction of movement (towards / away from zero)
	//     * Find the closest range boundary in that direction for the index outside the range and use that.
	//  C- If BOTH from/to are outside ranges:
	//     * Find which ranges are jumped over. If none, return !ok
	//     * For each jumped over range:
	//        * Set from/to index to the range boundaries according to the direction of movement
	//     * Return potentially > 1 set of move indexes

	isFromInsideRange := rl.Ranges.Inside(int64(fromIndex))
	isToInsideRange := rl.Ranges.Inside(int64(toIndex))
	if isFromInsideRange && isToInsideRange { // case A
		return fromIndex, toIndex, true
	}
	if !isFromInsideRange && !isToInsideRange { // case C
		// TODO jumping over multiple range
		// work out which ranges are jumped over
		var jumpedOverRanges [][2]int64
		hi := int64(fromIndex)
		lo := int64(toIndex)
		if fromIndex < toIndex {
			hi = int64(toIndex)
			lo = int64(fromIndex)
		}
		for _, r := range rl.Ranges {
			if r[0] > lo && r[0] < hi && r[1] > lo && r[1] < hi {
				jumpedOverRanges = append(jumpedOverRanges, r)
			}
		}
		if len(jumpedOverRanges) == 0 {
			return 0, 0, false
		}
		// TODO: handle multiple ranges
		jumpedOverRange := jumpedOverRanges[0]
		if fromIndex > toIndex { // heading towards zero
			return int(jumpedOverRange[1]), int(jumpedOverRange[0]), true
		} else {
			return int(jumpedOverRange[0]), int(jumpedOverRange[1]), true
		}
	}

	// case B
	if isFromInsideRange {
		f = fromIndex
		// snap toIndex to a lower value i.e towards zero IF to > from
		t = int(rl.Ranges.ClosestInDirection(int64(toIndex), toIndex > fromIndex))
	}
	if isToInsideRange {
		t = toIndex
		// snap fromIndex to a lower value i.e towards zero IF to < from
		f = int(rl.Ranges.ClosestInDirection(int64(fromIndex), toIndex < fromIndex))
	}

	return f, t, true
}

// Move a room from an absolute index position to another absolute position. These positions do not
// need to be inside a valid range. Returns 0-2 operations. For example:
//   1,2,3,4,5 tracking range [0,4]
//   3 bumps to top -> 3,1,2,4,5 -> DELETE index=2, INSERT val=3 index=0
//   7 bumps to top -> 7,1,2,3,4 -> DELETE index=4, INSERT val=7 index=0
//   7 bumps to op again -> 7,1,2,3,4 -> no-op as from == to index
//   new room 8 in i=5 -> 7,1,2,3,4,8 -> no-op as 8 is outside the range.
// Returns the list of ops as well as the new toIndex if it wasn't inside a range.
func (rl *RequestList) WriteSwapOp(
	roomID string, fromIndex, toIndex int,
) []ResponseOp {
	if fromIndex == toIndex {
		return nil // we only care to notify clients about moves in the list
	}

	return []ResponseOp{
		&ResponseOpSingle{
			Operation: OpDelete,
			Index:     &fromIndex,
		},
		&ResponseOpSingle{
			Operation: OpInsert,
			Index:     &toIndex,
			RoomID:    roomID,
		},
	}
}

func (r *Request) SetPos(pos int64) {
	r.pos = pos
}
func (r *Request) TimeoutMSecs() int {
	return r.timeoutMSecs
}
func (r *Request) SetTimeoutMSecs(timeout int) {
	r.timeoutMSecs = timeout
}

func (r *Request) Same(other *Request) bool {
	serialised, err := json.Marshal(r)
	if err != nil {
		return false
	}
	otherSer, err := json.Marshal(other)
	if err != nil {
		return false
	}
	return bytes.Equal(serialised, otherSer)
}

// Internal struct used to represent the diffs between 2 requests
type RequestDelta struct {
	// new room IDs to subscribe to
	Subs []string
	// room IDs to unsubscribe from
	Unsubs []string
	// The complete union of both lists (contains max(a,b) lists)
	Lists []RequestListDelta
}

// Internal struct used to represent a single list delta.
type RequestListDelta struct {
	// What was there before, nullable
	Prev *RequestList
	// What is there now, nullable. Combined result.
	Curr *RequestList
}

// Apply this delta on top of the request. Returns a new Request with the combined output, along
// with the delta operations `nextReq` cannot be nil, but `r` can be nil in the case of an initial
// request.
func (r *Request) ApplyDelta(nextReq *Request) (result *Request, delta *RequestDelta) {
	if r == nil {
		result = &Request{
			Extensions: nextReq.Extensions,
		}
		r = &Request{}
	} else {
		// Use the newer values unless they aren't specified, then use the older ones.
		// Go is ew in that this can't be represented in a nicer way
		result = &Request{
			Extensions: r.Extensions.ApplyDelta(&nextReq.Extensions),
		}
	}

	delta = &RequestDelta{}
	lists := make([]RequestList, len(nextReq.Lists))
	for i := 0; i < len(lists); i++ {
		var existingList *RequestList
		if i < len(r.Lists) {
			existingList = &r.Lists[i]
		}
		// default to recency sort order if missing and there isn't a previous list to draw from
		if len(nextReq.Lists[i].Sort) == 0 && existingList == nil {
			nextReq.Lists[i].Sort = []string{SortByRecency}
		}
		if existingList == nil {
			// we added a list
			lists[i] = nextReq.Lists[i]
			continue
		}
		nextList := nextReq.Lists[i]
		rooms := nextList.Ranges
		if rooms == nil {
			rooms = existingList.Ranges
		}
		sort := nextList.Sort
		if sort == nil {
			sort = existingList.Sort
		}
		reqState := nextList.RequiredState
		if reqState == nil {
			reqState = existingList.RequiredState
		}
		timelineLimit := nextList.TimelineLimit
		if timelineLimit == 0 {
			timelineLimit = existingList.TimelineLimit
		}
		filters := nextList.Filters
		if filters == nil {
			filters = existingList.Filters
		}
		lists[i] = RequestList{
			RoomSubscription: RoomSubscription{
				RequiredState: reqState,
				TimelineLimit: timelineLimit,
			},
			Ranges:  rooms,
			Sort:    sort,
			Filters: filters,
		}
	}
	result.Lists = lists
	// the delta is as large as the longest list of lists
	maxLen := len(result.Lists)
	if len(r.Lists) > maxLen {
		maxLen = len(r.Lists)
	}
	delta.Lists = make([]RequestListDelta, maxLen)
	for i := range result.Lists {
		delta.Lists[i] = RequestListDelta{
			Curr: &result.Lists[i],
		}
	}
	for i := range r.Lists {
		delta.Lists[i].Prev = &r.Lists[i]
	}

	// Work out subscriptions. The operations are applied as:
	// old.subs -> apply old.unsubs (should be empty) -> apply new.subs -> apply new.unsubs
	// Meaning if a room is both in subs and unsubs then the result is unsub.
	// This also allows clients to update their filters for an existing room subscription.
	resultSubs := make(map[string]RoomSubscription)
	for roomID, val := range r.RoomSubscriptions {
		resultSubs[roomID] = val
	}
	for _, roomID := range r.UnsubscribeRooms {
		_, ok := resultSubs[roomID]
		if ok {
			delta.Unsubs = append(delta.Unsubs, roomID)
		}
		delete(resultSubs, roomID)
	}
	for roomID, val := range nextReq.RoomSubscriptions {
		// either updating an existing sub or is a new sub, we don't care which for now.
		resultSubs[roomID] = val
	}
	for _, roomID := range nextReq.UnsubscribeRooms {
		_, ok := resultSubs[roomID]
		if ok {
			// if this request both subscribes and unsubscribes to the same room ID,
			// don't mark this as an unsub delta
			if _, ok = nextReq.RoomSubscriptions[roomID]; !ok {
				delta.Unsubs = append(delta.Unsubs, roomID)
			}
		}
		delete(resultSubs, roomID)
	}
	// new subscriptions are the delta between old room subs and the newly calculated ones
	for roomID := range resultSubs {
		if _, ok := r.RoomSubscriptions[roomID]; ok {
			continue // already subscribed
		}
		delta.Subs = append(delta.Subs, roomID)
	}
	result.RoomSubscriptions = resultSubs
	return
}

func (r *Request) GetTimelineLimit(listIndex int, roomID string) int64 {
	if r.RoomSubscriptions != nil {
		room, ok := r.RoomSubscriptions[roomID]
		if ok && room.TimelineLimit > 0 {
			return room.TimelineLimit
		}
	}
	if r.Lists[listIndex].TimelineLimit > 0 {
		return r.Lists[listIndex].TimelineLimit
	}
	return DefaultTimelineLimit
}

type RequestFilters struct {
	Spaces         []string `json:"spaces"`
	IsDM           *bool    `json:"is_dm"`
	IsEncrypted    *bool    `json:"is_encrypted"`
	IsInvite       *bool    `json:"is_invite"`
	IsTombstoned   *bool    `json:"is_tombstoned"`
	RoomNameFilter string   `json:"room_name_like"`
	// TODO options to control which events should be live-streamed e.g not_types, types from sync v2
}

func (rf *RequestFilters) Include(r *RoomConnMetadata) bool {
	if rf.IsEncrypted != nil && *rf.IsEncrypted != r.Encrypted {
		return false
	}
	if rf.IsTombstoned != nil && *rf.IsTombstoned != r.Tombstoned {
		return false
	}
	if rf.IsDM != nil && *rf.IsDM != r.IsDM {
		return false
	}
	if rf.IsInvite != nil && *rf.IsInvite != r.IsInvite {
		return false
	}
	if rf.RoomNameFilter != "" && !strings.Contains(strings.ToLower(internal.CalculateRoomName(&r.RoomMetadata, 5)), strings.ToLower(rf.RoomNameFilter)) {
		return false
	}
	return true
}

type RoomSubscription struct {
	RequiredState [][2]string `json:"required_state"`
	TimelineLimit int64       `json:"timeline_limit"`
}

// Combine this subcription with another, returning a union of both as a copy.
func (rs RoomSubscription) Combine(other RoomSubscription) RoomSubscription {
	var result RoomSubscription
	// choose max value
	if rs.TimelineLimit > other.TimelineLimit {
		result.TimelineLimit = rs.TimelineLimit
	} else {
		result.TimelineLimit = other.TimelineLimit
	}
	// combine together required_state fields, we'll union them later
	result.RequiredState = append(rs.RequiredState, other.RequiredState...)
	return result
}

// Calculate the required state map for this room subscription. Given event types A,B,C and state keys
// 1,2,3, the following Venn diagrams are possible:
//  .---------[*,*]----------.
//  |      .---------.       |
//  |      |   A,2   | A,3   |
//  | .----+--[B,*]--+-----. |
//  | |    | .-----. |     | |
//  | |B,1 | | B,2 | | B,3 | |
//  | |    | `[B,2]` |     | |
//  | `----+---------+-----` |
//  |      |   C,2   | C,3   |
//  |      `--[*,2]--`       |
//  `------------------------`
//
// The largest set will be used when returning the required state map.
// For example, [B,2] + [B,*] = [B,*] because [B,*] encompasses [B,2]. This means [*,*] encompasses
// everything.
func (rs RoomSubscription) RequiredStateMap() *internal.RequiredStateMap {
	result := make(map[string][]string)
	eventTypesWithWildcardStateKeys := make(map[string]struct{})
	var stateKeysForWildcardEventType []string
	for _, tuple := range rs.RequiredState {
		if tuple[0] == "*" {
			if tuple[1] == "*" { // all state
				return internal.NewRequiredStateMap(nil, nil, nil, true)
			}
			stateKeysForWildcardEventType = append(stateKeysForWildcardEventType, tuple[1])
			continue
		}
		if tuple[1] == "*" { // wildcard state key
			eventTypesWithWildcardStateKeys[tuple[0]] = struct{}{}
		} else {
			result[tuple[0]] = append(result[tuple[0]], tuple[1])
		}
	}
	return internal.NewRequiredStateMap(eventTypesWithWildcardStateKeys, stateKeysForWildcardEventType, result, false)
}
