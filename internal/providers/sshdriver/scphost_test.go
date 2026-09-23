package sshdriver

import "testing"

func TestSCPHost(t *testing.T) {
	cases := []struct{ in, want string }{
		// IPv6 literals must be bracketed or scp splits on the first colon.
		{"ubuntu@2600:1f18:55f0:5e8a::c916", "ubuntu@[2600:1f18:55f0:5e8a::c916]"},
		{"2600:1f18::1", "[2600:1f18::1]"},
		// Already bracketed: leave alone, never double-bracket.
		{"ubuntu@[2600:1f18::1]", "ubuntu@[2600:1f18::1]"},
		// IPv4 and DNS names are unchanged.
		{"ubuntu@10.0.135.108", "ubuntu@10.0.135.108"},
		{"ubuntu@driver.example.com", "ubuntu@driver.example.com"},
		{"10.0.0.1", "10.0.0.1"},
	}
	for _, c := range cases {
		if got := scpHost(c.in); got != c.want {
			t.Errorf("scpHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
