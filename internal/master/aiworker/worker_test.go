package aiworker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderArguments(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, edit := range []bool{true, false} {
			args, e := Arguments(Request{Provider: provider, Action: "run", Workspace: "/project", Prompt: "$(touch /bad)", Edit: edit})
			if e != nil {
				t.Fatal(e)
			}
			all := strings.Join(args, " ")
			if strings.Contains(all, "touch") || strings.Contains(all, "dangerously") {
				t.Fatal("unsafe command construction", all)
			}
			if provider == "codex" && !strings.Contains(all, map[bool]string{true: "workspace-write", false: "read-only"}[edit]) {
				t.Fatal(all)
			}
			if provider == "claude" && strings.Contains(all, "Bash") {
				t.Fatal("unexpected shell permission")
			}
		}
	}
	if _, e := Arguments(Request{Provider: "arbitrary", Action: "run"}); e == nil {
		t.Fatal("unknown provider accepted")
	}
}
func TestDedicatedProviderEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-inherit")
	t.Setenv("DATABASE_PASSWORD", "must-not-inherit")
	w := New(t.TempDir())
	w.Resolve = func(string) (string, error) { return "fake-cli", nil }
	c, e := w.command(Request{Provider: "codex", Action: "status"})
	if e != nil {
		t.Fatal(e)
	}
	all := strings.Join(c.Env, "\n")
	if strings.Contains(all, "must-not-inherit") {
		t.Fatal("inherited secrets")
	}
	if !strings.Contains(all, "CODEX_HOME="+filepath.Join(w.Root, "auth", "codex")) {
		t.Fatal("shared system login directory used")
	}
}
func fakeCLI(t *testing.T, body string) *Worker {
	t.Helper()
	if _, e := exec.LookPath("node"); e != nil {
		t.Skip("Node.js not installed")
	}
	w := New(t.TempDir())
	p := filepath.Join(w.Root, "tools", "node_modules", "@openai", "codex", "bin")
	if e := os.MkdirAll(p, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(p, "codex.js"), []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	return w
}
func TestPromptUsesStdinAndStreamsOutput(t *testing.T) {
	w := fakeCLI(t, `let input='';process.stdin.on('data',b=>input+=b);process.stdin.on('end',()=>{console.log(JSON.stringify({type:'thread.started',thread_id:'test-session'}));console.log(input);});`)
	var out bytes.Buffer
	_, done, e := w.Start(context.Background(), Request{Provider: "codex", Action: "run", Workspace: t.TempDir(), Prompt: "hello ; arbitrary shell syntax"}, &out)
	if e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "hello ; arbitrary shell syntax") || !strings.Contains(out.String(), "test-session") {
		t.Fatal(out.String())
	}
}
func TestCancelStopsCLI(t *testing.T) {
	w := fakeCLI(t, `setInterval(()=>{},1000);`)
	var out bytes.Buffer
	p, done, e := w.Start(context.Background(), Request{Provider: "codex", Action: "login"}, &out)
	if e != nil {
		t.Fatal(e)
	}
	p.Stop()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("cancelled process succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation left process alive")
	}
}
