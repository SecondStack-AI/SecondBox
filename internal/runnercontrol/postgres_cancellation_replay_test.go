package runnercontrol

import (
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"google.golang.org/protobuf/proto"
)

func TestOpenConnectionReplaysDeliveredDataPlaneCancellation(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Now().UTC()
	command := &runnerv1.DataPlaneCancelCommand{MessageId: "cancel-replay", Sequence: 7,
		Fence:       &runnerv1.AssignmentFence{AssignmentId: "assignment", SandboxId: "sandbox", InstanceId: "instance", SandboxGeneration: 1, FencingToken: []byte("fence")},
		OperationId: "operation", StreamId: "stream", Kind: runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE, Reason: "upload disconnected"}
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{Message: &runnerv1.ControlPlaneToRunner_DataPlaneCancel{DataPlaneCancel: command}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `INSERT INTO secondbox.runner_commands (id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at,delivered_at) VALUES ($1,'runner-home','assignment','data-plane-cancel',$2,'delivered','connection-old',1,$3,$3,$3)`, command.MessageId, payload, now); err != nil {
		t.Fatal(err)
	}
	if err := store.OpenConnection(t.Context(), RunnerIdentity{RunnerID: "runner-home", CredentialSerial: "credential"}, "connection-new", 1, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.ClaimCommands(t.Context(), "runner-home", "connection-new", 10, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range deliveries {
		if delivery.ID != command.MessageId {
			continue
		}
		replay := delivery.Message.GetDataPlaneCancel()
		if replay == nil || replay.Sequence == command.Sequence || replay.Sequence == 0 {
			t.Fatalf("replayed cancellation has no new connection sequence: %v", replay)
		}
		expected := proto.Clone(command).(*runnerv1.DataPlaneCancelCommand)
		expected.Sequence = replay.Sequence
		if !proto.Equal(replay, expected) {
			t.Fatalf("replay changed cancellation identity: %v", replay)
		}
		if err := store.MarkCommandDelivered(t.Context(), delivery, "connection-new", now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("delivered cancellation was not replayed on reconnect")
}
