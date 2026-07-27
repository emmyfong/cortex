package parse

import (
	"net"
	"strings"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// Public addresses must be reachable, or the parser is useless.
		{"public v4", "93.184.216.34", false},
		{"public v6", "2606:2800:220:1:248:1893:25c8:1946", false},
		{"cloudflare dns", "1.1.1.1", false},

		// Loopback: reaching our own API or Postgres would be an SSRF pivot.
		{"loopback v4", "127.0.0.1", true},
		{"loopback v4 alternate", "127.99.42.7", true},
		{"loopback v6", "::1", true},

		// RFC1918 private ranges — the local network.
		{"private 10/8", "10.0.0.5", true},
		{"private 172.16/12", "172.16.31.9", true},
		{"private 192.168/16", "192.168.1.1", true},

		// Link-local. 169.254.169.254 is the cloud metadata endpoint and the
		// single most valuable SSRF target there is.
		{"link local", "169.254.1.1", true},
		{"cloud metadata", "169.254.169.254", true},
		{"link local v6", "fe80::1", true},

		// Other non-routable space.
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		{"multicast", "224.0.0.1", true},
		{"v6 unique local", "fc00::1", true},

		// A v4-mapped v6 address must be judged on its embedded v4 address,
		// otherwise ::ffff:127.0.0.1 walks straight past the loopback check.
		{"v4-mapped loopback", "::ffff:127.0.0.1", true},
		{"v4-mapped private", "::ffff:10.0.0.1", true},
		{"v4-mapped public", "::ffff:93.184.216.34", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("test setup: %q is not a valid IP", tt.ip)
			}

			if got := isBlockedIP(ip); got != tt.want {
				t.Errorf("isBlockedIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIsBlockedIPRejectsNil(t *testing.T) {
	// An unparseable address must fail closed, not open.
	if !isBlockedIP(nil) {
		t.Error("isBlockedIP(nil) = false, want true (fail closed)")
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"https is allowed", "https://example.com/article", ""},
		{"http is allowed", "http://example.com/article", ""},

		// Non-HTTP schemes can read local files or hit internal services.
		{"file scheme", "file:///etc/passwd", "scheme"},
		{"gopher scheme", "gopher://example.com", "scheme"},
		{"ftp scheme", "ftp://example.com/x", "scheme"},
		{"no scheme", "example.com/article", "scheme"},

		{"empty", "", "scheme"},
		{"missing host", "https://", "host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateURL(tt.raw)

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validateURL(%q) = %v, want nil", tt.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateURL(%q) = nil, want error mentioning %q", tt.raw, tt.wantErr)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tt.wantErr) {
				t.Errorf("validateURL(%q) error = %q, want it to mention %q",
					tt.raw, err.Error(), tt.wantErr)
			}
		})
	}
}

// The dial guard is the real defense: it inspects the IP actually being
// connected to, so it survives DNS rebinding and redirects to internal hosts,
// which up-front URL validation alone cannot catch.
func TestGuardAddressBlocksInternalTargets(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"public", "93.184.216.34:443", false},
		{"loopback", "127.0.0.1:8080", true},
		{"private", "10.1.2.3:80", true},
		{"cloud metadata", "169.254.169.254:80", true},
		{"loopback v6", "[::1]:6379", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := guardAddress("tcp4", tt.address, nil)

			if tt.wantErr && err == nil {
				t.Errorf("guardAddress(%q) = nil, want error", tt.address)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("guardAddress(%q) = %v, want nil", tt.address, err)
			}
		})
	}
}

func TestGuardAddressRejectsNonTCPNetworks(t *testing.T) {
	if err := guardAddress("udp", "93.184.216.34:53", nil); err == nil {
		t.Error("guardAddress allowed a udp dial, want error")
	}
}
