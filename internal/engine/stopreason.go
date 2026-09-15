package engine

import (
	"context"
	"errors"
)

// A recording ends one of seven ways: the user released the hotkey,
// toggled it off, stopped talking, ran out the max_duration budget, the
// recorder failed, the daemon went down, or `yap record` was
// interrupted. Six of those cancel the same context, so by the time the
// engine sees them they are indistinguishable -- every one arrives as a
// bare context.Canceled.
//
// The sentinels below are what tells them apart. A caller cancels the
// recording context with a context.CancelCauseFunc (or labels its
// deadline with context.WithTimeoutCause) and passes the sentinel
// naming the stop; [Engine.Run] reads it back with context.Cause and
// logs the reason once, right after the recorder returns.
var (
	// ErrStopHotkeyRelease is hold mode: the user let go of the key.
	ErrStopHotkeyRelease = errors.New("engine: recording stopped by hotkey release")
	// ErrStopToggle is toggle mode, or `yap toggle` over IPC/SIGUSR1.
	ErrStopToggle = errors.New("engine: recording stopped by toggle")
	// ErrStopSilence is the silence detector deciding the user is done.
	ErrStopSilence = errors.New("engine: recording stopped by silence")
	// ErrStopMaxDuration is the general.max_duration deadline.
	ErrStopMaxDuration = errors.New("engine: recording stopped by max duration")
	// ErrStopDaemonShutdown is the daemon's own context going down and
	// taking the in-flight recording with it: SIGINT, SIGTERM, or
	// `yap stop` over IPC.
	ErrStopDaemonShutdown = errors.New("engine: recording stopped by daemon shutdown")
	// ErrStopInterrupt is `yap record` being signalled to go down --
	// Ctrl-C in the terminal it is running in. It is deliberately not
	// ErrStopDaemonShutdown: that process has no daemon in it, and
	// naming one would send whoever read the line looking for a
	// crashed service that was never running.
	ErrStopInterrupt = errors.New("engine: recording stopped by interrupt")
)

// Reason strings for the "recording stopped" log line. These are the
// stable vocabulary operators grep and filter on, so they are
// snake_case tokens rather than the sentinels' prose.
const (
	reasonHotkeyRelease  = "hotkey_release"
	reasonToggle         = "toggle"
	reasonSilence        = "silence"
	reasonMaxDuration    = "max_duration"
	reasonDaemonShutdown = "daemon_shutdown"
	reasonInterrupt      = "interrupt"
	reasonRecorderError  = "recorder_error"
	reasonUnknown        = "unknown"
)

// stopReason names why a recording ended, from what Recorder.Start
// returned and the recording context the caller owns.
//
// A recErr that is neither Canceled nor DeadlineExceeded means the
// recorder itself broke -- the audio device, not the user -- so it
// outranks whatever the context says. Every other stop is read back
// from the cause the caller attached when it cancelled.
//
// An unlabelled deadline is still the max_duration budget: it is the
// only deadline a recording context ever carries, so reading it that
// way identifies the stop rather than guessing at it.
//
// Everything else unlabelled is "unknown", and deliberately so. A bare
// context.Canceled says only that some ancestor was cancelled -- in the
// daemon that would be the shutdown, but `yap record` is a process with
// no daemon in it at all, and there Ctrl-C would have been reported as
// a daemon going down. Both stop sites now attach a cause, so a bare
// cancellation means nobody labelled the stop, and "unknown" says that
// instead of naming a plausible-looking reason that was never checked.
// The same goes for a recorder that returned on its own, with no error
// and nothing cancelled.
func stopReason(recCtx context.Context, recErr error) string {
	if recErr != nil &&
		!errors.Is(recErr, context.Canceled) &&
		!errors.Is(recErr, context.DeadlineExceeded) {
		return reasonRecorderError
	}
	switch cause := context.Cause(recCtx); {
	case errors.Is(cause, ErrStopHotkeyRelease):
		return reasonHotkeyRelease
	case errors.Is(cause, ErrStopToggle):
		return reasonToggle
	case errors.Is(cause, ErrStopSilence):
		return reasonSilence
	case errors.Is(cause, ErrStopMaxDuration):
		return reasonMaxDuration
	case errors.Is(cause, ErrStopDaemonShutdown):
		return reasonDaemonShutdown
	case errors.Is(cause, ErrStopInterrupt):
		return reasonInterrupt
	case errors.Is(cause, context.DeadlineExceeded):
		return reasonMaxDuration
	default:
		return reasonUnknown
	}
}
