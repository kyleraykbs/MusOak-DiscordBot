// Package musoakdtest runs a real musoakd for the tests that need one: the
// link contract test and the bot's room tests. It exists so there is one
// harness rather than one per package; nothing in the bot imports it.
package musoakdtest

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

// StartServer runs a real musoakd against a throwaway config and returns its
// base URL. The binary comes from $MUSOAKD or the PATH — the devShell and the
// flake check both put it there — and without it there is no contract worth
// testing, so the test steps aside rather than faking the server it is here to
// pin.
func StartServer(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("MUSOAKD")
	if bin == "" {
		looked, err := exec.LookPath("musoakd")
		if err != nil {
			t.Skip("musoakd not found: set MUSOAKD or run inside the devShell")
		}
		bin = looked
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := fmt.Sprintf("{\"listen\":%q,\"storageDir\":%q}\n", addr, filepath.Join(dir, "storage"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--config", configPath)
	var log bytes.Buffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start musoakd: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("musoakd output:\n%s", log.String())
		}
	})

	base := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("musoakd never became healthy: %v\n%s", err, log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// WriteWAV renders a short sine as a 44.1kHz mono PCM file the server can
// import, so a test has real tracks without touching a provider. A name
// starting with "Beta" is a different tone, which is how a test tells two
// imported songs apart by ear.
func WriteWAV(t *testing.T, name string, seconds float64) string {
	t.Helper()
	const sampleRate = 44100
	samples := int(seconds * sampleRate)
	dataBytes := samples * 2
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	freq := 440.0
	if strings.HasPrefix(name, "Beta") {
		freq = 330.0
	}
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+dataBytes))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:], 1) // mono
	binary.LittleEndian.PutUint32(header[24:], sampleRate)
	binary.LittleEndian.PutUint32(header[28:], sampleRate*2)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(dataBytes))
	if _, err := f.Write(header); err != nil {
		t.Fatal(err)
	}
	samplesBuf := make([]byte, dataBytes)
	for i := range samples {
		value := 0.4 * math.Sin(2*math.Pi*freq*float64(i)/sampleRate)
		binary.LittleEndian.PutUint16(samplesBuf[i*2:], uint16(int16(value*math.MaxInt16)))
	}
	if _, err := f.Write(samplesBuf); err != nil {
		t.Fatal(err)
	}
	return path
}

// Register makes an account on the server and returns a client holding its
// token.
func Register(t *testing.T, base, name string) *client.Client {
	t.Helper()
	auth, err := client.New(base).Register(context.Background(), name, "secret")
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return client.New(base, client.WithToken(auth.Token))
}

// Counter counts the requests that reached a server, by method and path.
type Counter struct {
	mu     sync.Mutex
	counts map[string]int
}

// Count is how many times one request arrived. The key is
// "METHOD /path?query", the way it was asked for.
func (c *Counter) Count(request string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[request]
}

// Total is every request that arrived, whatever it was.
func (c *Counter) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, n := range c.counts {
		total += n
	}
	return total
}

// Recorder puts a counting proxy in front of a server: the URL to use, and the
// counts. It is how a test says what a call costs in round trips without
// faking the server those round trips go to.
func Recorder(t *testing.T, base string) (string, *Counter) {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	counter := &Counter{counts: map[string]int{}}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.mu.Lock()
		counter.counts[r.Method+" "+r.URL.RequestURI()]++
		counter.mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	return front.URL, counter
}

// Import makes one real track on an account and returns its id.
func Import(t *testing.T, c *client.Client, name string, seconds float64) string {
	t.Helper()
	result, err := c.ImportPath(context.Background(), WriteWAV(t, name, seconds))
	if err != nil || len(result.Imported) == 0 {
		t.Fatalf("import %s: %v %+v", name, err, result)
	}
	return result.Imported[0].Track.ID
}
