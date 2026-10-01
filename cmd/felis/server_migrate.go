package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cmdServerMigrate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "server-migrate: start | status | retry (separate from database migrate)")
		return 2
	}
	fs := flag.NewFlagSet("server-migrate "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "stopped user server")
	target := fs.String("target-node", "", "approved target worker")
	id := fs.String("id", "", "operation id (required for retry)")
	kube := fs.String("kubeconfig", "/etc/rancher/k3s/k3s.yaml", "A's local kubeconfig")
	ns := fs.String("namespace", platform.DefaultMinecraftNamespace, "world namespace")
	cfgPath := fs.String("config", "/var/lib/felis/felis.host.toml", "host config used to record the current owner on the safety backup")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "server-migrate requires root/sudo")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := clientcmd.BuildConfigFromFlags("", *kube)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	v1alpha1.AddToScheme(scheme)
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m := &distributed.Manager{Client: cl, Namespace: *ns, Resolve: placement.Resolve(cl, *ns)}
	var nodes corev1.NodeList
	if err = cl.List(ctx, &nodes); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, n := range nodes.Items {
		if n.Labels[placement.LabelRole] == placement.RoleController {
			m.Controller = n.Name
		}
	}
	if m.Controller == "" {
		fmt.Fprintln(stderr, "distributed controller identity is not configured")
		return 1
	}
	m.Resolve = placement.Resolve(cl, *ns, m.Controller)
	var op distributed.Operation
	switch args[0] {
	case "start":
		host, cfgErr := config.Load(*cfgPath)
		if cfgErr != nil {
			fmt.Fprintln(stderr, cfgErr)
			return 1
		}
		drv, dbErr := openPodStore(ctx, host.Database.URL, "migration", stderr)
		if dbErr != nil {
			fmt.Fprintln(stderr, dbErr)
			return 1
		}
		defer drv.Close()
		var owner sql.NullString
		if err := drv.DB().QueryRowContext(ctx, "SELECT owner_id FROM servers WHERE name=$1 AND deleted_at IS NULL AND retire_requested_at IS NULL", *name).Scan(&owner); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		op, err = m.BeginMigration(ctx, *name, *target, owner.String)
	case "status":
		op, err = m.Migration(ctx, *name, *id)
	case "retry":
		if *id == "" {
			fmt.Fprintln(stderr, "retry requires --id")
			return 2
		}
		op, err = m.RetryMigration(ctx, *name, *id)
	default:
		fmt.Fprintln(stderr, "unknown migration operation")
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	json.NewEncoder(stdout).Encode(op)
	return 0
}
