package services

import (
	"context"
	"net"
	"testing"
)

// TestBindListener_FreePort verifies that bindListener returns a valid listener
// when no other process holds the port.
func TestBindListener_FreePort(t *testing.T) {
	t.Parallel()

	ln, err := bindListener(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("expected bind to succeed on a free port: %v", err)
	}
	ln.Close()
}

// TestBindListener_OccupiedPort verifies that bindListener returns a non-nil
// error when another listener already holds the port. This is the path that
// Start() uses to surface port conflicts immediately rather than swallowing
// them inside a goroutine.
func TestBindListener_OccupiedPort(t *testing.T) {
	t.Parallel()

	// Hold a port so the second bind must fail.
	anchor, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to acquire anchor listener: %v", err)
	}
	defer anchor.Close()

	addr := anchor.Addr().String()

	_, bindErr := bindListener(context.Background(), addr)
	if bindErr == nil {
		t.Fatal("expected bindListener to return an error while anchor holds the port, but it succeeded")
	}
}
