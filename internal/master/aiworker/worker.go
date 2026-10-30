// Package aiworker runs the official CLIs without interpolating shell commands.
package aiworker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Command struct {
	Binary    string
	Args, Env []string
	Dir       string
}
type Request struct {
	Provider, Action, Workspace, Prompt, Session string
	Edit                                         bool
}

func ValidProvider(p string) bool { return p == "codex" || p == "claude" }
func Arguments(r Request) ([]string, error) {
	if !ValidProvider(r.Provider) {
		return nil, fmt.Errorf("unknown provider")
	}
	switch r.Action {
	case "login":
		if r.Provider == "codex" {
			return []string{"login", "--device-auth"}, nil
		}
		return []string{"auth", "login"}, nil
	case "status":
		if r.Provider == "codex" {
			return []string{"login", "status"}, nil
		}
		return []string{"auth", "status", "--text"}, nil
	case "logout":
		if r.Provider == "codex" {
			return []string{"logout"}, nil
		}
		return []string{"auth", "logout"}, nil
	case "run":
		if r.Provider == "codex" {
			mode := "read-only"
			if r.Edit {
				mode = "workspace-write"
			}
			args := []string{"exec", "--json", "--skip-git-repo-check", "--sandbox", mode, "--cd", r.Workspace, "-c", "approval_policy=\"never\""}
			if r.Session != "" {
				args = append(args, "resume", r.Session)
			}
			return append(args, "-"), nil
		}
		tools := "Read,Glob,Grep"
		mode := "plan"
		if r.Edit {
			tools += ",Edit,Write"
			mode = "acceptEdits"
		}
		args := []string{"-p", "--verbose", "--output-format", "stream-json", "--permission-mode", mode, "--tools", tools, "--allowedTools", tools, "--setting-sources", "user", "--strict-mcp-config", "--mcp-config", "{\"mcpServers\":{}}"}
		if r.Session != "" {
			args = append(args, "--resume", r.Session)
		}
		return args, nil
	}
	return nil, fmt.Errorf("unknown action")
}

type Worker struct {
	Root    string
	Resolve func(string) (string, error)
}

func New(root string) *Worker {
	root, _ = filepath.Abs(root)
	return &Worker{Root: root, Resolve: exec.LookPath}
}
func (w *Worker) command(r Request) (Command, error) {
	args, err := Arguments(r)
	if err != nil {
		return Command{}, err
	}
	home := filepath.Join(w.Root, "auth", r.Provider)
	if err = os.MkdirAll(home, 0700); err != nil {
		return Command{}, err
	}
	binary := ""
	script := filepath.Join(w.Root, "tools", "node_modules", "@openai", "codex", "bin", "codex.js")
	if r.Provider == "claude" {
		script = filepath.Join(w.Root, "tools", "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	}
	if _, err = os.Stat(script); err == nil {
		binary, err = w.Resolve("node")
		args = append([]string{script}, args...)
	} else {
		binary, err = w.Resolve(r.Provider)
	}
	if err != nil {
		return Command{}, fmt.Errorf("install %s CLI first: %w", r.Provider, err)
	}
	env := []string{}
	allowed := map[string]bool{"PATH": true, "PATHEXT": true, "SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "TEMP": true, "TMP": true, "LANG": true, "LC_ALL": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true}
	for _, v := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(v, "=", 2)[0])
		if allowed[key] {
			env = append(env, v)
		}
	}
	env = append(env, "CODEX_HOME="+home, "CLAUDE_CONFIG_DIR="+home, "HOME="+home, "USERPROFILE="+home, "APPDATA="+filepath.Join(home, "appdata"), "LOCALAPPDATA="+filepath.Join(home, "local"), "NO_COLOR=1")
	if r.Provider == "codex" {
		args = append(args, "-c", "cli_auth_credentials_store=\"file\"")
	}
	dir := home
	if r.Action == "run" {
		dir = r.Workspace
	}
	return Command{Binary: binary, Args: args, Env: env, Dir: dir}, nil
}
func (w *Worker) InstallCommand() (Command, error) {
	node, err := w.Resolve("node")
	if err != nil {
		return Command{}, fmt.Errorf("Node.js is required to install the CLIs")
	}
	npm, err := w.Resolve("npm")
	if err != nil {
		return Command{}, err
	}
	candidates := []string{filepath.Join(filepath.Dir(npm), "node_modules", "npm", "bin", "npm-cli.js"), filepath.Join(filepath.Dir(node), "node_modules", "npm", "bin", "npm-cli.js"), filepath.Join(filepath.Dir(node), "..", "lib", "node_modules", "npm", "bin", "npm-cli.js")}
	for _, p := range candidates {
		if _, err = os.Stat(p); err == nil {
			return Command{Binary: node, Args: []string{p, "install", "--prefix", filepath.Join(w.Root, "tools"), "--no-audit", "--no-fund", "@openai/codex", "@anthropic-ai/claude-code"}, Dir: w.Root, Env: os.Environ()}, nil
		}
	}
	return Command{}, fmt.Errorf("npm-cli.js not found; install the official CLIs on this host")
}

type Process struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc
}

func (w *Worker) Start(ctx context.Context, r Request, output io.Writer) (*Process, <-chan error, error) {
	if err := os.MkdirAll(w.Root, 0700); err != nil {
		return nil, nil, err
	}
	var spec Command
	var err error
	if r.Action == "install" {
		spec, err = w.InstallCommand()
	} else {
		spec, err = w.command(r)
	}
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	cmd.Stdout = output
	cmd.Stderr = output
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	cmd.WaitDelay = 5 * time.Second
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, nil, err
	}
	p := &Process{cmd: cmd, stdin: stdin, cancel: cancel}
	done := make(chan error, 1)
	if r.Action == "run" {
		_, err = io.WriteString(stdin, r.Prompt+"\n")
		stdin.Close()
		if err != nil {
			p.Stop()
		}
	}
	go func() { e := cmd.Wait(); cancel(); done <- e; close(done) }()
	return p, done, nil
}
func (p *Process) Input(text string) error {
	if len(text) > 8192 {
		return fmt.Errorf("input too long")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := io.WriteString(p.stdin, text+"\n")
	return err
}
func (p *Process) Stop() { p.mu.Lock(); defer p.mu.Unlock(); killProcess(p.cmd); p.cancel() }
