package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/planner"
)

const intent = "Grid-search the unexplored area to lift the fog of war"

// Shipped artifacts, relative to this package.
var shipped = []string{"--world", "../../examples/worlds/reference.yaml", "--doctrine-dir", "../../doctrine-packs"}

func keelctl(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return keelctlIn(t, strings.NewReader(""), args...)
}

// keelctlIn runs keelctl with stdin as its standard input.
func keelctlIn(t *testing.T, stdin io.Reader, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, stdin, &out, &errOut)
	return code, out.String(), errOut.String()
}

func compileArgs(extra ...string) []string {
	args := append([]string{"plan", "compile"}, shipped...)
	return append(append(args, extra...), intent)
}

func TestDryRunCallsNoBackend(t *testing.T) {
	// An unreachable Ollama proves nothing is dialled.
	code, out, errOut := keelctl(t, compileArgs("--dry-run", "--ollama-url", "http://127.0.0.1:1")...)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"# triage system\n", "# triage schema\n", `"mission"`, "# system\n", "fog_of_war_east", "# user\n" + intent, "# schema\n", `"keel.plan/v1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q", want)
		}
	}
}

// triageMission is the triage reply finding a mission in the intent.
const triageMission = `{"reason":"A grid search of the unexplored area.","mission":true}`

// fakeOllama answers the triage request with triage and every plan request
// with reply.
func fakeOllama(t *testing.T, triage, reply string) string {
	t.Helper()
	answer := func(content string) []byte {
		body, err := json.Marshal(map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "done": true, "done_reason": "stop"})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	triaged, planned := answer(triage), answer(reply)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Format json.RawMessage `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if planner.IsTriage(planner.Conversation{Schema: req.Format}) {
			_, _ = w.Write(triaged)
			return
		}
		_, _ = w.Write(planned)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCompileExitCodes(t *testing.T) {
	valid := `{"apiVersion":"keel.plan/v1","intent":"` + intent + `","doctrine":"recon-standard@2.1.0",` +
		`"mission":{"type":"systematic_reconnaissance","priority":"normal","area":"fog_of_war_east"},` +
		`"tactic":{"pattern":"parallel_lanes","orientation":"long_axis","lanes":4},` +
		`"assignment":{"policy":"nearest_capable","requires":["aerial","camera"],"gcs":"gcs-west"},` +
		`"rationale":"Four drones, four lanes."}`

	t.Run("plan produced", func(t *testing.T) {
		code, out, errOut := keelctl(t, compileArgs("--ollama-url", fakeOllama(t, triageMission, valid))...)
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		var o planner.Outcome
		if err := json.Unmarshal([]byte(out), &o); err != nil {
			t.Fatal(err)
		}
		if !o.OK() || o.Plan.Hash == "" || len(o.Attempts) != 1 {
			t.Fatalf("outcome: ok %v, %d attempts", o.OK(), len(o.Attempts))
		}
	})

	t.Run("every attempt refused", func(t *testing.T) {
		code, out, _ := keelctl(t, compileArgs("--ollama-url", fakeOllama(t, triageMission, `{"area":"somewhere"}`))...)
		if code != exitNoPlan {
			t.Fatalf("exit %d, want %d", code, exitNoPlan)
		}
		var o planner.Outcome
		if err := json.Unmarshal([]byte(out), &o); err != nil {
			t.Fatal(err)
		}
		if o.OK() || len(o.Attempts) != planner.MaxAttempts {
			t.Fatalf("outcome: ok %v, %d attempts", o.OK(), len(o.Attempts))
		}
	})

	t.Run("intent declined", func(t *testing.T) {
		url := fakeOllama(t, `{"reason":"A request for the fleet's status.","mission":false}`, valid)
		code, out, errOut := keelctl(t, compileArgs("--ollama-url", url)...)
		if code != exitNoPlan || !strings.Contains(errOut, "asks for no mission") {
			t.Fatalf("exit %d, want %d: %s", code, exitNoPlan, errOut)
		}
		var o planner.Outcome
		if err := json.Unmarshal([]byte(out), &o); err != nil {
			t.Fatal(err)
		}
		if !o.Declined() || o.OK() || len(o.Attempts) != 0 {
			t.Fatalf("outcome: declined %v, ok %v, %d attempts", o.Declined(), o.OK(), len(o.Attempts))
		}
	})

	t.Run("backend down", func(t *testing.T) {
		if code, _, errOut := keelctl(t, compileArgs("--ollama-url", "http://127.0.0.1:1")...); code != exitError || !strings.Contains(errOut, "backend failed") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
	})

	t.Run("unknown backend", func(t *testing.T) {
		if code, _, _ := keelctl(t, compileArgs("--backend", "gpt")...); code != exitError {
			t.Fatalf("exit %d, want %d", code, exitError)
		}
	})

	t.Run("no intent", func(t *testing.T) {
		if code, _, _ := keelctl(t, append([]string{"plan", "compile"}, shipped...)...); code != exitError {
			t.Fatalf("exit %d, want %d", code, exitError)
		}
	})
}
