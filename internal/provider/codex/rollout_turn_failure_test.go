package codex

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// capturedRoutingFailure is the terminal row codex-cli 0.163.0 wrote when a
// turn failed offline on a ChatGPT login (captured 2026-10-26).
const capturedRoutingFailure = `{"timestamp":"2026-10-26T20:26:29.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"01a17bc4-853d-7e02-86e4-9129a2a78cd9","last_agent_message":null,"error":{"message":"workspace routing discovery failed","codex_error_info":"other"},"started_at":1793077839,"completed_at":1793077989,"duration_ms":150376}}`

func TestReadRolloutStateShouldReportUnreachableWhenTaskCompleteCarriesWorkspaceRoutingDiscoveryFailed(t *testing.T) {
	state, err := ReadRolloutState(writeRollout(t, capturedRoutingFailure+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Runtime != agentgraph.RuntimeIdle || state.TurnFailure != "unreachable" || state.UsageLimit != nil {
		t.Fatalf("state = %+v, want idle with an unreachable failure", state)
	}
}

func TestReadRolloutStateShouldReportUnreachableWhenTheErrorIsAResponseStreamDisconnect(t *testing.T) {
	line := `{"timestamp":"2026-10-26T20:26:29Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"stream","codex_error_info":{"response_stream_disconnected":{"http_status_code":null}}}}}`
	state, err := ReadRolloutState(writeRollout(t, line+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if state.TurnFailure != "unreachable" {
		t.Fatalf("turn failure = %q, want unreachable", state.TurnFailure)
	}
}

func TestReadRolloutStateShouldReportNoTurnFailureWhenTheErrorIsAUsageLimit(t *testing.T) {
	state, err := ReadRolloutState(rolloutFixtureWith(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if state.UsageLimit == nil || state.TurnFailure != "" {
		t.Fatalf("limit %+v failure %q, want the limit alone", state.UsageLimit, state.TurnFailure)
	}
}

func TestReadRolloutStateShouldReportNoTurnFailureWhenALaterTaskStartedFollowsIt(t *testing.T) {
	body := capturedRoutingFailure + "\n" + rolloutMarker("task_started", "2026-10-26T20:30:00Z") + "\n"
	state, err := ReadRolloutState(writeRollout(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if state.Runtime != agentgraph.RuntimeActive || state.TurnFailure != "" {
		t.Fatalf("state = %+v, want active with no failure", state)
	}
}

func failedTurn(completedAt int64) rpcTurn {
	return rpcTurn{ID: "t" + itoa(completedAt), StartedAt: completedAt - 5, CompletedAt: completedAt, Error: &rpcTurnError{
		Message:        "stream",
		CodexErrorInfo: []byte(`{"responseStreamDisconnected":{"httpStatusCode":null}}`),
	}}
}

func TestObserveTurnFailureShouldRecordWhenTheNewestTurnFailed(t *testing.T) {
	state := &graphState{}
	state.observeTurnFailure([]rpcTurn{{ID: "a", StartedAt: 90, CompletedAt: 95}, failedTurn(200)})
	if !state.turnFailureAt.Equal(time.Unix(200, 0)) || state.turnFailureKind != "unreachable" {
		t.Fatalf("failure = %v %q, want unreachable at 200", state.turnFailureAt, state.turnFailureKind)
	}
}

func TestObserveTurnFailureShouldNotRecordWhenANewerTurnSucceeded(t *testing.T) {
	state := &graphState{}
	state.observeTurnFailure([]rpcTurn{failedTurn(200), {ID: "b", StartedAt: 300, CompletedAt: 310}})
	if !state.turnFailureAt.IsZero() || state.turnFailureKind != "" {
		t.Fatalf("failure = %v %q, want none", state.turnFailureAt, state.turnFailureKind)
	}
}

func TestObserveTurnFailureShouldNotRecordWhenTheErrorIsAUsageLimit(t *testing.T) {
	state := &graphState{}
	turn := failedTurn(200)
	turn.Error.CodexErrorInfo = []byte(`"usageLimitExceeded"`)
	state.observeTurnFailure([]rpcTurn{turn})
	if state.turnFailureKind != "" {
		t.Fatalf("kind = %q, want none", state.turnFailureKind)
	}
}
