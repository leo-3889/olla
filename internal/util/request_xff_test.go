package util

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// TestGetClientIP_TrustedProxy_XFFUsesRightmostEntry pins the security fix:
// the rate-limit bucket key must use the RIGHTMOST X-Forwarded-For entry (the
// IP the trusted proxy observed its upstream connect from), NOT the leftmost
// (fully attacker-controlled).
//
// With trust_proxy_headers=true and RemoteAddr in a trusted CIDR, an attacker
// sends XFF: "fake, real". The rightmost entry ("real") is the IP the trusted
// proxy saw; the leftmost ("fake") is spoofed. Bucketing on the leftmost
// allows an attacker to bypass rate limits by prepending fake IPs.
func TestGetClientIP_TrustedProxy_XFFUsesRightmostEntry(t *testing.T) {
	trustedCIDRs := []*net.IPNet{mustCIDR("10.0.0.0/8")}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345" // in trusted CIDR
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	got := GetClientIP(req, true, trustedCIDRs)
	// Rightmost = "5.6.7.8" (the trusted proxy's observed IP).
	// Leftmost = "1.2.3.4" (attacker-controlled).
	if got != "5.6.7.8" {
		t.Fatalf("GetClientIP with trusted proxy returned %q, want %q (rightmost XFF entry). "+
			"Using the leftmost entry allows rate-limit bypass via spoofed XFF.", got, "5.6.7.8")
	}
}

// TestGetClientIP_TrustedProxy_SingleXFFEntry verifies a single-entry XFF
// (the common single-proxy case) still works — rightmost == leftmost.
func TestGetClientIP_TrustedProxy_SingleXFFEntry(t *testing.T) {
	trustedCIDRs := []*net.IPNet{mustCIDR("10.0.0.0/8")}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")

	got := GetClientIP(req, true, trustedCIDRs)
	if got != "203.0.113.50" {
		t.Fatalf("GetClientIP = %q, want %q", got, "203.0.113.50")
	}
}

// TestGetClientIP_UntrustedRemoteAddr_FallsBackToRemoteAddr verifies that
// when RemoteAddr is NOT in a trusted CIDR, the XFF header is ignored entirely
// (prevents untrusted clients from spoofing their IP).
func TestGetClientIP_UntrustedRemoteAddr_FallsBackToRemoteAddr(t *testing.T) {
	trustedCIDRs := []*net.IPNet{mustCIDR("10.0.0.0/8")}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1:9999" // NOT in trusted CIDR
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	got := GetClientIP(req, true, trustedCIDRs)
	if got != "192.168.1.1" {
		t.Fatalf("GetClientIP = %q, want %q (RemoteAddr fallback for untrusted source)", got, "192.168.1.1")
	}
}

// TestGetClientIP_TrustProxyFalse_UsesRemoteAddr verifies that when
// trust_proxy_headers=false, the behaviour is unchanged: RemoteAddr only.
func TestGetClientIP_TrustProxyFalse_UsesRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1:9999"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	got := GetClientIP(req, false, nil)
	if got != "192.168.1.1" {
		t.Fatalf("GetClientIP = %q, want %q (RemoteAddr when trust_proxy_headers=false)", got, "192.168.1.1")
	}
}
