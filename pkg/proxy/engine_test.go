package proxy

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestProxyBidirectionalIdleTimeout(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	defer clientA.Close()
	defer clientB.Close()
	defer backendA.Close()
	defer backendB.Close()

	done := make(chan struct{})
	go func() {
		proxyBidirectionalWithTimeout(clientB, backendA, 50*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("proxyBidirectionalWithTimeout did not return after idle timeout")
	}
}

func TestProxyBidirectionalActiveTransferExceedsIdleTimeout(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	defer clientA.Close()
	defer clientB.Close()
	defer backendA.Close()
	defer backendB.Close()

	proxyDone := make(chan struct{})
	go func() {
		proxyBidirectionalWithTimeout(clientB, backendA, 60*time.Millisecond)
		close(proxyDone)
	}()

	// Send chunks every 20ms for 120ms (total > 60ms idle timeout),
	// verifying that active traffic refreshes deadlines.
	want := []byte("chunk-1|chunk-2|chunk-3|chunk-4|chunk-5|chunk-6|")
	go func() {
		chunk := []byte("chunk-1|")
		for i := 1; i <= 6; i++ {
			chunk[6] = byte('0' + i)
			if _, err := clientA.Write(chunk); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = clientA.Close()
		_ = backendB.Close()
	}()

	got, err := io.ReadAll(backendB)
	if err != nil && !bytes.Equal(got, want) {
		t.Fatalf("ReadAll: %v (got %q, want %q)", err, got, want)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("proxied bytes = %q, want %q", got, want)
	}

	select {
	case <-proxyDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("proxyBidirectionalWithTimeout did not finish after close")
	}
}
