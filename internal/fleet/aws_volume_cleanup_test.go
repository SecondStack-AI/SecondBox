package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

type fleetEC2Fixture struct {
	instance      ec2types.Instance
	volumes       map[string]ec2types.Volume
	deleted       []string
	instanceError error
}

func (f *fleetEC2Fixture) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.instanceError != nil {
		return nil, f.instanceError
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{f.instance}}}}, nil
}
func (f *fleetEC2Fixture) DescribeVolumes(_ context.Context, input *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{f.volumes[input.VolumeIds[0]]}}, nil
}
func (f *fleetEC2Fixture) DeleteVolume(_ context.Context, input *ec2.DeleteVolumeInput, _ ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	f.deleted = append(f.deleted, *input.VolumeId)
	return &ec2.DeleteVolumeOutput{}, nil
}

func TestFleetRetiredVolumeCleanupRequiresReceiptAndDetachedOwnedDisk(t *testing.T) {
	database := openFleetTestDatabase(t)
	now := time.Now().UTC()
	if _, err := database.Exec(t.Context(), `INSERT INTO secondbox.runners(id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,software_version,active_connection_id,last_sequence,drain_phase,reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,sandbox_start_p95_milliseconds,revision,created_at,updated_at,fleet_state) VALUES('runner-i-test','code','test','offline','[]','[]','{}','[]',1,1,'test','',0,'drained','{}','{}',0,0,1,$1,$1,'retired')`, now); err != nil {
		t.Fatal(err)
	}
	tags := []ec2types.Tag{{Key: aws.String("secondstack.ai/fleet-ref"), Value: aws.String("run")}, {Key: aws.String("secondbox-fleet"), Value: aws.String("group")}}
	ec2Fixture := &fleetEC2Fixture{instance: ec2types.Instance{InstanceId: aws.String("i-test"), Tags: tags, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameTerminated}}, volumes: map[string]ec2types.Volume{
		"vol-safe":      {VolumeId: aws.String("vol-safe"), Tags: tags, State: ec2types.VolumeStateInUse, Attachments: []ec2types.VolumeAttachment{{InstanceId: aws.String("i-test")}}},
		"vol-unplanned": {VolumeId: aws.String("vol-unplanned"), Tags: tags, State: ec2types.VolumeStateAvailable},
	}}
	cloud := AWSCloud{Database: database, Pool: "code", GroupName: "group", FleetRef: "run", EC2: ec2Fixture}
	if err := cloud.CleanupRetiredVolumes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ec2Fixture.deleted) != 0 {
		t.Fatal("deleted unreceipted volume")
	}
	ec2Fixture.instance.RootDeviceName = aws.String("/dev/sda1")
	ec2Fixture.instance.BlockDeviceMappings = []ec2types.InstanceBlockDeviceMapping{
		{DeviceName: aws.String("/dev/sda1"), Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-root"), DeleteOnTermination: aws.Bool(true)}},
		{DeviceName: aws.String("/dev/sdf"), Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-safe"), DeleteOnTermination: aws.Bool(false)}},
	}
	if err := cloud.recordRetiredNodeVolumes(t.Context(), Node{InstanceID: "i-test", RunnerID: "runner-i-test"}); err != nil {
		t.Fatal(err)
	}
	if err := cloud.CleanupRetiredVolumes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ec2Fixture.deleted) != 0 {
		t.Fatal("deleted attached volume")
	}
	volume := ec2Fixture.volumes["vol-safe"]
	volume.State = ec2types.VolumeStateAvailable
	volume.Attachments = nil
	volume.Tags = nil
	ec2Fixture.volumes["vol-safe"] = volume
	if err := cloud.CleanupRetiredVolumes(t.Context()); err == nil {
		t.Fatal("foreign volume accepted")
	}
	volume.Tags = tags
	ec2Fixture.volumes["vol-safe"] = volume
	ec2Fixture.instanceError = &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound", Message: "terminated instance no longer visible"}
	if err := cloud.CleanupRetiredVolumes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ec2Fixture.deleted) != 1 || ec2Fixture.deleted[0] != "vol-safe" {
		t.Fatalf("deleted=%v", ec2Fixture.deleted)
	}
	var completed bool
	if err := database.QueryRow(t.Context(), `SELECT completed_at IS NOT NULL FROM secondbox.fleet_node_terminations WHERE instance_id='i-test'`).Scan(&completed); err != nil || !completed {
		t.Fatalf("completed=%v err=%v", completed, err)
	}
}
