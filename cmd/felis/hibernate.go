package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"felis.lolicon.best/internal/hibernate"
)

// hibernateResultPath is the container's termination message, which the operator reads back from the Job's pod.
const hibernateResultPath = "/dev/termination-log"

type hibernateArgs struct {
	action, root, container string
	expect                  int64
}

// parseHibernate reads the action first, as the operator's node Job passes it, then its flags.
func parseHibernate(args []string, stderr io.Writer) (hibernateArgs, bool) {
	var a hibernateArgs
	if len(args) == 0 || (args[0] != "freeze" && args[0] != "thaw" && args[0] != "kill") {
		return a, false
	}
	a.action = args[0]
	fs := flag.NewFlagSet("hibernate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&a.root, "cgroup-root", "/host/cgroup", "the host's cgroup v2 hierarchy")
	fs.StringVar(&a.container, "container", "", "container ID of the game process")
	fs.Int64Var(&a.expect, "expect", 0, "bytes swapped out at the freeze, which a thaw waits to read back")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || a.container == "" {
		return a, false
	}
	return a, true
}

// cmdHibernate freezes, thaws or kills one container's processes from a node Job that sees the host's cgroups and processes.
func cmdHibernate(args []string, stdout, stderr io.Writer) int {
	a, ok := parseHibernate(args, stderr)
	if !ok {
		fmt.Fprintln(stderr, "usage: felis hibernate freeze|thaw|kill --container <id> [--expect <bytes>]")
		return 2
	}
	host := hibernate.Host{CgroupRoot: a.root, Meminfo: "/proc/meminfo", Prefetch: hibernate.PrefetchSwap}
	result, err := runHibernate(host, a.action, a.container, a.expect)
	if err != nil {
		var refusal *hibernate.Error
		code := hibernate.CodeFailed
		if errors.As(err, &refusal) {
			code = refusal.Code
		}
		result = hibernate.Result{Code: code, Message: err.Error()}
	}
	out, _ := json.Marshal(result)
	_ = os.WriteFile(hibernateResultPath, out, 0o644)
	fmt.Fprintln(stdout, string(out))
	if !result.OK {
		return 1
	}
	return 0
}

func runHibernate(host hibernate.Host, action, container string, expect int64) (hibernate.Result, error) {
	cg, err := host.Find(container)
	if err != nil {
		return hibernate.Result{}, err
	}
	switch action {
	case "freeze":
		return host.Freeze(cg)
	case "kill":
		return host.Kill(cg)
	}
	return host.Thaw(cg, expect)
}
