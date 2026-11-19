package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AutoScalingAPI interface {
	DescribeAutoScalingGroups(context.Context, *autoscaling.DescribeAutoScalingGroupsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	SetDesiredCapacity(context.Context, *autoscaling.SetDesiredCapacityInput, ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error)
	TerminateInstanceInAutoScalingGroup(context.Context, *autoscaling.TerminateInstanceInAutoScalingGroupInput, ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error)
}

type EC2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DeleteVolume(context.Context, *ec2.DeleteVolumeInput, ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
}

type AWSCloud struct {
	AutoScaling      AutoScalingAPI
	EC2              EC2API
	GroupName        string
	MaximumNodes     int
	QualificationRun string
	Database         *pgxpool.Pool
	Pool             string
}

func (cloud AWSCloud) ListNodes(ctx context.Context) ([]Node, error) {
	if cloud.AutoScaling == nil || cloud.EC2 == nil || cloud.GroupName == "" || cloud.MaximumNodes < 1 || cloud.QualificationRun == "" {
		return nil, errors.New("SecondBox AWS fleet requires clients, named group, ownership tag and maximum size")
	}
	groups, err := cloud.AutoScaling.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{cloud.GroupName}})
	if err != nil {
		return nil, err
	}
	if len(groups.AutoScalingGroups) != 1 {
		return nil, errors.New("SecondBox AWS fleet requires exactly its configured Auto Scaling group")
	}
	group := groups.AutoScalingGroups[0]
	owned := false
	for _, tag := range group.Tags {
		if aws.ToString(tag.Key) == "secondstack.ai/qualification-run" && aws.ToString(tag.Value) == cloud.QualificationRun {
			owned = true
		}
	}
	if !owned || aws.ToInt32(group.MaxSize) > int32(cloud.MaximumNodes) {
		return nil, errors.New("SecondBox AWS fleet group ownership or maximum capacity differs")
	}
	ids := make([]string, 0, len(group.Instances))
	for _, instance := range group.Instances {
		if instance.InstanceId != nil {
			ids = append(ids, *instance.InstanceId)
		}
	}
	if len(ids) == 0 {
		return []Node{}, nil
	}
	instances, err := cloud.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(ids))
	for _, reservation := range instances.Reservations {
		for _, instance := range reservation.Instances {
			instanceID := aws.ToString(instance.InstanceId)
			if !strings.HasPrefix(instanceID, "i-") || instance.LaunchTime == nil {
				return nil, errors.New("SecondBox AWS fleet instance lacks a launch identity")
			}
			owned := false
			for _, tag := range instance.Tags {
				if aws.ToString(tag.Key) == "secondstack.ai/qualification-run" && aws.ToString(tag.Value) == cloud.QualificationRun {
					owned = true
				}
			}
			if !owned {
				return nil, fmt.Errorf("SecondBox AWS fleet instance %s lacks its ownership tag", instanceID)
			}
			node := Node{InstanceID: instanceID, RunnerID: "runner-" + instanceID, CreatedAt: *instance.LaunchTime}
			if instance.State != nil {
				node.State = string(instance.State.Name)
			}
			nodes = append(nodes, node)
		}
	}
	return nodes, nil
}

func (cloud AWSCloud) SetDesired(ctx context.Context, count int) error {
	if count < 0 || count > cloud.MaximumNodes {
		return errors.New("SecondBox AWS fleet requested capacity exceeds its ceiling")
	}
	if _, err := cloud.ListNodes(ctx); err != nil {
		return err
	}
	_, err := cloud.AutoScaling.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{AutoScalingGroupName: aws.String(cloud.GroupName), DesiredCapacity: aws.Int32(int32(count)), HonorCooldown: aws.Bool(false)})
	return err
}

func (cloud AWSCloud) Terminate(ctx context.Context, node Node) error {
	nodes, err := cloud.ListNodes(ctx)
	if err != nil {
		return err
	}
	owned := false
	for _, candidate := range nodes {
		if candidate.InstanceID == node.InstanceID && candidate.RunnerID == node.RunnerID {
			owned = true
		}
	}
	if !owned {
		return errors.New("SecondBox AWS fleet refuses termination outside its exact inventory")
	}
	if err := cloud.recordRetiredNodeVolumes(ctx, node); err != nil {
		return err
	}
	_, err = cloud.AutoScaling.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{InstanceId: aws.String(node.InstanceID), ShouldDecrementDesiredCapacity: aws.Bool(true)})
	return err
}
