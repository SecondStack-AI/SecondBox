package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// The receipt is created only after permanent Runner retirement and before
// termination. Unplanned-loss volumes have no receipt and are never deleted.
func (cloud AWSCloud) recordRetiredNodeVolumes(ctx context.Context, node Node) error {
	if cloud.Database == nil || cloud.Pool == "" {
		return errors.New("SecondBox AWS fleet requires durable retired-volume inventory")
	}
	result, err := cloud.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{node.InstanceID}})
	if err != nil {
		return err
	}
	var volumes []string
	found := false
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			if aws.ToString(instance.InstanceId) != node.InstanceID || !cloud.ownsInstance(instance) {
				return errors.New("SecondBox AWS retired-volume instance ownership differs")
			}
			found = true
			for _, device := range instance.BlockDeviceMappings {
				if aws.ToString(device.DeviceName) != aws.ToString(instance.RootDeviceName) && device.Ebs != nil {
					id := aws.ToString(device.Ebs.VolumeId)
					if id == "" || aws.ToBool(device.Ebs.DeleteOnTermination) {
						return errors.New("SecondBox AWS workspace volume lacks retained identity")
					}
					volumes = append(volumes, id)
				}
			}
		}
	}
	if !found || len(volumes) == 0 || len(volumes) > 16 {
		return errors.New("SecondBox AWS retired-volume inventory is absent or exceeds its bound")
	}
	encoded, err := json.Marshal(volumes)
	if err != nil {
		return err
	}
	tx, err := cloud.Database.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var retired bool
	if err := tx.QueryRow(ctx, `SELECT fleet_state='retired' FROM secondbox.runners WHERE id=$1 AND pool_name=$2 FOR UPDATE`, node.RunnerID, cloud.Pool).Scan(&retired); err != nil {
		return err
	}
	if !retired {
		return errors.New("SecondBox AWS refuses volume cleanup authority for a non-retired Runner")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.fleet_node_terminations(instance_id,pool_name,runner_id,group_name,fleet_ref,volume_ids,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(instance_id) DO NOTHING`, node.InstanceID, cloud.Pool, node.RunnerID, cloud.GroupName, cloud.FleetRef, encoded, time.Now().UTC()); err != nil {
		return err
	}
	var same bool
	if err := tx.QueryRow(ctx, `SELECT pool_name=$2 AND runner_id=$3 AND group_name=$4 AND fleet_ref=$5 AND volume_ids=$6::jsonb FROM secondbox.fleet_node_terminations WHERE instance_id=$1`, node.InstanceID, cloud.Pool, node.RunnerID, cloud.GroupName, cloud.FleetRef, encoded).Scan(&same); err != nil {
		return err
	}
	if !same {
		return errors.New("SecondBox AWS retired-volume receipt differs")
	}
	return tx.Commit(ctx)
}

func (cloud AWSCloud) ownsInstance(instance ec2types.Instance) bool {
	fleetRef, fleet := "", ""
	for _, tag := range instance.Tags {
		if aws.ToString(tag.Key) == "secondstack.ai/fleet-ref" {
			fleetRef = aws.ToString(tag.Value)
		}
		if aws.ToString(tag.Key) == "secondbox-fleet" {
			fleet = aws.ToString(tag.Value)
		}
	}
	return fleetRef == cloud.FleetRef && fleet == cloud.GroupName
}

func (cloud AWSCloud) CleanupRetiredVolumes(ctx context.Context) error {
	if cloud.Database == nil || cloud.Pool == "" {
		return errors.New("SecondBox AWS fleet requires durable retired-volume inventory")
	}
	rows, err := cloud.Database.Query(ctx, `SELECT t.instance_id,t.volume_ids FROM secondbox.fleet_node_terminations t JOIN secondbox.runners r ON r.id=t.runner_id WHERE t.pool_name=$1 AND t.group_name=$2 AND t.fleet_ref=$3 AND t.completed_at IS NULL AND r.fleet_state='retired' ORDER BY t.created_at LIMIT 64`, cloud.Pool, cloud.GroupName, cloud.FleetRef)
	if err != nil {
		return err
	}
	type receipt struct {
		id      string
		volumes []string
	}
	var receipts []receipt
	for rows.Next() {
		var entry receipt
		var encoded []byte
		if err := rows.Scan(&entry.id, &encoded); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(encoded, &entry.volumes); err != nil {
			rows.Close()
			return err
		}
		if len(entry.volumes) == 0 || len(entry.volumes) > 16 {
			rows.Close()
			return errors.New("SecondBox AWS retired-volume receipt bounds invalid")
		}
		receipts = append(receipts, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, entry := range receipts {
		result, err := cloud.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{entry.id}})
		absent := isAWSInstanceAbsent(err)
		if err != nil && !absent {
			return err
		}
		// EC2 eventually forgets terminated instances. The committed retirement
		// receipt still names exact volumes; ownership and detachment below
		// remain mandatory before any deletion.
		terminated := absent
		if result != nil {
			for _, reservation := range result.Reservations {
				for _, instance := range reservation.Instances {
					if aws.ToString(instance.InstanceId) != entry.id || !cloud.ownsInstance(instance) {
						return errors.New("SecondBox AWS retired-volume owner differs before cleanup")
					}
					terminated = instance.State != nil && instance.State.Name == ec2types.InstanceStateNameTerminated
				}
			}
		}
		if !terminated {
			continue
		}
		complete := true
		for _, id := range entry.volumes {
			result, err := cloud.EC2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
			if isAWSVolumeAbsent(err) {
				continue
			}
			if err != nil {
				return err
			}
			if len(result.Volumes) != 1 {
				return errors.New("SecondBox AWS retired-volume identity is ambiguous")
			}
			volume := result.Volumes[0]
			fleetRef, fleet := "", ""
			for _, tag := range volume.Tags {
				if aws.ToString(tag.Key) == "secondstack.ai/fleet-ref" {
					fleetRef = aws.ToString(tag.Value)
				}
				if aws.ToString(tag.Key) == "secondbox-fleet" {
					fleet = aws.ToString(tag.Value)
				}
			}
			if aws.ToString(volume.VolumeId) != id || fleetRef != cloud.FleetRef || fleet != cloud.GroupName {
				return fmt.Errorf("SecondBox AWS retired-volume %s ownership differs", id)
			}
			if volume.State != ec2types.VolumeStateAvailable || len(volume.Attachments) != 0 {
				complete = false
				continue
			}
			if _, err := cloud.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(id)}); err != nil && !isAWSVolumeAbsent(err) {
				return err
			}
		}
		if complete {
			if _, err := cloud.Database.Exec(ctx, `UPDATE secondbox.fleet_node_terminations SET completed_at=$2 WHERE instance_id=$1 AND completed_at IS NULL`, entry.id, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

func isAWSVolumeAbsent(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "InvalidVolume.NotFound"
}

func isAWSInstanceAbsent(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "InvalidInstanceID.NotFound"
}
