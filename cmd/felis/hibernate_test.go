package main

import (
	"io"
	"testing"
)

func TestHibernateTakesTheActionBeforeItsFlags(t *testing.T) {
	a, ok := parseHibernate([]string{"thaw", "--container", "c0ffee", "--expect", "966367641"}, io.Discard)
	if !ok || a.action != "thaw" || a.container != "c0ffee" || a.expect != 966367641 || a.root != "/host/cgroup" {
		t.Fatalf("parse = %+v, %v", a, ok)
	}
	for _, args := range [][]string{{"--container", "c0ffee", "freeze"}, {"freeze"}, {"pause", "--container", "c0ffee"}} {
		if _, ok := parseHibernate(args, io.Discard); ok {
			t.Fatalf("%q parsed", args)
		}
	}
}
