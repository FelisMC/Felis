package api

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Sizes reach the quota sums in whole MiB, rounded up: a part-MiB used to be
// dropped, so a server sized in decimal units counted for less than it holds.
func TestQuantityToMB(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"4Gi", 4096},
		{"1Mi", 1},
		{"1G", 954},     // 953.67 MiB
		{"1500M", 1431}, // 1430.51 MiB
		{"1048577", 2},  // one byte past 1 MiB
		{"1", 1},
		{"0", 0},
	} {
		if got := quantityToMB(resource.MustParse(c.in)); got != c.want {
			t.Errorf("quantityToMB(%s) = %d, want %d", c.in, got, c.want)
		}
	}
	if got := quantityToMB(resource.Quantity{}); got != 0 {
		t.Errorf("quantityToMB(unset) = %d, want 0", got)
	}
}
