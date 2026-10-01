package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"time"
)

// A trusted probe runs with the same server labels and security context before node admission.
func cmdNodeProbe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "listen and print observed source addresses (admission only)")
	var open, closed multiFlag
	fs.Var(&open, "open", "required reachable host:port")
	fs.Var(&closed, "closed", "required blocked host:port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *listen != "" {
		l, err := net.Listen("tcp", *listen)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer l.Close()
		for {
			c, err := l.Accept()
			if err != nil {
				return 1
			}
			fmt.Fprintln(stdout, "felis-probe-source", c.RemoteAddr().String())
			c.Write([]byte("felis-probe\n"))
			c.Close()
		}
	}
	time.Sleep(3 * time.Second)
	for _, check := range []struct {
		addresses []string
		wantOpen  bool
	}{{open, true}, {closed, false}} {
		for _, addr := range check.addresses {
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if c != nil {
				c.Close()
			}
			if (err == nil) != check.wantOpen {
				fmt.Fprintf(stderr, "node-probe: %s open=%t, expected %t\n", addr, err == nil, check.wantOpen)
				return 1
			}
		}
	}
	return 0
}
