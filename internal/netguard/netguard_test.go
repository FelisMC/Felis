package netguard

import "testing"

// TestRefuseLinkLocal pins that the metadata address and its kin are refused while
// ordinary public, private and loopback addresses pass.
func TestRefuseLinkLocal(t *testing.T) {
	for _, addr := range []string{"169.254.169.254:80", "[fe80::1]:443", "0.0.0.0:80", "[::]:80", "224.0.0.1:80", "[ff02::1]:25"} {
		if err := RefuseLinkLocal("tcp", addr, nil); err == nil {
			t.Errorf("%s was allowed", addr)
		}
	}
	for _, addr := range []string{"203.0.113.7:443", "127.0.0.1:8080", "10.0.0.5:587", "[2001:db8::1]:465"} {
		if err := RefuseLinkLocal("tcp", addr, nil); err != nil {
			t.Errorf("%s was refused: %v", addr, err)
		}
	}
}
