package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"felis.lolicon.best/internal/api"
)

func sampleUsage(ctx context.Context, a *api.API, stderr io.Writer) {
	ticker := time.NewTicker(api.UsageInterval)
	defer ticker.Stop()
	for {
		sample, cancel := context.WithTimeout(ctx, 10*time.Second)
		servers, err := a.Cluster.ListServers(sample)
		if err == nil {
			err = a.Usage.Collect(sample, servers, time.Now())
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "felis api: resource sampling: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
