package exec

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Enriquefft/yap/pkg/yap/config"
	"github.com/Enriquefft/yap/pkg/yap/hint"
	"github.com/Enriquefft/yap/pkg/yap/inject"
)

// terminalTarget is the ordinary target used by most tests: a focused
// terminal whose window id happens to be a pid.
func terminalTarget() inject.Target {
	return inject.Target{
		DisplayServer: "wayland",
		WindowID:      "1234",
		AppClass:      "foot",
		AppType:       inject.AppTerminal,
	}
}

// newTestProvider builds a provider through the real factory so the
// tests exercise the same construction the daemon performs.
func newTestProvider(t *testing.T, command string) *provider {
	t.Helper()
	built, err := NewFactory(hint.Config{
		RootPath:             "/test/project",
		ExecCommand:          command,
		ConversationMaxBytes: 8000,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	p, ok := built.(*provider)
	if !ok {
		t.Fatalf("NewFactory returned %T, want *provider", built)
	}
	return p
}

// helperScript writes an executable shell script and returns its path
// for use as the provider's command.
func helperScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	return path
}

// helperEmitting returns a helper that prints exactly stdout and exits
// with exitCode. The payload is catted from a file so no amount of
// quoting in a fixture can change the script's meaning.
func helperEmitting(t *testing.T, stdout string, exitCode int) string {
	t.Helper()
	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, []byte(stdout), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return helperScript(t, "cat \""+payload+"\"\nexit "+strconv.Itoa(exitCode))
}

// fetchWithTimeout runs Fetch under a deadline, as the daemon does.
func fetchWithTimeout(t *testing.T, p *provider, timeout time.Duration) (hint.Bundle, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return p.Fetch(ctx, terminalTarget())
}

func TestName(t *testing.T) {
	if got := newTestProvider(t, "true").Name(); got != "exec" {
		t.Errorf("Name() = %q, want %q", got, "exec")
	}
}

func TestSupports(t *testing.T) {
	tests := []struct {
		name    string
		command string
		target  inject.Target
		want    bool
	}{
		{"terminal", "true", inject.Target{AppType: inject.AppTerminal}, true},
		{"electron", "true", inject.Target{AppType: inject.AppElectron}, true},
		{"browser", "true", inject.Target{AppType: inject.AppBrowser}, true},
		{"generic", "true", inject.Target{AppType: inject.AppGeneric}, true},
		// An empty title must never gate the provider: the helper is
		// the one that decides what to do without a title.
		{"terminal without title or window id", "true", inject.Target{AppType: inject.AppTerminal}, true},
		{"no command configured", "", inject.Target{AppType: inject.AppTerminal}, false},
		{"unknown app type", "true", inject.Target{AppType: inject.AppType(99)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newTestProvider(t, tt.command).Supports(tt.target); got != tt.want {
				t.Errorf("Supports(%+v) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

// TestFetchStatusMatrix walks every documented outcome of the v1
// contract and checks the two things the daemon acts on: whether
// conversation text came back, and whether the call is an error.
func TestFetchStatusMatrix(t *testing.T) {
	tests := []struct {
		name             string
		stdout           string
		exitCode         int
		wantConversation string
		wantErr          bool
	}{
		{
			name:             "ok",
			stdout:           `{"schema":1,"status":"ok","conversation":"user: hi\n\nassistant: hey","reason":"matched one session","diagnostics":{"session":"abc"}}`,
			wantConversation: "user: hi\n\nassistant: hey",
		},
		{
			name:   "ok with empty conversation",
			stdout: `{"schema":1,"status":"ok","conversation":"","reason":"session had no messages"}`,
		},
		{
			name:   "no_match",
			stdout: `{"schema":1,"status":"no_match","reason":"not an application I know"}`,
		},
		{
			name:   "ambiguous",
			stdout: `{"schema":1,"status":"ambiguous","reason":"three sessions share this window"}`,
		},
		{
			name:   "degraded",
			stdout: `{"schema":1,"status":"degraded","reason":"session file was truncated"}`,
		},
		{
			name:    "error",
			stdout:  `{"schema":1,"status":"error","reason":"could not read the session directory","diagnostics":{"errno":13}}`,
			wantErr: true,
		},
		{
			name:    "unknown status",
			stdout:  `{"schema":1,"status":"perhaps","reason":"from a newer helper"}`,
			wantErr: true,
		},
		{
			name:    "schema mismatch",
			stdout:  `{"schema":2,"status":"ok","conversation":"user: hi"}`,
			wantErr: true,
		},
		{
			name:    "empty stdout",
			stdout:  "",
			wantErr: true,
		},
		{
			name:     "non-zero exit",
			stdout:   `{"schema":1,"status":"ok","conversation":"user: hi"}`,
			exitCode: 3,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t, helperEmitting(t, tt.stdout, tt.exitCode))
			bundle, err := fetchWithTimeout(t, p, 5*time.Second)

			if tt.wantErr && err == nil {
				t.Errorf("Fetch returned nil error, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Fetch returned error %v, want nil", err)
			}
			if bundle.Conversation != tt.wantConversation {
				t.Errorf("Conversation = %q, want %q", bundle.Conversation, tt.wantConversation)
			}
			if tt.wantConversation != "" && bundle.Source != providerName {
				t.Errorf("Source = %q, want %q", bundle.Source, providerName)
			}
		})
	}
}

// TestFetchNonJSONStdoutKeepsConversationEmpty is the regression test
// that keeps a crashing helper out of the transform prompt. A Python
// traceback on stdout must end the call as an error with nothing in
// the Bundle — never as text that a language model is then asked to
// treat as the user's conversation.
func TestFetchNonJSONStdoutKeepsConversationEmpty(t *testing.T) {
	traceback := "Traceback (most recent call last):\n" +
		"  File \"/usr/lib/helper.py\", line 42, in <module>\n" +
		"    main()\n" +
		"KeyError: 'session'\n"

	p := newTestProvider(t, helperEmitting(t, traceback, 0))
	bundle, err := fetchWithTimeout(t, p, 5*time.Second)

	if err == nil {
		t.Fatal("Fetch returned nil error for non-JSON stdout, want an error")
	}
	if bundle.Conversation != "" {
		t.Errorf("Conversation = %q, want empty — a traceback must never reach the transform", bundle.Conversation)
	}
	if bundle.Source != "" {
		t.Errorf("Source = %q, want empty", bundle.Source)
	}
	if strings.Contains(err.Error(), "Traceback") {
		t.Errorf("error text carries the helper's stdout: %v", err)
	}
}

// TestFetchRejectsOversizedStdout checks the 1 MiB read cap: a helper
// that floods stdout is reported, not buffered without limit.
func TestFetchRejectsOversizedStdout(t *testing.T) {
	p := newTestProvider(t, helperEmitting(t, strings.Repeat("a", maxStdoutBytes+512), 0))
	bundle, err := fetchWithTimeout(t, p, 30*time.Second)

	if err == nil {
		t.Fatal("Fetch returned nil error for oversized stdout, want an error")
	}
	if bundle.Conversation != "" {
		t.Errorf("Conversation = %q, want empty", bundle.Conversation)
	}
}

// TestFetchDeadlineKillsProcessGroup checks that expiring the context
// takes down everything the helper spawned, not just the helper. The
// grandchild writes a marker after the deadline has passed: if only
// the direct child were killed, the marker would appear.
func TestFetchDeadlineKillsProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild-ran")
	p := newTestProvider(t, helperScript(t,
		"( sleep 0.4; echo alive > \""+marker+"\" ) &\nsleep 5"))

	start := time.Now()
	bundle, err := fetchWithTimeout(t, p, 100*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Fetch returned nil error after the deadline, want an error")
	}
	if bundle.Conversation != "" {
		t.Errorf("Conversation = %q, want empty", bundle.Conversation)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Fetch took %v, want it to return near the 100ms deadline", elapsed)
	}

	// Past the moment the grandchild would have written its marker.
	time.Sleep(900 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("grandchild outlived the deadline: the process group was not killed")
	}
}

// TestFetchToleratesDescendantHoldingOutput is the counterpart to the
// test above and the far likelier case: the helper answers correctly
// and exits, but something it started is still alive holding the
// descriptor it inherited. Nothing is hung and nothing is hostile — a
// backgrounded cleanup, an ssh control master, a stray subshell.
//
// The answer has to come back, promptly, whichever descriptor is being
// held. Both variants regressed before this: stdout read from a
// StdoutPipe blocked until the descendant let go, costing the whole
// deadline and then reporting a helper that had answered in
// milliseconds as never having answered; stderr surfaced as
// ErrWaitDelay out of Wait and was reported as a failed call. Either
// way a complete, valid conversation was thrown away.
func TestFetchToleratesDescendantHoldingOutput(t *testing.T) {
	const answer = `{"schema":1,"status":"ok","conversation":"user: hi"}`

	tests := []struct {
		name string
		// redirect sends one of the descendant's two descriptors to
		// /dev/null; the other is the one it keeps open.
		redirect string
	}{
		{name: "descendant holds stdout", redirect: "2>/dev/null"},
		{name: "descendant holds stderr", redirect: ">/dev/null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t, helperScript(t,
				"( sleep 10 ) "+tt.redirect+" &\n"+
					"printf '%s' '"+answer+"'"))

			start := time.Now()
			bundle, err := fetchWithTimeout(t, p, 30*time.Second)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("Fetch: %v — a valid answer must survive a descendant holding a descriptor", err)
			}
			if bundle.Conversation != "user: hi" {
				t.Errorf("Conversation = %q, want %q", bundle.Conversation, "user: hi")
			}
			if bundle.Source != providerName {
				t.Errorf("Source = %q, want %q", bundle.Source, providerName)
			}
			// The helper exits immediately; only WaitDelay is owed on
			// top of that. Anything near the descendant's 10s means
			// the call waited on the descriptor again.
			if elapsed > 5*time.Second {
				t.Errorf("Fetch took %v, want it to return as soon as the helper exited", elapsed)
			}
		})
	}
}

// dumpedEnv runs a helper that records its environment and returns the
// YAP_HINT_* variables it saw, plus the raw dump for leak checks.
func dumpedEnv(t *testing.T, p *provider) (map[string]string, string) {
	t.Helper()
	dump := filepath.Join(t.TempDir(), "env")
	script := helperScript(t, "env > \""+dump+"\"\n"+
		`printf '%s' '{"schema":1,"status":"no_match","reason":"environment dump"}'`)
	p.command = script

	if _, err := fetchWithTimeout(t, p, 10*time.Second); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	seen := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			seen[name] = value
		}
	}
	return seen, string(raw)
}

// TestChildEnvCarriesTheContract checks that every variable the v1
// contract defines reaches the helper, always set, with unknown values
// spelled as empty rather than left out.
func TestChildEnvCarriesTheContract(t *testing.T) {
	t.Setenv("YAP_EXEC_TEST_PASSTHROUGH", "kept")
	// A stale variable under the prefix must not survive into the
	// child: everything the helper reads there has to come from yap.
	t.Setenv("YAP_HINT_WINDOW_ID", "stale-from-parent")

	seen, _ := dumpedEnv(t, newTestProvider(t, ""))

	want := map[string]string{
		"YAP_HINT_API":          "1",
		"YAP_HINT_WINDOW_ID":    "1234",
		"YAP_HINT_WINDOW_TITLE": "",
		"YAP_HINT_APP_TYPE":     "terminal",
		"YAP_HINT_ROOT_PATH":    "/test/project",
		"YAP_HINT_MAX_BYTES":    "8000",
	}
	for name, wantValue := range want {
		value, ok := seen[name]
		if !ok {
			t.Errorf("%s was not set in the child environment", name)
			continue
		}
		if value != wantValue {
			t.Errorf("%s = %q, want %q", name, value, wantValue)
		}
	}

	deadline, ok := seen["YAP_HINT_DEADLINE_MS"]
	if !ok {
		t.Error("YAP_HINT_DEADLINE_MS was not set in the child environment")
	} else if ms, err := strconv.Atoi(deadline); err != nil || ms <= 0 {
		t.Errorf("YAP_HINT_DEADLINE_MS = %q, want a positive number of milliseconds", deadline)
	}

	if seen["YAP_EXEC_TEST_PASSTHROUGH"] != "kept" {
		t.Error("the helper did not inherit yap's own environment")
	}
}

// TestChildEnvStripsSecrets checks that no configured secret reaches
// the helper. The names come from reflection over the config schema,
// not from a list kept here, so a secret added to the schema later is
// guarded by this test the moment it is tagged.
func TestChildEnvStripsSecrets(t *testing.T) {
	const sentinel = "sk-test-do-not-leak"

	names := secretEnvNamesFromSchema(t)
	if len(names) == 0 {
		t.Fatal("no secret env names found in the config schema — the guard would pass vacuously")
	}
	for _, name := range names {
		t.Setenv(name, sentinel)
	}

	seen, raw := dumpedEnv(t, newTestProvider(t, ""))

	for _, name := range names {
		if _, ok := seen[name]; ok {
			t.Errorf("secret %s reached the helper", name)
		}
	}
	if strings.Contains(raw, sentinel) {
		t.Error("a secret value reached the helper's environment")
	}
}

// secretEnvNamesFromSchema reflects over config.Config and returns the
// environment variable names of every field tagged yap:"secret".
//
// The tag is parsed here rather than through config.SecretEnvNames on
// purpose: this test is what proves that function strips what the
// schema actually declares, so it must not take that function's word
// for what the schema says.
func secretEnvNamesFromSchema(t *testing.T) []string {
	t.Helper()
	var names []string
	cfgType := reflect.TypeOf(config.Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		section := cfgType.Field(i).Type
		if section.Kind() != reflect.Struct {
			continue
		}
		for j := 0; j < section.NumField(); j++ {
			field := section.Field(j)
			tag := field.Tag.Get("yap")
			if !strings.HasPrefix(tag, "secret;") && tag != "secret" {
				continue
			}
			declared := envNamesFromTag(tag)
			if len(declared) == 0 {
				t.Errorf("%s.%s is tagged secret but declares no env= names; add env=NAME to its yap tag so it is stripped from helper environments",
					section.Name(), field.Name)
				continue
			}
			names = append(names, declared...)
		}
	}
	return names
}

// envNamesFromTag pulls the comma-separated env= names out of a yap
// struct tag, stopping at the greedy doc= part.
func envNamesFromTag(tag string) []string {
	for _, part := range strings.Split(tag, ";") {
		if part == "secret" {
			continue
		}
		if strings.HasPrefix(part, "doc=") {
			return nil
		}
		if after, ok := strings.CutPrefix(part, "env="); ok {
			return strings.Split(after, ",")
		}
	}
	return nil
}

func TestFetchWithoutCommandIsAnError(t *testing.T) {
	bundle, err := fetchWithTimeout(t, newTestProvider(t, ""), time.Second)
	if err == nil {
		t.Fatal("Fetch with no command returned nil error, want an error")
	}
	if bundle.Conversation != "" {
		t.Errorf("Conversation = %q, want empty", bundle.Conversation)
	}
}

func TestNewFactoryLeavesProviderDisabledWithoutCommand(t *testing.T) {
	p := newTestProvider(t, "   ")
	if p.Supports(terminalTarget()) {
		t.Error("a whitespace-only command should leave the provider disabled")
	}
}
