package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/placement"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func distributionManager(cl client.Client, cfg *config.Config, image string) (*distributed.Manager, error) {
	if os.Getenv("FELIS_DISTRIBUTED") != "true" {
		return nil, nil
	}
	controller, url, key := os.Getenv("FELIS_CONTROLLER_NODE"), os.Getenv("FELIS_ARCHIVE_URL"), os.Getenv(archivetransfer.KeyEnv)
	if controller == "" || url == "" || len(key) < 32 || image == "" || cfg.Archive.Store != "tarLocal" {
		return nil, fmt.Errorf("distributed mode requires controller identity, archive service/key, Felis image and tarLocal")
	}
	return &distributed.Manager{Client: cl, Namespace: cfg.K8s.Namespace, Image: image, Controller: controller, Archive: archivetransfer.Client{URL: url, Root: cfg.Archive.LocalPath, Key: key}, Resolve: placement.Resolve(cl, cfg.K8s.Namespace, controller)}, nil
}

func reconcileDistribution(ctx context.Context, m *distributed.Manager, stderr io.Writer) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if err := m.SettleBackups(ctx); err != nil {
			fmt.Fprintln(stderr, "distributed backup:", err)
		}
		if err := m.ReconcileMigrations(ctx); err != nil {
			fmt.Fprintln(stderr, "migration:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
