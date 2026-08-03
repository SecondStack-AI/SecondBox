// Package relayretention instruments the durable relay during a qualified live
// scenario. It deliberately talks to PostgreSQL as an operator measurement
// harness; production data-plane code never receives database paths or keys.
package relayretention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	InteractivePTYInputBytes  = 512
	InteractivePTYOutputBytes = 4096
	LargeFileBytes            = 1 << 20
	RelayPortFrameBytes       = 16 << 10
	RelayPortTotalBytes       = 1 << 20
	MeasurementCycles         = 3
)

type FrameStat struct {
	Kind         string `json:"kind"`
	Direction    string `json:"direction"`
	Rows         int64  `json:"rows"`
	PayloadBytes int64  `json:"payloadBytes"`
}

type RelationSnapshot struct {
	HeapBytes  int64            `json:"heapBytes"`
	IndexBytes map[string]int64 `json:"indexBytes"`
}

type Cycle struct {
	Number           int              `json:"number"`
	SessionIDs       []string         `json:"sessionIds"`
	FramesAfterWork  []FrameStat      `json:"framesAfterWorkload"`
	FramesAfterSweep []FrameStat      `json:"framesAfterSweep"`
	Before           RelationSnapshot `json:"beforeWorkload"`
	AfterWork        RelationSnapshot `json:"afterWorkload"`
	AfterVacuum      RelationSnapshot `json:"afterVacuumFull"`
}

type NotificationCounts struct {
	InboundForMeasuredSessions int64 `json:"inboundDataPlaneSession"`
	OutboundForRunner          int64 `json:"outboundRunnerCommand"`
}

type Report struct {
	SchemaVersion          int       `json:"schemaVersion"`
	StartedAt              time.Time `json:"startedAt"`
	CompletedAt            time.Time `json:"completedAt"`
	Postgres               string    `json:"postgresVersion"`
	SeparateFrameRetention bool      `json:"separateFrameRetention"`
	Parameters             struct {
		PTYInputBytes  int `json:"ptyInputBytes"`
		PTYOutputBytes int `json:"ptyOutputBytes"`
		FileBytes      int `json:"fileBytes"`
		PortFrameBytes int `json:"portFrameBytes"`
		PortTotalBytes int `json:"portTotalBytes"`
		Cycles         int `json:"cycles"`
	} `json:"parameters"`
	Cycles        []Cycle            `json:"cycles"`
	Notifications NotificationCounts `json:"notifications"`
}

type notification struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

type Collector struct {
	pool                   *pgxpool.Pool
	databaseURL            string
	runnerID               string
	separateFrameRetention bool
	mu                     sync.Mutex
	notices                []notification
	listenStop             context.CancelFunc
	listenDone             chan error
}

func Open(ctx context.Context, databaseURL string, runnerID string) (*Collector, error) {
	if databaseURL == "" || runnerID == "" {
		return nil, errors.New("SecondBox relay-retention collector requires database URL and Runner ID")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox relay-retention PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox relay-retention PostgreSQL readiness: %w", err)
	}
	var separateFrameRetention bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_attribute
			WHERE attrelid='secondbox.data_plane_sessions'::regclass
			  AND attname='frame_cleanup_completed_at'
			  AND NOT attisdropped
		)`).Scan(&separateFrameRetention); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox relay-retention schema inspection: %w", err)
	}
	return &Collector{pool: pool, databaseURL: databaseURL, runnerID: runnerID, separateFrameRetention: separateFrameRetention}, nil
}

func (collector *Collector) SeparateFrameRetention() bool {
	return collector.separateFrameRetention
}

func (collector *Collector) Close() error {
	err := collector.StopNotifications()
	collector.pool.Close()
	return err
}

func (collector *Collector) StartNotifications(ctx context.Context) error {
	if collector.listenStop != nil {
		return errors.New("SecondBox relay-retention listener is already active")
	}
	connection, err := pgx.Connect(ctx, collector.databaseURL)
	if err != nil {
		return fmt.Errorf("SecondBox relay-retention listener connection: %w", err)
	}
	if _, err := connection.Exec(ctx, "LISTEN secondbox_work"); err != nil {
		_ = connection.Close(context.Background())
		return fmt.Errorf("SecondBox relay-retention LISTEN: %w", err)
	}
	listenContext, cancel := context.WithCancel(context.Background())
	collector.listenStop = cancel
	collector.listenDone = make(chan error, 1)
	go func() {
		defer connection.Close(context.Background())
		for {
			notice, err := connection.WaitForNotification(listenContext)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					collector.listenDone <- nil
				} else {
					collector.listenDone <- fmt.Errorf("SecondBox relay-retention notification wait: %w", err)
				}
				return
			}
			var decoded notification
			if err := json.Unmarshal([]byte(notice.Payload), &decoded); err != nil {
				collector.listenDone <- fmt.Errorf("SecondBox relay-retention notification decode: %w", err)
				return
			}
			collector.mu.Lock()
			collector.notices = append(collector.notices, decoded)
			collector.mu.Unlock()
		}
	}()
	return nil
}

func (collector *Collector) StopNotifications() error {
	if collector.listenStop == nil {
		return nil
	}
	collector.listenStop()
	err := <-collector.listenDone
	collector.listenStop = nil
	collector.listenDone = nil
	return err
}

func (collector *Collector) PostgresVersion(ctx context.Context) (string, error) {
	var version string
	err := collector.pool.QueryRow(ctx, "SELECT version()").Scan(&version)
	return version, err
}

func (collector *Collector) RelationSnapshot(ctx context.Context) (RelationSnapshot, error) {
	result := RelationSnapshot{IndexBytes: make(map[string]int64)}
	if err := collector.pool.QueryRow(ctx, `
		SELECT pg_relation_size('secondbox.data_plane_frames')`).Scan(&result.HeapBytes); err != nil {
		return RelationSnapshot{}, fmt.Errorf("SecondBox relay-retention heap size: %w", err)
	}
	rows, err := collector.pool.Query(ctx, `
		SELECT indexrelid::regclass::text,pg_relation_size(indexrelid)
		FROM pg_index
		WHERE indrelid='secondbox.data_plane_frames'::regclass
		ORDER BY indexrelid::regclass::text`)
	if err != nil {
		return RelationSnapshot{}, fmt.Errorf("SecondBox relay-retention index sizes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			return RelationSnapshot{}, fmt.Errorf("SecondBox relay-retention index scan: %w", err)
		}
		result.IndexBytes[name] = size
	}
	return result, rows.Err()
}

func (collector *Collector) SessionIDs(
	ctx context.Context,
	sandboxID string,
	createdAfter time.Time,
) ([]string, error) {
	rows, err := collector.pool.Query(ctx, `
		SELECT id FROM secondbox.data_plane_sessions
		WHERE sandbox_id=$1 AND created_at>=$2
		  AND (kind IN ('terminal','file','port'))
		ORDER BY created_at,id`, sandboxID, createdAfter.UTC())
	if err != nil {
		return nil, fmt.Errorf("SecondBox relay-retention measured session lookup: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("SecondBox relay-retention measured session scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (collector *Collector) FrameStats(ctx context.Context, ids []string) ([]FrameStat, error) {
	rows, err := collector.pool.Query(ctx, `
		SELECT session.kind,frame.direction,count(*),COALESCE(sum(frame.payload_bytes),0)
		FROM secondbox.data_plane_frames AS frame
		JOIN secondbox.data_plane_sessions AS session ON session.id=frame.session_id
		WHERE frame.session_id=ANY($1)
		GROUP BY session.kind,frame.direction
		ORDER BY session.kind,frame.direction`, ids)
	if err != nil {
		return nil, fmt.Errorf("SecondBox relay-retention frame statistics: %w", err)
	}
	defer rows.Close()
	stats := make([]FrameStat, 0)
	for rows.Next() {
		var stat FrameStat
		if err := rows.Scan(&stat.Kind, &stat.Direction, &stat.Rows, &stat.PayloadBytes); err != nil {
			return nil, fmt.Errorf("SecondBox relay-retention frame statistics scan: %w", err)
		}
		stats = append(stats, stat)
	}
	return stats, rows.Err()
}

func (collector *Collector) WaitForFrameCleanup(
	ctx context.Context,
	ids []string,
) error {
	if !collector.separateFrameRetention {
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var remaining int64
		if err := collector.pool.QueryRow(ctx, `
			SELECT count(*) FROM secondbox.data_plane_sessions AS session
			WHERE session.id=ANY($1)
			  AND session.frame_cleanup_completed_at IS NULL`, ids).Scan(&remaining); err != nil {
			return fmt.Errorf("SecondBox relay-retention cleanup observation: %w", err)
		}
		if remaining == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("SecondBox relay-retention frame cleanup remained incomplete: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (collector *Collector) VacuumFull(ctx context.Context) error {
	_, err := collector.pool.Exec(ctx, "VACUUM (FULL, ANALYZE) secondbox.data_plane_frames")
	if err != nil {
		return fmt.Errorf("SecondBox relay-retention VACUUM FULL: %w", err)
	}
	return nil
}

func (collector *Collector) Counts(sessionIDs []string) NotificationCounts {
	measured := make(map[string]struct{}, len(sessionIDs))
	for _, id := range sessionIDs {
		measured[id] = struct{}{}
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	var counts NotificationCounts
	for _, notice := range collector.notices {
		if notice.Kind == "data_plane_session" {
			if _, ok := measured[notice.Key]; ok {
				counts.InboundForMeasuredSessions++
			}
		}
		if notice.Kind == "runner_command" && notice.Key == collector.runnerID {
			counts.OutboundForRunner++
		}
	}
	return counts
}

func NewReport(startedAt time.Time, postgresVersion string, separateFrameRetention bool) Report {
	report := Report{SchemaVersion: 1, StartedAt: startedAt.UTC(), Postgres: postgresVersion, SeparateFrameRetention: separateFrameRetention}
	report.Parameters.PTYInputBytes = InteractivePTYInputBytes
	report.Parameters.PTYOutputBytes = InteractivePTYOutputBytes
	report.Parameters.FileBytes = LargeFileBytes
	report.Parameters.PortFrameBytes = RelayPortFrameBytes
	report.Parameters.PortTotalBytes = RelayPortTotalBytes
	report.Parameters.Cycles = MeasurementCycles
	return report
}

func WriteReport(path string, report Report) error {
	if !filepath.IsAbs(path) {
		return errors.New("SecondBox relay-retention output path must be absolute")
	}
	report.CompletedAt = time.Now().UTC()
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("SecondBox relay-retention report encoding: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("SecondBox relay-retention output directory: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("SecondBox relay-retention report write: %w", err)
	}
	return nil
}
