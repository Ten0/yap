package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Enriquefft/yap/pkg/yap/config"
	"github.com/Enriquefft/yap/pkg/yap/hint"
	"github.com/Enriquefft/yap/pkg/yap/inject"
)

// providerName is the registry name, Bundle.Source value, and log tag.
const providerName = "exec"

// contractVersion is the value of YAP_HINT_API. It changes only when
// the input or output shape changes in a way a helper must notice.
const contractVersion = "1"

// shell interprets the configured command line. Spelled absolutely
// rather than looked up on PATH so that which shell runs a user's
// command never depends on how the daemon happened to be launched.
const shell = "/bin/sh"

// maxStdoutBytes caps how much of the helper's stdout is read into
// memory. A megabyte is far past any real conversation, so a helper
// that exceeds it is malfunctioning rather than verbose.
const maxStdoutBytes = 1 << 20

// maxStderrBytes caps the stderr tail kept for diagnostics. Enough for
// a stack trace, small enough to log without flooding the journal.
const maxStderrBytes = 4 << 10

// stdoutHeadBytes is how much unparseable stdout is logged. Enough to
// recognize a traceback or a shell error, not enough to be a dump.
const stdoutHeadBytes = 256

// envPrefix namespaces every variable this provider sets.
const envPrefix = "YAP_HINT_"

// hintVarCount is how many variables the contract defines, used to
// size the child environment exactly once.
const hintVarCount = 7

// waitDelay bounds how long Wait lingers after the deadline killed the
// helper. Without it, a grandchild holding the inherited stderr open
// would keep yap waiting past its own deadline.
const waitDelay = 100 * time.Millisecond

// provider runs a helper command and adopts its stdout as conversation
// context. It knows nothing about any particular application: the
// window description goes out as environment variables, a small JSON
// answer comes back, and everything application-specific stays inside
// the helper's opaque diagnostics.
type provider struct {
	// command is the shell command line to run. Empty disables the
	// provider — Supports answers false and nothing is ever spawned.
	command string

	// rootPath is the project-directory signal handed to the helper.
	rootPath string

	// maxBytes is the conversation budget handed to the helper so it
	// can truncate at the source instead of shipping text yap will
	// only throw away.
	maxBytes int

	// secretEnv names the environment variables stripped from the
	// child. Derived from the config schema rather than hand-kept, so
	// a secret added to the schema later is stripped without anyone
	// remembering that this package exists.
	secretEnv []string
}

// NewFactory returns a hint.Factory that constructs exec providers.
// An empty ExecCommand is not an error: it yields a provider that
// declines every target, which is how the feature stays off until a
// user configures it.
func NewFactory(cfg hint.Config) (hint.Provider, error) {
	return &provider{
		command:   strings.TrimSpace(cfg.ExecCommand),
		rootPath:  cfg.RootPath,
		maxBytes:  cfg.ConversationMaxBytes,
		secretEnv: config.SecretEnvNames(),
	}, nil
}

func (p *provider) Name() string { return providerName }

// Supports gates on the app type and nothing else. In particular it
// does not require a window title: an empty title is a legitimate
// input that the helper handles itself, and refusing to run without
// one would silently disable the provider on every compositor that
// does not publish titles.
func (p *provider) Supports(target inject.Target) bool {
	if p.command == "" {
		return false
	}
	switch target.AppType {
	case inject.AppGeneric, inject.AppTerminal, inject.AppElectron, inject.AppBrowser:
		return true
	default:
		// An app type this build does not know cannot be named within
		// the contract's four values — YAP_HINT_APP_TYPE would carry
		// something like "AppType(7)". Decline rather than hand the
		// helper a value the contract does not define.
		return false
	}
}

// Fetch runs the helper and turns its answer into a Bundle. Every
// outcome is reported: a Bundle with conversation, an empty Bundle for
// the cases where the helper legitimately has nothing to say, or an
// error when the helper itself misbehaved. Nothing is ever swallowed.
func (p *provider) Fetch(ctx context.Context, target inject.Target) (hint.Bundle, error) {
	if p.command == "" {
		return hint.Bundle{}, errors.New("exec: no command configured")
	}
	res, err := p.run(ctx, target)
	if err != nil {
		return hint.Bundle{}, err
	}
	return p.interpret(ctx, res)
}

// runResult is everything one helper invocation produced. Collected
// first, judged second, so the classification below reads as a single
// list of cases instead of being tangled through the I/O.
type runResult struct {
	stdout     []byte
	stderr     string
	waitErr    error
	overflow   bool
	elapsed    time.Duration
	deadlineMS string
}

// run spawns the helper, collects its output under fixed bounds, and
// waits for it. It returns an error only when the helper could not be
// started at all; a helper that ran and failed is reported through
// runResult so interpret can classify it.
func (p *provider) run(ctx context.Context, target inject.Target) (runResult, error) {
	start := time.Now()
	deadlineMS := remainingMS(ctx, start)

	cmd := osexec.CommandContext(ctx, shell, "-c", p.command)
	cmd.Env = p.childEnv(os.Environ(), target, deadlineMS)
	// A nil Stdin is the null device: this is a one-way query, and a
	// helper must never block waiting for input that is not coming.
	cmd.Stdin = nil
	stderr := &cappedBuffer{limit: maxStderrBytes}
	cmd.Stderr = stderr
	// Give the helper its own process group so the deadline can reach
	// everything it spawned. A helper that shells out to ssh or a
	// pipeline would otherwise leave those children running after yap
	// has stopped waiting for them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process) }
	cmd.WaitDelay = waitDelay

	// Buffered through cmd.Stdout, as stderr already is, rather than
	// read from a StdoutPipe. os/exec fills this from its own
	// goroutine and cmd.WaitDelay is what bounds that goroutine;
	// reading the pipe here would instead block until every process
	// holding its write end let go, which no deadline can cut short.
	// A helper that answers correctly and leaves `( sleep 30 ) &`
	// behind holds it — the common accident, not the hostile case.
	//
	// The one byte over the cap distinguishes "exactly at the cap"
	// from "over it". cappedBuffer keeps the first limit bytes and
	// reports a full write for the rest, so a helper that floods
	// stdout is discarded as it arrives instead of blocking on a full
	// pipe, and memory stays bounded either way.
	stdout := &cappedBuffer{limit: maxStdoutBytes + 1}
	cmd.Stdout = stdout

	if err := cmd.Start(); err != nil {
		slog.Warn("exec: helper failed to start",
			"provider", providerName, "command", p.command, "error", err)
		return runResult{}, fmt.Errorf("exec: start helper: %w", err)
	}

	// Wait before reading either buffer: both are filled by os/exec
	// goroutines, and Wait is what guarantees those goroutines have
	// finished — or, past WaitDelay, that os/exec has stopped waiting
	// for them. cappedBuffer's mutex covers that second case.
	waitErr := cmd.Wait()

	out := []byte(stdout.String())
	return runResult{
		stdout:     out[:min(len(out), maxStdoutBytes)],
		stderr:     strings.TrimSpace(stderr.String()),
		waitErr:    waitErr,
		overflow:   len(out) > maxStdoutBytes,
		elapsed:    time.Since(start),
		deadlineMS: deadlineMS,
	}, nil
}

// interpret classifies one helper invocation. The cases are spelled
// out in full and in the order they can occur — process-level failures
// first, then the response envelope, then the status the helper chose.
func (p *provider) interpret(ctx context.Context, res runResult) (hint.Bundle, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		slog.Warn("exec: helper did not answer within the deadline",
			"provider", providerName, "deadline_ms", res.deadlineMS,
			"elapsed_ms", res.elapsed.Milliseconds(), "stderr", res.stderr)
		return hint.Bundle{}, fmt.Errorf("exec: helper did not answer within %sms: %w", res.deadlineMS, ctxErr)
	}

	if res.waitErr != nil {
		var exitErr *osexec.ExitError
		if errors.As(res.waitErr, &exitErr) {
			slog.Warn("exec: helper exited non-zero",
				"provider", providerName, "exit_code", exitErr.ExitCode(), "stderr", res.stderr)
			return hint.Bundle{}, fmt.Errorf("exec: helper exited with status %d: %s",
				exitErr.ExitCode(), orNoStderr(res.stderr))
		}
		if !errors.Is(res.waitErr, osexec.ErrWaitDelay) {
			slog.Warn("exec: waiting for the helper failed",
				"provider", providerName, "error", res.waitErr, "stderr", res.stderr)
			return hint.Bundle{}, fmt.Errorf("exec: wait for helper: %w", res.waitErr)
		}
		// ErrWaitDelay can only mean the helper itself exited zero —
		// os/exec reports a non-zero exit in preference to it — and
		// that something it spawned still held stdout or stderr open
		// when Wait stopped waiting for the copy. The answer is
		// already buffered, so this is not a failure: discarding a
		// complete conversation over a descendant's stray descriptor
		// would fail the ordinary case for the hostile one's sake. If
		// the copy really was cut mid-object, the decode below says so
		// on the evidence rather than on the suspicion.
		slog.Info("exec: helper left a descendant holding its output open",
			"provider", providerName, "wait_delay_ms", waitDelay.Milliseconds(),
			"stdout_bytes", len(res.stdout), "stderr", res.stderr)
	}

	if res.overflow {
		slog.Warn("exec: helper stdout exceeded the cap",
			"provider", providerName, "max_bytes", maxStdoutBytes,
			"stdout_head", head(res.stdout), "stderr", res.stderr)
		return hint.Bundle{}, fmt.Errorf("exec: helper wrote more than %d bytes to stdout", maxStdoutBytes)
	}

	var resp response
	if err := json.Unmarshal(res.stdout, &resp); err != nil {
		// The head of stdout is logged because this is where a Python
		// traceback or a shell error message shows up, and reading it
		// in the journal is the whole diagnosis. It goes to the log
		// and nowhere near the returned Bundle.
		slog.Warn("exec: helper stdout is not one JSON object",
			"provider", providerName, "stdout_head", head(res.stdout),
			"error", err, "stderr", res.stderr)
		return hint.Bundle{}, fmt.Errorf("exec: decode helper response: %w", err)
	}

	if resp.Schema != 1 {
		slog.Warn("exec: helper answered with an unknown schema",
			"provider", providerName, "schema", resp.Schema, "stderr", res.stderr)
		return hint.Bundle{}, fmt.Errorf("exec: unsupported response schema %d", resp.Schema)
	}

	return p.classify(resp)
}

// classify turns a well-formed response into the Bundle-and-error pair
// its status calls for. Only "ok" can produce conversation text; every
// other status yields an empty Bundle, and only a helper-reported
// error or an unrecognized status is worth failing over.
func (p *provider) classify(resp response) (hint.Bundle, error) {
	diagnostics := string(resp.Diagnostics)

	switch resp.Status {
	case "ok":
		if resp.Conversation == "" {
			slog.Info("exec: helper matched but returned no conversation",
				"provider", providerName, "reason", orNoReason(resp.Reason), "diagnostics", diagnostics)
			return hint.Bundle{}, nil
		}
		slog.Info("exec: context matched",
			"provider", providerName, "bytes", len(resp.Conversation), "diagnostics", diagnostics)
		return hint.Bundle{Conversation: resp.Conversation, Source: providerName}, nil

	case "no_match":
		// The ordinary case of dictating into a window the helper has
		// no context for. Not a failure, and not worth a warning.
		slog.Info("exec: no context for this window",
			"provider", providerName, "reason", orNoReason(resp.Reason))
		return hint.Bundle{}, nil

	case "degraded", "ambiguous":
		// The helper knows its answer would be wrong or incomplete.
		// Worth a warning so the user can see why context stopped
		// arriving, but not an error: dictation proceeds without it.
		slog.Warn("exec: helper declined to answer",
			"provider", providerName, "status", resp.Status,
			"reason", orNoReason(resp.Reason), "diagnostics", diagnostics)
		return hint.Bundle{}, nil

	case "error":
		slog.Warn("exec: helper reported an error",
			"provider", providerName, "reason", orNoReason(resp.Reason), "diagnostics", diagnostics)
		return hint.Bundle{}, fmt.Errorf("exec: helper reported an error: %s", orNoReason(resp.Reason))

	default:
		slog.Warn("exec: helper answered with an unknown status",
			"provider", providerName, "status", resp.Status,
			"reason", orNoReason(resp.Reason), "diagnostics", diagnostics)
		return hint.Bundle{}, fmt.Errorf("exec: unknown status %q", resp.Status)
	}
}

// response is the helper's stdout contract. Diagnostics stays raw on
// purpose: it is logged verbatim and never parsed, which is what lets
// helpers carry application-specific detail without any of it leaking
// into this package's decisions.
type response struct {
	Schema       int             `json:"schema"`
	Status       string          `json:"status"`
	Conversation string          `json:"conversation"`
	Reason       string          `json:"reason"`
	Diagnostics  json.RawMessage `json:"diagnostics"`
}

// childEnv builds the helper's environment: yap's own, minus secrets,
// minus any YAP_HINT_* the daemon itself was started with, plus the
// contract variables. Stripping the inherited prefix is what lets a
// helper trust that everything it reads under YAP_HINT_ came from yap.
func (p *provider) childEnv(parent []string, target inject.Target, deadlineMS string) []string {
	out := make([]string, 0, len(parent)+hintVarCount)
	for _, entry := range parent {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(name, envPrefix) || slices.Contains(p.secretEnv, name) {
			continue
		}
		out = append(out, entry)
	}
	return append(out,
		envPrefix+"API="+contractVersion,
		// WindowID is opaque and passed through exactly as detection
		// produced it: usually the focused process id, but an X11
		// window id when _NET_WM_PID is missing, and empty on generic
		// wlroots. A helper must check /proc/<id> before believing it
		// is a pid.
		envPrefix+"WINDOW_ID="+target.WindowID,
		// inject.Target carries no title in v1. The variable is still
		// always set, because the contract spells "unknown" as empty,
		// and a helper that needs the title can look it up itself —
		// that lookup is application knowledge, which lives out there.
		envPrefix+"WINDOW_TITLE=",
		envPrefix+"APP_TYPE="+target.AppType.String(),
		envPrefix+"ROOT_PATH="+p.rootPath,
		envPrefix+"MAX_BYTES="+positiveOrEmpty(p.maxBytes),
		envPrefix+"DEADLINE_MS="+deadlineMS,
	)
}

// killGroup stops the helper and everything it spawned.
func killGroup(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	// A negative pid signals the whole process group established by
	// Setpgid. ESRCH just means it finished on its own.
	err := syscall.Kill(-proc.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	// The group could not be signalled; stop at least the helper
	// itself rather than leave it running past the deadline.
	return proc.Kill()
}

// remainingMS renders the milliseconds left on ctx as of now, or ""
// when there is no deadline — the contract's spelling for unknown.
func remainingMS(ctx context.Context, now time.Time) string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ""
	}
	return strconv.FormatInt(max(deadline.Sub(now).Milliseconds(), 0), 10)
}

// positiveOrEmpty renders a budget for the helper: a positive value in
// decimal, anything else as "" meaning unknown.
func positiveOrEmpty(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// head returns the leading bytes of unparseable stdout for logging.
func head(b []byte) string {
	if len(b) <= stdoutHeadBytes {
		return string(b)
	}
	return string(b[:stdoutHeadBytes]) + "…"
}

// orNoReason keeps an empty reason from logging as a blank field: a
// helper that failed without saying why is itself worth seeing.
func orNoReason(reason string) string {
	if reason == "" {
		return "(helper gave no reason)"
	}
	return reason
}

// orNoStderr does the same for an empty stderr tail.
func orNoStderr(stderr string) string {
	if stderr == "" {
		return "(no stderr)"
	}
	return stderr
}

// cappedBuffer keeps the first limit bytes written to it and discards
// the rest, always reporting a full write so the helper never sees a
// short write or a broken pipe on stderr.
//
// os/exec fills it from its own goroutine, so the mutex is what makes
// reading the result safe even in the corner where Wait gives up on a
// copy that outlived WaitDelay.
type cappedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}
