package main

import "testing"

func TestFirewallPortAllowed(t *testing.T) {
	status := []byte(`Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
80/tcp                     ALLOW       Anywhere
443/tcp                    ALLOW IN    Anywhere
80/tcp (v6)                ALLOW       Anywhere (v6)
443/tcp (v6)               ALLOW IN    Anywhere (v6)
`)

	for _, port := range []string{"80", "443"} {
		if !firewallPortAllowed(status, port) {
			t.Fatalf("expected %s/tcp to be recognized as allowed", port)
		}
	}
	if firewallPortAllowed(status, "2053") {
		t.Fatal("unexpectedly recognized an absent port as allowed")
	}
}
