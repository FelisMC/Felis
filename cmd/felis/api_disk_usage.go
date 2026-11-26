package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"felis.lolicon.best/internal/api"
)

const diskSampleInterval = time.Minute

type diskUsageCluster interface {
	ListServers(context.Context) ([]api.ServerInfo, error)
	DiskUsageTarget(context.Context, string) (api.DiskUsageTarget, error)
	PublishDiskUsage(context.Context, api.DiskUsageTarget, int64, time.Time) error
	RecordDiskUsageFailure(context.Context, api.DiskUsageTarget, time.Time) error
}

type diskUsageReader interface {
	DiskUsage(context.Context, string) (int64, error)
}

func sampleDiskUsage(ctx context.Context, cluster diskUsageCluster, files diskUsageReader, stderr io.Writer) {
	ticker := time.NewTicker(diskSampleInterval)
	defer ticker.Stop()
	for {
		if err := collectDiskUsage(ctx, cluster, files, stderr); err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "felis api: disk sampling: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// collectDiskUsage scans every server, stopped ones too, one volume at a time.
func collectDiskUsage(ctx context.Context, cluster diskUsageCluster, files diskUsageReader, stderr io.Writer) error {
	read, cancel := context.WithTimeout(ctx, 10*time.Second)
	servers, err := cluster.ListServers(read)
	cancel()
	if err != nil {
		return err
	}
	for _, server := range servers {
		if err := ctx.Err(); err != nil {
			return err
		}
		sample, cancel := context.WithTimeout(ctx, 75*time.Second)
		target, err := cluster.DiskUsageTarget(sample, server.Name)
		if err == nil {
			var used int64
			used, err = files.DiskUsage(sample, server.Name)
			if err == nil && sample.Err() == nil {
				err = cluster.PublishDiskUsage(sample, target, used, time.Now().UTC())
			} else if ctx.Err() == nil {
				record, recordCancel := context.WithTimeout(ctx, 10*time.Second)
				if rerr := cluster.RecordDiskUsageFailure(record, target, time.Now().UTC()); rerr != nil {
					fmt.Fprintf(stderr, "felis api: record disk sampling failure for %s: %v\n", server.Name, rerr)
				}
				recordCancel()
			}
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "felis api: disk sampling for %s: %v\n", server.Name, err)
		}
	}
	return ctx.Err()
}
