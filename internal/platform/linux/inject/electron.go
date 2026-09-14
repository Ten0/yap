package inject

import (
	"context"
	"fmt"
	"time"

	"github.com/Enriquefft/yap/internal/platform"
	yinject "github.com/Enriquefft/yap/pkg/yap/inject"
)

// electronStrategy delivers text to Electron and browser apps via the
// clipboard + synthesized Ctrl+V path. Most Monaco-style autocomplete
// editors swallow synthetic typing but accept clipboard pastes
// reliably, which is why this dedicated strategy exists.
//
// The flow is: save current clipboard → write text → synthesize
// Ctrl+V via wtype/xdotool → wait briefly → restore the previous
// clipboard. The restore wait routes through Deps.SleepCtx so tests
// have a single hook for time control and ctx cancellation unblocks
// the wait promptly. The wait is the only bounded delay permitted in
// the inject package, and it is documented in the Phase 4 plan §1.8.
type electronStrategy struct {
	deps Deps
	opts platform.InjectionOptions
}

// electronRestoreDelay is the bounded wait between writing the new
// clipboard contents and restoring the saved value. Picked at 50ms
// because synthesized Ctrl+V is processed by the focused app within
// a few milliseconds; 50ms gives even slow Electron apps time to
// observe the clipboard before we put the original back.
const electronRestoreDelay = 50 * time.Millisecond

// newElectronStrategy constructs an electron strategy bound to deps
// and opts. The opts argument carries the ElectronStrategy field —
// when it is not "clipboard", Supports returns false so the
// orchestrator falls through to the wayland/x11 strategies.
func newElectronStrategy(deps Deps, opts platform.InjectionOptions) *electronStrategy {
	return &electronStrategy{deps: deps, opts: opts}
}

// Name returns the strategy identifier used in audit logs and
// app_overrides lookups.
func (s *electronStrategy) Name() string { return "electron" }

// Supports returns true for Electron and browser targets when the
// configured ElectronStrategy is "clipboard". The "keystroke"
// alternative is served by the wayland/x11 generic strategies, so
// this strategy declines those targets explicitly.
func (s *electronStrategy) Supports(target yinject.Target) bool {
	if s.opts.ElectronStrategy != "" && s.opts.ElectronStrategy != "clipboard" {
		return false
	}
	return target.AppType == yinject.AppElectron ||
		target.AppType == yinject.AppBrowser ||
		target.AppType == yinject.AppTerminal
}

// pasteChord is the key combination that pastes the clipboard in the
// target. Ctrl+V is the desktop convention, but a terminal passes it
// through to the program it is running -- readline reads it as
// quoted-insert, vim as visual block -- so terminals paste with
// Ctrl+Shift+V instead.
func pasteChord(target yinject.Target) (xdotool string, wtypeArgs []string) {
	if target.AppType == yinject.AppTerminal {
		return "ctrl+shift+v", []string{"-M", "ctrl", "-M", "shift", "v", "-m", "shift", "-m", "ctrl"}
	}
	return "ctrl+v", []string{"-M", "ctrl", "v", "-m", "ctrl"}
}

// Deliver writes text to the clipboard, synthesizes a paste keystroke
// via the appropriate display-server tool, then restores the original
// clipboard contents after a bounded wait. Returns an error if the
// clipboard write or the paste synthesis fails.
func (s *electronStrategy) Deliver(ctx context.Context, target yinject.Target, text string) error {
	saved, saveErr := s.deps.ClipboardRead()
	if err := s.deps.ClipboardWrite(text); err != nil {
		return fmt.Errorf("electron: clipboard write: %w", err)
	}
	var pasteErr error
	switch target.DisplayServer {
	case "wayland":
		pasteErr = s.synthesizePasteWayland(ctx, target)
	case "x11":
		pasteErr = s.synthesizePasteX11(ctx, target)
	default:
		pasteErr = yinject.ErrStrategyUnsupported
	}
	if pasteErr != nil {
		// Restore the clipboard before bubbling so we don't leave
		// the user with stale paste content on a failed delivery.
		if saveErr == nil {
			_ = s.deps.ClipboardWrite(saved)
		}
		return fmt.Errorf("electron: synthesize paste: %w", pasteErr)
	}
	if saveErr == nil {
		// SleepCtx returns ctx.Err() on cancellation. We proceed with
		// the restore regardless so the clipboard is always returned
		// to its saved state — the alternative (skipping restore on
		// cancel) would leave the user with stale paste content.
		_ = s.deps.SleepCtx(ctx, electronRestoreDelay)
		_ = s.deps.ClipboardWrite(saved)
	}
	return nil
}

// synthesizePasteWayland sends the target's paste chord via wtype. The
// strategy uses the pressed/released modifier syntax wtype expects.
func (s *electronStrategy) synthesizePasteWayland(ctx context.Context, target yinject.Target) error {
	if _, err := s.deps.LookPath("wtype"); err != nil {
		return yinject.ErrStrategyUnsupported
	}
	_, args := pasteChord(target)
	cmd := s.deps.ExecCommandContext(ctx, "wtype", args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wtype paste: %w", err)
	}
	return nil
}

// synthesizePasteX11 sends the target's paste chord via xdotool.
func (s *electronStrategy) synthesizePasteX11(ctx context.Context, target yinject.Target) error {
	if _, err := s.deps.LookPath("xdotool"); err != nil {
		return yinject.ErrStrategyUnsupported
	}
	chord, _ := pasteChord(target)
	cmd := s.deps.ExecCommandContext(ctx, "xdotool", "key", "--clearmodifiers", chord)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("xdotool paste: %w", err)
	}
	return nil
}
