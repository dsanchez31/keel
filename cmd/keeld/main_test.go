package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/transport"
	"github.com/dsanchez31/keel/vector"
)

// wait bounds every wait on the network.
const wait = 10 * time.Second

var (
	referenceWorld = filepath.Join("..", "..", "examples", "worlds", "reference.yaml")
	shippedPacks   = filepath.Join("..", "..", "doctrine-packs")
)

// syncBuffer is a bytes.Buffer safe to read while the command writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// writeConfig serves the reference fleet on native vehicles and writes a
// configuration binding keeld to them, on a free loopback port.
func writeConfig(t *testing.T) string {
	t.Helper()
	w, err := files.ReadWorld(referenceWorld)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := native.NewServer(native.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		_ = srv.Close()
		ts.Close()
	})
	worldPath, err := filepath.Abs(referenceWorld)
	if err != nil {
		t.Fatal(err)
	}
	packsPath, err := filepath.Abs(shippedPacks)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: keel.daemon/v1\nname: test\nworld: %q\ndoctrine_dir: %q\ndata_dir: data\nlisten: 127.0.0.1:0\nvectors:\n", worldPath, packsPath)
	for _, fv := range w.Fleet {
		if _, err := srv.Add(fv.Caps, vector.CommandTypes()); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "  - id: %s\n    native: {url: %q}\n", fv.Caps.ID, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/vectors/"+string(fv.Caps.ID))
	}
	path := filepath.Join(t.TempDir(), "keeld.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServesUntilInterrupted(t *testing.T) {
	cfg := writeConfig(t)
	var logs syncBuffer
	cmd := newRootCmd(io.Discard, &logs)
	cmd.SetArgs([]string{"--config", cfg})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	api := regexp.MustCompile(`api=(http://\S+/api/v1)`)
	deadline := time.Now().Add(wait)
	for !api.MatchString(logs.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("no serving line in %q", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	base := api.FindStringSubmatch(logs.String())[1]

	resp, err := http.Get(base + "/doctrine")
	if err != nil {
		t.Fatal(err)
	}
	var packs []transport.DoctrineInfo
	err = json.NewDecoder(resp.Body).Decode(&packs)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || len(packs) == 0 {
		t.Fatalf("GET /doctrine: status %d, %d packs, err %v", resp.StatusCode, len(packs), err)
	}
	resp, err = http.Get(base + "/missions/MSN-001")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET an unknown mission: status %d, want 404", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interrupted keeld returned %v, want nil", err)
		}
	case <-time.After(wait):
		t.Fatal("keeld did not stop")
	}
	if !strings.Contains(logs.String(), "keeld stopped") {
		t.Errorf("no stop line in %q", logs.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "data", "missions")); err != nil {
		t.Errorf("the data directory was not prepared: %v", err)
	}
}

func TestRefusesABadConfiguration(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--config", filepath.Join(t.TempDir(), "none.yaml")}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "reading the configuration") {
		t.Fatalf("stderr %q does not say what failed", stderr.String())
	}
}
