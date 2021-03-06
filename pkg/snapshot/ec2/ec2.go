package ec2

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awsec2 "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	"github.com/grid-x/aws-auto-snapshot/pkg/datastore"
)

const (
	defaultBackupTag    = "backup"
	defaultRetentionTag = "retention"

	defaultSnapshotSuffix = "auto-snapshot"
	defaultDeleteAfterTag = "_DELETE_AFTER"

	defaultRetentionDays = 7 // Default are 7 days retention
	defaultDescription   = "auto snapshot created by grid-x/aws-auto-snapshot"
)

var (
	describeVolumesRequets = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ec2_describe_volumes_requests_total",
		Help: "Total number of describe volumes requests",
	})
	describeSnapshotsRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ec2_describe_snapshots_requests_total",
		Help: "Total number of describe snapshots requests",
	})
	createSnapshotRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ec2_create_snapshot_requests_total",
		Help: "Total number of create snapshot requests",
	})
	createTagsRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ec2_create_tags_requests_total",
		Help: "Total number of create tags requests",
	})
	deleteSnapshotRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ec2_delete_snapshot_requests_total",
		Help: "Total number of delete snapshot requests",
	})
)

func init() {
	prometheus.MustRegister(describeVolumesRequets)
	prometheus.MustRegister(describeSnapshotsRequests)
	prometheus.MustRegister(createSnapshotRequests)
	prometheus.MustRegister(createTagsRequests)
	prometheus.MustRegister(deleteSnapshotRequests)
}

// SnapshotManager manages the snapshot creation and pruning of EC2 EBS-based
// snapshots
type SnapshotManager struct {
	client   *awsec2.EC2
	volumeID string

	suffix         string // snapshot suffix
	backupTag      string
	retentionTag   string
	deleteAfterTag string

	logger log.FieldLogger

	datastore datastore.Datastore
}

// Opt is the type for Options of the SnapshotManager
type Opt func(*SnapshotManager)

// WithRetentionTag sets the retention tag key
func WithRetentionTag(t string) Opt {
	return func(m *SnapshotManager) {
		m.retentionTag = t
	}
}

// WithBackupTag sets the backup tag key
func WithBackupTag(t string) Opt {
	return func(m *SnapshotManager) {
		m.backupTag = t
	}
}

// WithSnapshotSuffix sets the automated snapshot suffix
func WithSnapshotSuffix(suf string) Opt {
	return func(m *SnapshotManager) {
		m.suffix = suf
	}
}

// WithDeleteAfterTag sets the tag key to be used for indication the deletion
// date
func WithDeleteAfterTag(tag string) Opt {
	return func(m *SnapshotManager) {
		m.deleteAfterTag = tag
	}
}

// NewSnapshotManager creates a new SnapshotManager given an EC2 client and a
// set of Opts
func NewSnapshotManager(client *awsec2.EC2, datastore datastore.Datastore, opts ...Opt) *SnapshotManager {
	smgr := &SnapshotManager{
		client: client,

		suffix:         defaultSnapshotSuffix,
		retentionTag:   defaultRetentionTag,
		backupTag:      defaultBackupTag,
		deleteAfterTag: defaultDeleteAfterTag,

		logger: log.New().WithFields(
			log.Fields{
				"component": "ec2-snapshot-manager",
			},
		),
		datastore: datastore,
	}

	for _, o := range opts {
		o(smgr)
	}

	return smgr
}

func (smgr *SnapshotManager) fetchVolumes(ctx context.Context) ([]*awsec2.Volume, error) {
	var result []*awsec2.Volume
	var token *string
	for {
		in := &awsec2.DescribeVolumesInput{}
		if token != nil {
			in.NextToken = token
		}

		// Filter so we get only volumes that have the Backup tag set
		in.SetFilters([]*awsec2.Filter{
			{
				Name: aws.String("tag-key"),
				Values: []*string{
					aws.String(smgr.backupTag),
					aws.String(strings.ToLower(smgr.backupTag)), // we are not case sensitive
				},
			},
		})

		resp, err := smgr.client.DescribeVolumesWithContext(ctx, in)
		describeVolumesRequets.Inc()
		if err != nil {
			return nil, err
		}
		for _, volume := range resp.Volumes {
			if volume.VolumeId == nil {
				//skip
				continue
			}
			result = append(result, volume)
		}

		if resp.NextToken == nil {
			break
		}
		token = resp.NextToken
	}

	return result, nil
}

func (smgr *SnapshotManager) fetchSnapshots(ctx context.Context) ([]*awsec2.Snapshot, error) {
	var result []*awsec2.Snapshot
	var token *string
	for {
		in := &awsec2.DescribeSnapshotsInput{}
		if token != nil {
			in.NextToken = token
		}

		// Filter so we get only volumes that have the Backup tag set
		in.SetFilters([]*awsec2.Filter{
			{
				Name: aws.String("tag-key"),
				Values: []*string{
					aws.String(smgr.deleteAfterTag),
				},
			},
		})

		resp, err := smgr.client.DescribeSnapshotsWithContext(ctx, in)
		describeSnapshotsRequests.Inc()
		if err != nil {
			return nil, err
		}
		for _, snap := range resp.Snapshots {
			if snap.SnapshotId == nil {
				//skip
				continue
			}
			result = append(result, snap)
		}

		if resp.NextToken == nil {
			break
		}
		token = resp.NextToken
	}

	return result, nil
}

// Snapshot creates EBS snapshots for all matching EBS volumes, i.e. all EBS
// volumes having a Backup tag and optionally a retention tag set
func (smgr *SnapshotManager) Snapshot(ctx context.Context) error {

	volumes, err := smgr.fetchVolumes(ctx)
	if err != nil {
		return err
	}

	for _, volume := range volumes {
		// For each volume it should at most take 5 minutes
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		snapshotName := fmt.Sprintf("%s-%d-%s",
			*volume.VolumeId,
			time.Now().UnixNano(),
			smgr.suffix,
		)

		logger := smgr.logger.WithFields(
			log.Fields{
				"volume-id":     volume.VolumeId,
				"snapshot-name": snapshotName,
			},
		)

		var days int64
		for _, tag := range volume.Tags {
			if tag.Key == nil {
				continue
			}
			if strings.ToLower(*tag.Key) == strings.ToLower(smgr.retentionTag) {
				if tag.Value == nil {
					logger.Warnf("Retention tag value is nil")
					continue
				}
				days, err = strconv.ParseInt(*tag.Value, 10, 64)
				if err != nil {
					logger.Warnf("Couldn't parse retention days: %+v. Falling back to default value", err)
					days = defaultRetentionDays // if error occurs fall back to default retention time
				}
				break
			}
		}

		if days == 0 {
			days = defaultRetentionDays
		}

		created := time.Now()
		deleteAfter := created.Add(time.Duration(days) * 24 * time.Hour)

		logger.Infof("Creating snapshot with name %s", snapshotName)
		snapshot, err := smgr.client.CreateSnapshotWithContext(
			ctx,
			&awsec2.CreateSnapshotInput{
				VolumeId:    volume.VolumeId,
				Description: aws.String(defaultDescription),
			},
		)
		createSnapshotRequests.Inc()
		if err != nil {
			logger.Error(err)
			continue
		}

		if snapshot.SnapshotId == nil {
			logger.Errorf("Snapshot ID is nil.")
			continue
		}

		tags := []*awsec2.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String(snapshotName),
			},
			{
				Key:   aws.String(smgr.deleteAfterTag),
				Value: aws.String(deleteAfter.Format(time.RFC3339)),
			},
		}

		for _, t := range volume.Tags {
			if t.Key != nil && *t.Key == "Name" {
				tags = append(tags, &awsec2.Tag{
					Key:   aws.String("volume-name"),
					Value: t.Value,
				})
				break
			}
		}

		if _, err := smgr.client.CreateTagsWithContext(
			ctx,
			&awsec2.CreateTagsInput{
				Resources: []*string{
					snapshot.SnapshotId,
				},
				Tags: tags,
			},
		); err != nil {
			logger.Error(err)
			continue
		}
		createTagsRequests.Inc()

		if err := smgr.datastore.StoreSnapshotInfo(&datastore.SnapshotInfo{
			Resource: datastore.SnapshotResource(*volume.VolumeId),
			ID:       datastore.SnapshotID(*snapshot.SnapshotId),
			// The createdAt timestamp is used as a key for ordering
			// in the datatstore. Hence we need to ensure it is
			// stable. To avoid problems let's truncate it to one
			// minute
			CreatedAt: (*snapshot.StartTime).Truncate(time.Minute),
		}); err != nil {
			logger.Error(err)
			continue
		}
	}
	return nil
}

// Prune deletes all matching EBS snapshots, i.e. snapshots with a delete after
// tag that is set to a date in the past
func (smgr *SnapshotManager) Prune(ctx context.Context) error {

	snaps, err := smgr.fetchSnapshots(ctx)
	if err != nil {
		return err
	}
	for _, snap := range snaps {
		smgr.logger.Infof("Processing snapshot %s", *snap.SnapshotId)
		for _, tag := range snap.Tags {
			if tag.Key == nil {
				continue
			}
			if *tag.Key == smgr.deleteAfterTag {
				// add context to the logger
				logger := smgr.logger.WithFields(log.Fields{
					"snapshotID": *snap.SnapshotId,
				})
				if tag.Value == nil {
					logger.Errorf("Delete after tag value is nil")
					continue
				}

				deleteAfter, err := time.Parse(time.RFC3339, *tag.Value)
				if err != nil {
					logger.Error("Couldn't parse tag value for : %+v", err)
					break
				}
				if time.Now().Before(deleteAfter) {
					logger.Info("Snapshot not yet scheduled for deletion")
					break
				}
				if _, err := smgr.client.DeleteSnapshotWithContext(ctx, &awsec2.DeleteSnapshotInput{
					SnapshotId: snap.SnapshotId,
				}); err != nil {
					logger.Error("Couldn't delete snapshot: %+v", err)
					break
				}
				deleteSnapshotRequests.Inc()
				logger.Info("Successfully deleted snapshot")
				if err := smgr.datastore.DeleteSnapshotInfo(&datastore.SnapshotInfo{
					Resource: datastore.SnapshotResource(*snap.VolumeId),
					ID:       datastore.SnapshotID(*snap.SnapshotId),
					// The createdAt timestamp is used as a key for ordering
					// in the datatstore. Hence we need to ensure it is
					// stable. To avoid problems it was truncated to one
					// minute during creation above
					CreatedAt: (*snap.StartTime).Truncate(time.Minute),
				}); err != nil {
					smgr.logger.Error(err)
				}
				break
			}
		}
	}

	return nil
}
