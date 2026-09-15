package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Enriquefft/yap/internal/engine"
	"github.com/Enriquefft/yap/internal/platform"
	"github.com/Enriquefft/yap/pkg/yap/transcribe"
	"github.com/Enriquefft/yap/pkg/yap/transcribe/mock"
	"github.com/Enriquefft/yap/pkg/yap/transform/passthrough"
	"github.com/stretchr/testify/require"
)

// instantRecorder returns from Start straight away -- no error, and
// without waiting to be cancelled. It stands for a recorder that ended
// on its own, the one stop the engine has nothing to attribute.
type instantRecorder struct{ wav []byte }

func (r *instantRecorder) Start(context.Context) error { return nil }
func (r *instantRecorder) Encode() ([]byte, error)     { return r.wav, nil }
func (r *instantRecorder) Close()                      {}

// loggedStopReasons decodes the JSON slog stream and returns the reason
// attribute of every "recording stopped" record. Decoding rather than
// substring-matching keeps the assertion on the structured attribute
// operators actually filter on, and counts the lines so a reason logged
// twice fails just as loudly as one logged wrong.
func loggedStopReasons(t *testing.T, logged string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if line == "" {
			continue
		}
		rec := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		if rec["msg"] != "recording stopped" {
			continue
		}
		reason, ok := rec["reason"].(string)
		require.True(t, ok, "no string reason attribute: %s", line)
		out = append(out, reason)
	}
	return out
}

// runForStopReason runs one pipeline against the given recording
// context and returns the stop reasons it logged. Run's own error is
// deliberately ignored: a cancelled recording still goes on to
// transcribe and a broken recorder returns early, but either way the
// reason must have been logged before that split.
func runForStopReason(t *testing.T, rec platform.Recorder, recCtx context.Context) []string {
	t.Helper()
	var buf bytes.Buffer
	eng, err := engine.New(rec, &mockChime{}, nil,
		mock.New(transcribe.TranscriptChunk{Text: "ok", IsFinal: true}),
		passthrough.New(), &recordingInjector{},
		slog.New(slog.NewJSONHandler(&buf, nil)))
	require.NoError(t, err)

	_ = eng.Run(context.Background(), engine.RunOptions{
		RecordCtx:      recCtx,
		StreamPartials: true,
	})
	return loggedStopReasons(t, buf.String())
}

// cancelledWithCause returns a recording context already cancelled with
// cause, the way one of the daemon's stop sites cancels it.
func cancelledWithCause(cause error) context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	return ctx
}

// expiredDeadline returns a context whose deadline has already passed
// with no cause attached -- a caller that reached for a plain
// context.WithTimeout for its recording budget.
func expiredDeadline() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	return ctx
}

// TestEngineRun_StopReasonLogged pins each way a recording can end to
// the reason logged for it. Before stop causes, five of these were one
// indistinguishable context.Canceled and the log could not tell an
// intentional stop from the daemon dying under the user.
func TestEngineRun_StopReasonLogged(t *testing.T) {
	deviceErr := errors.New("device unavailable")

	cases := []struct {
		name     string
		recCtx   context.Context
		startErr error
		want     string
	}{
		{
			name:   "hotkey release",
			recCtx: cancelledWithCause(engine.ErrStopHotkeyRelease),
			want:   "hotkey_release",
		},
		{
			name:   "toggle",
			recCtx: cancelledWithCause(engine.ErrStopToggle),
			want:   "toggle",
		},
		{
			name:   "silence",
			recCtx: cancelledWithCause(engine.ErrStopSilence),
			want:   "silence",
		},
		{
			name:   "max duration",
			recCtx: cancelledWithCause(engine.ErrStopMaxDuration),
			want:   "max_duration",
		},
		{
			name:   "daemon shutdown",
			recCtx: cancelledWithCause(engine.ErrStopDaemonShutdown),
			want:   "daemon_shutdown",
		},
		{
			name:   "interrupt",
			recCtx: cancelledWithCause(engine.ErrStopInterrupt),
			want:   "interrupt",
		},
		{
			name:   "an unlabelled deadline is the max duration budget",
			recCtx: expiredDeadline(),
			want:   "max_duration",
		},
		{
			name:   "an unlabelled cancel is attributed to nothing",
			recCtx: preCancelledRecCtx(),
			want:   "unknown",
		},
		{
			name:     "a broken recorder outranks the context's cause",
			recCtx:   cancelledWithCause(engine.ErrStopHotkeyRelease),
			startErr: deviceErr,
			want:     "recorder_error",
		},
		{
			name:     "a broken recorder with nothing cancelled at all",
			recCtx:   context.Background(),
			startErr: deviceErr,
			want:     "recorder_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runForStopReason(t,
				&mockRecorder{wavData: []byte("fake-wav"), startErr: tc.startErr},
				tc.recCtx)
			require.Equal(t, []string{tc.want}, got,
				"exactly one line, naming why the recording ended")
		})
	}
}

// TestEngineRun_StopReasonUnattributable covers the null case. A
// recorder that returned of its own accord leaves no error and no
// cancelled context to read, and the engine has to admit that rather
// than pick the nearest plausible reason.
func TestEngineRun_StopReasonUnattributable(t *testing.T) {
	got := runForStopReason(t, &instantRecorder{wav: []byte("fake-wav")}, context.Background())
	require.Equal(t, []string{"unknown"}, got,
		"a stop with no error and no cancellation must not be given a reason that was never checked")
}

// TestEngineRun_StopReasonThroughTimeoutLayer mirrors how the daemon
// builds its recording context: a cancel-cause layer for the stop sites
// wrapped in a timeout layer that labels the deadline. The sentinel has
// to survive that wrap -- context.Cause on the child reports the
// parent's cause -- or every hotkey release would log as max_duration.
func TestEngineRun_StopReasonThroughTimeoutLayer(t *testing.T) {
	t.Run("a stop site's cause survives the timeout wrap", func(t *testing.T) {
		base, stop := context.WithCancelCause(context.Background())
		recCtx, cancel := context.WithTimeoutCause(base, time.Hour, engine.ErrStopMaxDuration)
		defer cancel()
		stop(engine.ErrStopHotkeyRelease)

		got := runForStopReason(t, &mockRecorder{wavData: []byte("fake-wav")}, recCtx)
		require.Equal(t, []string{"hotkey_release"}, got)
	})

	t.Run("the deadline itself reports max duration", func(t *testing.T) {
		base, stop := context.WithCancelCause(context.Background())
		defer stop(nil)
		recCtx, cancel := context.WithTimeoutCause(base, time.Millisecond, engine.ErrStopMaxDuration)
		defer cancel()

		got := runForStopReason(t, &mockRecorder{wavData: []byte("fake-wav")}, recCtx)
		require.Equal(t, []string{"max_duration"}, got)
	})
}
