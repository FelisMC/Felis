package netguard

import "testing"

func TestRefusePrivate(t *testing.T) {
	for _, addr := range []string{
		"10.0.0.1:80", "172.16.0.1:443", "192.168.1.1:80", "127.0.0.1:80",
		"169.254.169.254:80", "0.0.0.0:80", "100.64.0.1:80", "0.1.2.3:80",
		"224.0.0.1:80", "198.18.0.1:80", "192.0.2.1:80", "example.com:80",
		"[::]:80", "[::1]:80", "[::2]:80", "[fec0::1]:80", "[fc00::1]:80", "[fe80::1%eth0]:80", "[ff02::1]:80",
		"[::ffff:127.0.0.1]:80", "[::ffff:10.1.2.3]:80", "[2002:a00:1::1]:80",
		"[64:ff9b::a00:1]:80", "[2001:db8::1]:80",
	} {
		if err := RefusePrivate("tcp", addr, nil); err == nil {
			t.Errorf("%s was allowed", addr)
		}
	}
	for _, addr := range []string{"1.1.1.1:443", "93.184.216.34:80", "[2606:4700:4700::1111]:443", "[::ffff:1.1.1.1]:443"} {
		if err := RefusePrivate("tcp", addr, nil); err != nil {
			t.Errorf("%s was refused: %v", addr, err)
		}
	}
}
