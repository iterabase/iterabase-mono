package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	workspaceParticipantA = "session-a"
	workspaceParticipantB = "session-b"
)

var workspaceChildProofPattern = regexp.MustCompile(`workspace-child-proof participant=([a-z0-9-]+) session=([A-Za-z0-9._-]+) uid=([0-9]+) gid=([0-9]+) marker_sha256=([a-f0-9]{64}) invariants=groups-cleared,caps-cleared,no-new-privs,umask-0077,pool-root-0:0:0711,session-tree-0700,sibling-EACCES,tls-key-EACCES`)

type workspaceExpectedParticipant struct {
	Participant  string `json:"participant"`
	WorkItemID   string `json:"work_item_id"`
	AttemptID    string `json:"attempt_id"`
	SessionID    string `json:"session_id"`
	MarkerSHA256 string `json:"-"`
}

type workspaceBarrierArrival struct {
	Participant  string `json:"participant"`
	WorkItemID   string `json:"work_item_id"`
	AttemptID    string `json:"attempt_id"`
	SessionID    string `json:"session_id"`
	UID          uint32 `json:"uid"`
	GID          uint32 `json:"gid"`
	MarkerSHA256 string `json:"marker_sha256"`
}

type workspaceBarrierStatus struct {
	State    string                         `json:"state"`
	Ready    bool                           `json:"ready"`
	Failure  string                         `json:"failure"`
	Expected []workspaceExpectedParticipant `json:"expected"`
	Arrivals []workspaceBarrierArrival      `json:"arrivals"`
}

type workspaceConcurrencyRow struct {
	WorkItemID      string `json:"work_item_id"`
	AttemptID       string `json:"attempt_id"`
	SessionID       string `json:"session_id"`
	AssignmentState string `json:"assignment_state"`
	TurnID          string `json:"turn_id"`
	WorkerID        string `json:"worker_id"`
	AllocationState string `json:"allocation_state"`
	AllocationUID   uint32 `json:"allocation_uid"`
	AssignedSession string `json:"assigned_session"`
	AssignedUID     uint32 `json:"assigned_uid"`
	AssignedGID     uint32 `json:"assigned_gid"`
	ChildResult     string `json:"child_result"`
}

type workspaceChildProof struct {
	Participant  string
	SessionID    string
	UID          uint32
	GID          uint32
	MarkerSHA256 string
}

func parseWorkspaceChildProof(raw string) (workspaceChildProof, error) {
	match := workspaceChildProofPattern.FindStringSubmatch(raw)
	if len(match) != 6 {
		return workspaceChildProof{}, fmt.Errorf("child result lacks the exact isolation proof")
	}
	uid, uidErr := strconv.ParseUint(match[3], 10, 32)
	gid, gidErr := strconv.ParseUint(match[4], 10, 32)
	if uidErr != nil || gidErr != nil {
		return workspaceChildProof{}, fmt.Errorf("child result has an invalid UID/GID")
	}
	return workspaceChildProof{
		Participant: match[1], SessionID: match[2], UID: uint32(uid), GID: uint32(gid), MarkerSHA256: match[5],
	}, nil
}

func validateWorkspaceConcurrencyProof(expected []workspaceExpectedParticipant, rows []workspaceConcurrencyRow, barrier workspaceBarrierStatus) error {
	if len(expected) != 2 {
		return fmt.Errorf("concurrency proof requires exactly two expected participants (got %d)", len(expected))
	}
	if len(rows) != 2 {
		return fmt.Errorf("concurrency proof requires two correlated rows (got %d)", len(rows))
	}
	if err := validateWorkspaceBarrierReadiness(barrier); err != nil {
		return err
	}
	expectedByWork, expectedByParticipant, err := indexExpectedParticipants(expected)
	if err != nil {
		return err
	}
	participantUIDs, seenParticipants, err := validateConcurrencyRows(rows, expectedByWork)
	if err != nil {
		return err
	}
	return validateWorkspaceBarrierCorrelation(expectedByParticipant, participantUIDs, seenParticipants, barrier)
}

// validateWorkspaceBarrierReadiness requires a ready barrier that describes
// exactly the two participant arrivals the proof correlates.
func validateWorkspaceBarrierReadiness(barrier workspaceBarrierStatus) error {
	if barrier.State != "ready" || !barrier.Ready || barrier.Failure != "" {
		return fmt.Errorf("workspace barrier is not ready: state=%s failure=%q", barrier.State, barrier.Failure)
	}
	if len(barrier.Expected) != 2 || len(barrier.Arrivals) != 2 {
		return fmt.Errorf("workspace barrier does not contain exactly two expected arrivals")
	}
	return nil
}

// indexExpectedParticipants rejects incomplete or duplicated expected
// participants and returns their work-item and participant lookups.
func indexExpectedParticipants(expected []workspaceExpectedParticipant) (map[string]workspaceExpectedParticipant, map[string]workspaceExpectedParticipant, error) {
	byWork := map[string]workspaceExpectedParticipant{}
	byParticipant := map[string]workspaceExpectedParticipant{}
	for _, participant := range expected {
		if participant.Participant == "" || participant.WorkItemID == "" || participant.AttemptID == "" || participant.SessionID == "" || participant.MarkerSHA256 == "" {
			return nil, nil, fmt.Errorf("expected participant identity is incomplete")
		}
		if _, duplicate := byWork[participant.WorkItemID]; duplicate {
			return nil, nil, fmt.Errorf("duplicate expected work item %s", participant.WorkItemID)
		}
		if _, duplicate := byParticipant[participant.Participant]; duplicate {
			return nil, nil, fmt.Errorf("duplicate expected participant %s", participant.Participant)
		}
		byWork[participant.WorkItemID] = participant
		byParticipant[participant.Participant] = participant
	}
	return byWork, byParticipant, nil
}

// concurrencyRowSeen tracks the execution identities already attributed while
// validating correlated rows, so a collapsed or reused identity fails closed.
type concurrencyRowSeen struct {
	turns   map[string]bool
	workers map[string]bool
	uids    map[uint32]bool
}

func newConcurrencyRowSeen() *concurrencyRowSeen {
	return &concurrencyRowSeen{turns: map[string]bool{}, workers: map[string]bool{}, uids: map[uint32]bool{}}
}

// observe validates one row against its expected participant and records the
// turn, worker, and UID it claims so a later row cannot reuse them.
func (seen *concurrencyRowSeen) observe(participant workspaceExpectedParticipant, row workspaceConcurrencyRow) error {
	if row.AttemptID != participant.AttemptID || row.SessionID != participant.SessionID {
		return fmt.Errorf("durable work/attempt/session identity mismatch for %s", participant.Participant)
	}
	if row.AssignmentState != "active" || row.TurnID == "" || row.WorkerID == "" {
		return fmt.Errorf("participant %s has no active assigned worker", participant.Participant)
	}
	if seen.turns[row.TurnID] {
		return fmt.Errorf("concurrent participants collapsed onto turn %s", row.TurnID)
	}
	seen.turns[row.TurnID] = true
	if seen.workers[row.WorkerID] {
		return fmt.Errorf("concurrent participants collapsed onto worker %s", row.WorkerID)
	}
	seen.workers[row.WorkerID] = true
	if row.AllocationState != "in_use" || row.AllocationUID < 10000 || row.AllocationUID >= 60000 {
		return fmt.Errorf("participant %s has no in-use in-range UID allocation", participant.Participant)
	}
	if seen.uids[row.AllocationUID] {
		return fmt.Errorf("concurrent participants collided on UID %d", row.AllocationUID)
	}
	seen.uids[row.AllocationUID] = true
	if row.AssignedSession != row.SessionID || row.AssignedUID != row.AllocationUID || row.AssignedGID != row.AllocationUID {
		return fmt.Errorf("assigned sandbox identity disagrees for participant %s", participant.Participant)
	}
	child, err := parseWorkspaceChildProof(row.ChildResult)
	if err != nil {
		return fmt.Errorf("participant %s: %w", participant.Participant, err)
	}
	if child.Participant != participant.Participant || child.SessionID != row.SessionID || child.UID != row.AllocationUID || child.GID != row.AllocationUID || child.MarkerSHA256 != participant.MarkerSHA256 {
		return fmt.Errorf("child-emitted identity disagrees for participant %s", participant.Participant)
	}
	return nil
}

// validateConcurrencyRows attributes every correlated row to its expected
// participant and returns the participant UID map plus the participants that
// were actually observed.
func validateConcurrencyRows(rows []workspaceConcurrencyRow, expectedByWork map[string]workspaceExpectedParticipant) (map[string]uint32, map[string]bool, error) {
	seen := newConcurrencyRowSeen()
	participantUIDs := map[string]uint32{}
	seenParticipants := map[string]bool{}
	for _, row := range rows {
		participant, ok := expectedByWork[row.WorkItemID]
		if !ok {
			return nil, nil, fmt.Errorf("unattributed work item %s", row.WorkItemID)
		}
		if err := seen.observe(participant, row); err != nil {
			return nil, nil, err
		}
		participantUIDs[participant.Participant] = row.AllocationUID
		seenParticipants[participant.Participant] = true
	}
	return participantUIDs, seenParticipants, nil
}

// validateWorkspaceBarrierCorrelation proves the barrier configured and observed
// exactly the participants whose durable work the rows correlated.
func validateWorkspaceBarrierCorrelation(expectedByParticipant map[string]workspaceExpectedParticipant, participantUIDs map[string]uint32, seenParticipants map[string]bool, barrier workspaceBarrierStatus) error {
	configured := map[string]workspaceExpectedParticipant{}
	for _, participant := range barrier.Expected {
		if _, duplicate := configured[participant.Participant]; duplicate {
			return fmt.Errorf("barrier repeats expected participant %s", participant.Participant)
		}
		configured[participant.Participant] = participant
	}
	arrived := map[string]workspaceBarrierArrival{}
	for _, arrival := range barrier.Arrivals {
		if _, duplicate := arrived[arrival.Participant]; duplicate {
			return fmt.Errorf("barrier repeats arrival for participant %s", arrival.Participant)
		}
		arrived[arrival.Participant] = arrival
	}
	for participantName, participant := range expectedByParticipant {
		configuredParticipant, configuredOK := configured[participantName]
		arrival, arrivalOK := arrived[participantName]
		if !configuredOK || !arrivalOK {
			return fmt.Errorf("barrier is missing participant %s", participantName)
		}
		if configuredParticipant.WorkItemID != participant.WorkItemID || configuredParticipant.AttemptID != participant.AttemptID || configuredParticipant.SessionID != participant.SessionID {
			return fmt.Errorf("barrier configuration identity mismatch for participant %s", participantName)
		}
		if arrival.WorkItemID != participant.WorkItemID || arrival.AttemptID != participant.AttemptID || arrival.SessionID != participant.SessionID ||
			arrival.MarkerSHA256 != participant.MarkerSHA256 || arrival.UID != arrival.GID || arrival.UID != participantUIDs[participantName] {
			return fmt.Errorf("barrier arrival identity mismatch for participant %s", participantName)
		}
		if !seenParticipants[participantName] {
			return fmt.Errorf("correlated proof is missing participant %s", participantName)
		}
	}
	return nil
}

func validWorkspaceConcurrencyProof() ([]workspaceExpectedParticipant, []workspaceConcurrencyRow, workspaceBarrierStatus) {
	proofLine := func(participant, session string, uid uint32, digest string) string {
		return fmt.Sprintf("workspace-child-proof participant=%s session=%s uid=%d gid=%d marker_sha256=%s invariants=groups-cleared,caps-cleared,no-new-privs,umask-0077,pool-root-0:0:0711,session-tree-0700,sibling-EACCES,tls-key-EACCES", participant, session, uid, uid, digest)
	}
	expected := []workspaceExpectedParticipant{
		{Participant: workspaceParticipantA, WorkItemID: "work-a", AttemptID: "attempt-a", SessionID: "durable-a", MarkerSHA256: strings.Repeat("a", 64)},
		{Participant: workspaceParticipantB, WorkItemID: "work-b", AttemptID: "attempt-b", SessionID: "durable-b", MarkerSHA256: strings.Repeat("b", 64)},
	}
	rows := []workspaceConcurrencyRow{
		{WorkItemID: "work-a", AttemptID: "attempt-a", SessionID: "durable-a", AssignmentState: "active", TurnID: "turn-a", WorkerID: "worker-a", AllocationState: "in_use", AllocationUID: 10000, AssignedSession: "durable-a", AssignedUID: 10000, AssignedGID: 10000, ChildResult: proofLine(workspaceParticipantA, "durable-a", 10000, strings.Repeat("a", 64))},
		{WorkItemID: "work-b", AttemptID: "attempt-b", SessionID: "durable-b", AssignmentState: "active", TurnID: "turn-b", WorkerID: "worker-b", AllocationState: "in_use", AllocationUID: 10001, AssignedSession: "durable-b", AssignedUID: 10001, AssignedGID: 10001, ChildResult: proofLine(workspaceParticipantB, "durable-b", 10001, strings.Repeat("b", 64))},
	}
	barrier := workspaceBarrierStatus{State: "ready", Ready: true, Expected: append([]workspaceExpectedParticipant(nil), expected...)}
	barrier.Arrivals = []workspaceBarrierArrival{
		{Participant: workspaceParticipantA, WorkItemID: "work-a", AttemptID: "attempt-a", SessionID: "durable-a", UID: 10000, GID: 10000, MarkerSHA256: strings.Repeat("a", 64)},
		{Participant: workspaceParticipantB, WorkItemID: "work-b", AttemptID: "attempt-b", SessionID: "durable-b", UID: 10001, GID: 10001, MarkerSHA256: strings.Repeat("b", 64)},
	}
	return expected, rows, barrier
}

func TestValidateWorkspaceConcurrencyProofFailsClosed(t *testing.T) {
	if err := func() error {
		expected, rows, barrier := validWorkspaceConcurrencyProof()
		return validateWorkspaceConcurrencyProof(expected, rows, barrier)
	}(); err != nil {
		t.Fatalf("valid proof failed: %v", err)
	}

	tests := []struct {
		name string
		edit func([]workspaceExpectedParticipant, []workspaceConcurrencyRow, *workspaceBarrierStatus)
		want string
	}{
		{name: "UID collision", edit: func(_ []workspaceExpectedParticipant, rows []workspaceConcurrencyRow, barrier *workspaceBarrierStatus) {
			rows[1].AllocationUID, rows[1].AssignedUID, rows[1].AssignedGID = 10000, 10000, 10000
			rows[1].ChildResult = strings.ReplaceAll(rows[1].ChildResult, "10001", "10000")
			barrier.Arrivals[1].UID, barrier.Arrivals[1].GID = 10000, 10000
		}, want: "collided on UID"},
		{name: "missing allocation", edit: func(_ []workspaceExpectedParticipant, rows []workspaceConcurrencyRow, _ *workspaceBarrierStatus) {
			rows[1].AllocationState, rows[1].AllocationUID = "", 0
		}, want: "no in-use in-range UID allocation"},
		{name: "prematurely freed allocation", edit: func(_ []workspaceExpectedParticipant, rows []workspaceConcurrencyRow, _ *workspaceBarrierStatus) {
			rows[1].AllocationState = "freed"
		}, want: "no in-use in-range UID allocation"},
		{name: "assigned identity mismatch", edit: func(_ []workspaceExpectedParticipant, rows []workspaceConcurrencyRow, _ *workspaceBarrierStatus) {
			rows[1].AssignedUID = 10002
		}, want: "assigned sandbox identity disagrees"},
		{name: "child identity mismatch", edit: func(_ []workspaceExpectedParticipant, rows []workspaceConcurrencyRow, _ *workspaceBarrierStatus) {
			rows[1].ChildResult = strings.Replace(rows[1].ChildResult, "session=durable-b", "session=wrong", 1)
		}, want: "child-emitted identity disagrees"},
		{name: "duplicate barrier arrival", edit: func(_ []workspaceExpectedParticipant, _ []workspaceConcurrencyRow, barrier *workspaceBarrierStatus) {
			barrier.Arrivals[1].Participant = workspaceParticipantA
		}, want: "barrier repeats arrival"},
		{name: "barrier participant UID mismatch", edit: func(_ []workspaceExpectedParticipant, _ []workspaceConcurrencyRow, barrier *workspaceBarrierStatus) {
			barrier.Arrivals[1].UID, barrier.Arrivals[1].GID = 10000, 10000
		}, want: "barrier arrival identity mismatch"},
		{name: "one worker sequential execution", edit: func(_ []workspaceExpectedParticipant, rows []workspaceConcurrencyRow, _ *workspaceBarrierStatus) {
			rows[1].WorkerID = rows[0].WorkerID
		}, want: "collapsed onto worker"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected, rows, barrier := validWorkspaceConcurrencyProof()
			test.edit(expected, rows, &barrier)
			err := validateWorkspaceConcurrencyProof(expected, rows, barrier)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid proof did not fail with %q: %v", test.want, err)
			}
		})
	}
}
