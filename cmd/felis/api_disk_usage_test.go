package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

type diskSampleFixture struct {
	servers          []api.ServerInfo
	reads, published []string
	cancel           context.CancelFunc
}

func (f *diskSampleFixture) ListServers(context.Context) ([]api.ServerInfo, error) {
	return f.servers, nil
}

func (f *diskSampleFixture) DiskUsageTarget(_ context.Context, name string) (api.DiskUsageTarget, error) {
	if name == "maintenance" {
		return api.DiskUsageTarget{}, errors.New("world is being restored")
	}
	return api.DiskUsageTarget{Name: name}, nil
}

func (f *diskSampleFixture) DiskUsage(_ context.Context, name string) (int64, error) {
	f.reads = append(f.reads, name)
	if f.cancel != nil {
		f.cancel()
	}
	if name == "incomplete" {
		return 999, errors.New("scan exceeded its time limit")
	}
	return 4096, nil
}

func (f *diskSampleFixture) PublishDiskUsage(_ context.Context, target api.DiskUsageTarget, used int64, at time.Time) error {
	if used != 4096 || at.IsZero() {
		return errors.New("invalid successful measurement")
	}
	f.published = append(f.published, target.Name)
	return nil
}

func TestDiskSamplingKeepsStoppedServersAndRejectsIncompleteScans(t *testing.T) {
	f := &diskSampleFixture{servers: []api.ServerInfo{
		{Name: "maintenance", Phase: "Stopped"},
		{Name: "incomplete", Phase: "Running"},
		{Name: "stopped", Phase: "Stopped"},
		{Name: "running", Phase: "Running"},
	}}
	if err := collectDiskUsage(context.Background(), f, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.reads, []string{"incomplete", "stopped", "running"}) {
		t.Fatalf("scanned servers = %v", f.reads)
	}
	if !slices.Equal(f.published, []string{"stopped", "running"}) {
		t.Fatalf("published measurements = %v; incomplete results must not replace the last successful sample", f.published)
	}
}

func TestDiskSamplingDoesNotPublishAfterLeadershipCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &diskSampleFixture{servers: []api.ServerInfo{{Name: "first"}, {Name: "next"}}, cancel: cancel}
	if err := collectDiskUsage(ctx, f, f, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("result = %v, want context cancellation", err)
	}
	if !slices.Equal(f.reads, []string{"first"}) || len(f.published) != 0 {
		t.Fatalf("reads = %v, published = %v; cancelled leader must stop without publishing", f.reads, f.published)
	}
}
