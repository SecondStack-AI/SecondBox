package imagepreparation

import (
	"context"
	"encoding/json"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

const Deadline = 30 * time.Minute
const MaximumTargets = 16

type Target struct {
	RunnerID      string   `json:"runnerId"`
	Architectures []string `json:"architectures"`
}

type Payload struct {
	Role    string   `json:"role"`
	Targets []Target `json:"targets,omitempty"`
}

func Queue(ctx context.Context, tx pgx.Tx, effectID, operationID, tenant, reference, runner string, payload Payload, now, deadline time.Time) error {
	metadata, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	command, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{Message: &runnerv1.ControlPlaneToRunner_PrepareImage{PrepareImage: &runnerv1.PrepareImageCommand{OperationId: effectID, TenantRef: tenant, Reference: reference, DeadlineUnixMs: uint64(deadline.UnixMilli())}}})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO secondbox.lifecycle_effects
	 (id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,created_at,updated_at)
	 VALUES ($1,'',0,'prepare_image','queued',$2,'',$3,$1,'','',0,0,$4,'',$5,'','',$6,'{}',$5,$5)`, effectID, operationID, runner, deadline, now, metadata)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO secondbox.runner_commands
	 (id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at,delivered_at)
	 VALUES ($1,$2,$1,'prepare-image',$3,'pending','',0,$4,$4,NULL)`, effectID, runner, command, now)
	return err
}
