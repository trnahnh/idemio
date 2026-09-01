package faketest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const startTimeout = 20 * time.Second

var (
	buildOnce  sync.Once
	binaryPath string
	buildErr   error
)

type Fake struct {
	DataURL    string
	ControlURL string
	LedgerPath string
}

type Execution struct {
	Sequence      int    `json:"sequence"`
	CorrelationID string `json:"correlation_id"`
	ResourceID    string `json:"resource_id"`
	Behavior      string `json:"behavior"`
}

func Start(t *testing.T) *Fake {
	t.Helper()

	binary := build(t)
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")

	cmd := exec.Command(binary, "-ledger", ledger)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	addrs := readAddresses(t, stdout)
	return &Fake{
		DataURL:    "http://" + addrs["data"],
		ControlURL: "http://" + addrs["control"],
		LedgerPath: ledger,
	}
}

func (f *Fake) Script(t *testing.T, resourceID string, behaviors ...string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{"resource_id": resourceID, "behaviors": behaviors})
	if err != nil {
		t.Fatalf("encode script: %v", err)
	}
	f.post(t, f.ControlURL+"/control/script", body)
}

func (f *Fake) SetListener(t *testing.T, up bool) {
	t.Helper()

	body, err := json.Marshal(map[string]any{"up": up})
	if err != nil {
		t.Fatalf("encode listener: %v", err)
	}
	f.post(t, f.ControlURL+"/control/listener", body)
}

// Executions is the only sanctioned way to assert what the downstream actually did.
func (f *Fake) Executions(t *testing.T, correlationID string) []Execution {
	t.Helper()

	url := f.ControlURL + "/probe"
	if correlationID != "" {
		url += "?correlation_id=" + correlationID
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()

	var decoded struct {
		Executions []Execution `json:"executions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode probe: %v", err)
	}
	return decoded.Executions
}

func (f *Fake) LedgerLines(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(f.LedgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func (f *Fake) post(t *testing.T, url string, body []byte) {
	t.Helper()

	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("post %s: %s: %s", url, resp.Status, payload)
	}
}

func readAddresses(t *testing.T, stdout io.Reader) map[string]string {
	t.Helper()

	type result struct {
		addrs map[string]string
		err   error
	}
	done := make(chan result, 1)

	go func() {
		addrs := make(map[string]string)
		scanner := bufio.NewScanner(stdout)
		for len(addrs) < 2 && scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), "=")
			if !ok {
				continue
			}
			addrs[key] = value
		}
		if len(addrs) < 2 {
			done <- result{err: fmt.Errorf("fake reported %d addresses, want 2", len(addrs))}
			return
		}
		done <- result{addrs: addrs}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("read fake addresses: %v", r.err)
		}
		return r.addrs
	case <-time.After(startTimeout):
		t.Fatalf("fake did not report addresses within %s", startTimeout)
		return nil
	}
}

func build(t *testing.T) string {
	t.Helper()

	buildOnce.Do(func() {
		out := filepath.Join(os.TempDir(), "idemio-fakedownstream")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", out, "github.com/trnahnh/idemio/cmd/fakedownstream")
		if output, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build fake: %w: %s", err, output)
			return
		}
		binaryPath = out
	})
	if buildErr != nil {
		t.Fatalf("%v", buildErr)
	}
	return binaryPath
}
