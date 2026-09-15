package fallback_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Enriquefft/yap/pkg/yap/transcribe"
	"github.com/Enriquefft/yap/pkg/yap/transform"
	"github.com/Enriquefft/yap/pkg/yap/transform/fallback"
)

// stubTransformer is a test double for transform.Transformer. It
// emits a fixed list of chunks (plus an optional terminal error
// chunk) or returns a factory error from Transform.
type stubTransformer struct {
	name        string
	emit        []transcribe.TranscriptChunk
	factoryErr  error
	calls       int32
	block       chan struct{}
	lastInput   []transcribe.TranscriptChunk
	captureDone chan struct{}
}

func (s *stubTransformer) Transform(ctx context.Context, in <-chan transcribe.TranscriptChunk, _ transform.Options) (<-chan transcribe.TranscriptChunk, error) {
	atomic.AddInt32(&s.calls, 1)
	// Drain and capture the input.
	var input []transcribe.TranscriptChunk
	for c := range in {
		input = append(input, c)
	}
	s.lastInput = input
	if s.captureDone != nil {
		close(s.captureDone)
		s.captureDone = nil
	}
	if s.factoryErr != nil {
		out := make(chan transcribe.TranscriptChunk)
		close(out)
		return out, s.factoryErr
	}
	out := make(chan transcribe.TranscriptChunk)
	go func() {
		defer close(out)
		for _, c := range s.emit {
			if s.block != nil {
				select {
				case <-ctx.Done():
					return
				case <-s.block:
				}
			}
			select {
			case <-ctx.Done():
				return
			case out <- c:
			}
		}
	}()
	return out, nil
}

func inputChunks(chunks ...transcribe.TranscriptChunk) <-chan transcribe.TranscriptChunk {
	ch := make(chan transcribe.TranscriptChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return ch
}

func drain(out <-chan transcribe.TranscriptChunk) []transcribe.TranscriptChunk {
	var got []transcribe.TranscriptChunk
	for c := range out {
		got = append(got, c)
	}
	return got
}

// echoTransformer is a passthrough-style stub that emits the inputs
// verbatim. It stands in for the fallback path in tests that want to
// see the buffered slice replayed.
type echoTransformer struct {
	calls int32
}

func (e *echoTransformer) Transform(ctx context.Context, in <-chan transcribe.TranscriptChunk, _ transform.Options) (<-chan transcribe.TranscriptChunk, error) {
	atomic.AddInt32(&e.calls, 1)
	out := make(chan transcribe.TranscriptChunk)
	go func() {
		defer close(out)
		for c := range in {
			select {
			case <-ctx.Done():
				return
			case out <- c:
			}
		}
	}()
	return out, nil
}

func TestNew_RejectsNil(t *testing.T) {
	if _, err := fallback.New(nil, &echoTransformer{}, nil); err == nil {
		t.Error("expected error on nil primary")
	}
	if _, err := fallback.New(&echoTransformer{}, nil, nil); err == nil {
		t.Error("expected error on nil fallback")
	}
}

func TestTransform_PrimarySuccess_NoFallback(t *testing.T) {
	primary := &stubTransformer{
		name: "primary",
		emit: []transcribe.TranscriptChunk{
			{Text: "hello"},
			{Text: " world", IsFinal: true},
		},
	}
	fb := &echoTransformer{}
	var onErrCalls int32
	fbt, _ := fallback.New(primary, fb, func(error) { atomic.AddInt32(&onErrCalls, 1) })

	out, err := fbt.Transform(context.Background(), inputChunks(
		transcribe.TranscriptChunk{Text: "raw"},
	), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := drain(out)
	if len(got) != 2 || got[0].Text != "hello" || got[1].Text != " world" {
		t.Errorf("got = %+v, want primary chunks", got)
	}
	if atomic.LoadInt32(&fb.calls) != 0 {
		t.Errorf("fallback calls = %d, want 0", atomic.LoadInt32(&fb.calls))
	}
	if atomic.LoadInt32(&onErrCalls) != 0 {
		t.Errorf("OnError calls = %d, want 0", atomic.LoadInt32(&onErrCalls))
	}
}

func TestTransform_PrimaryFactoryError_FallbackTakesOver(t *testing.T) {
	primary := &stubTransformer{factoryErr: errors.New("primary factory boom")}
	fb := &echoTransformer{}
	var onErrCalls int32
	var capturedErr error
	fbt, _ := fallback.New(primary, fb, func(err error) {
		atomic.AddInt32(&onErrCalls, 1)
		capturedErr = err
	})

	raw := transcribe.TranscriptChunk{Text: "raw", IsFinal: true}
	out, err := fbt.Transform(context.Background(), inputChunks(raw), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := drain(out)
	if len(got) != 1 || got[0].Text != "raw" || !got[0].IsFinal {
		t.Errorf("got = %+v, want buffered replay via fallback", got)
	}
	if atomic.LoadInt32(&fb.calls) != 1 {
		t.Errorf("fallback calls = %d, want 1", atomic.LoadInt32(&fb.calls))
	}
	if atomic.LoadInt32(&onErrCalls) != 1 {
		t.Errorf("OnError calls = %d, want 1", atomic.LoadInt32(&onErrCalls))
	}
	if capturedErr == nil || capturedErr.Error() != "primary factory boom" {
		t.Errorf("captured err = %v", capturedErr)
	}
}

func TestTransform_PrimaryErrorChunk_FallbackTakesOver(t *testing.T) {
	primary := &stubTransformer{
		emit: []transcribe.TranscriptChunk{
			// A partially-transformed chunk followed by a terminal
			// error. The fallback must REPLACE the partial output
			// entirely — we never mix transformed + raw.
			{Text: "partial"},
			{IsFinal: true, Err: errors.New("mid-stream boom")},
		},
	}
	fb := &echoTransformer{}
	var onErrCalls int32
	fbt, _ := fallback.New(primary, fb, func(error) { atomic.AddInt32(&onErrCalls, 1) })

	raw1 := transcribe.TranscriptChunk{Text: "raw-a"}
	raw2 := transcribe.TranscriptChunk{Text: "raw-b", IsFinal: true}
	out, err := fbt.Transform(context.Background(), inputChunks(raw1, raw2), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := drain(out)
	if len(got) != 2 || got[0].Text != "raw-a" || got[1].Text != "raw-b" {
		t.Errorf("got = %+v, want exactly the buffered raw inputs (no partial 'partial')", got)
	}
	if atomic.LoadInt32(&fb.calls) != 1 {
		t.Errorf("fallback calls = %d, want 1", atomic.LoadInt32(&fb.calls))
	}
	if atomic.LoadInt32(&onErrCalls) != 1 {
		t.Errorf("OnError calls = %d, want 1", atomic.LoadInt32(&onErrCalls))
	}
}

func TestTransform_UpstreamError_NeitherRuns(t *testing.T) {
	primary := &stubTransformer{}
	fb := &echoTransformer{}
	var onErrCalls int32
	fbt, _ := fallback.New(primary, fb, func(error) { atomic.AddInt32(&onErrCalls, 1) })

	sentinel := errors.New("transcribe boom")
	out, err := fbt.Transform(context.Background(), inputChunks(
		transcribe.TranscriptChunk{Err: sentinel, IsFinal: true},
	), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := drain(out)
	if len(got) != 1 || !errors.Is(got[0].Err, sentinel) {
		t.Errorf("got = %+v, want single upstream-error chunk", got)
	}
	if atomic.LoadInt32(&primary.calls) != 0 {
		t.Errorf("primary calls = %d, want 0", atomic.LoadInt32(&primary.calls))
	}
	if atomic.LoadInt32(&fb.calls) != 0 {
		t.Errorf("fallback calls = %d, want 0", atomic.LoadInt32(&fb.calls))
	}
	if atomic.LoadInt32(&onErrCalls) != 0 {
		t.Errorf("OnError calls = %d, want 0", atomic.LoadInt32(&onErrCalls))
	}
}

func TestTransform_OnErrorCalledExactlyOnce(t *testing.T) {
	// Two ways to fail: factory error AND error chunk. Each path
	// must fire OnError exactly once.
	primary := &stubTransformer{
		emit: []transcribe.TranscriptChunk{
			{IsFinal: true, Err: errors.New("boom")},
		},
	}
	fb := &echoTransformer{}
	var onErrCalls int32
	fbt, _ := fallback.New(primary, fb, func(error) { atomic.AddInt32(&onErrCalls, 1) })

	raw := transcribe.TranscriptChunk{Text: "raw"}
	out, err := fbt.Transform(context.Background(), inputChunks(raw), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	drain(out)
	if got := atomic.LoadInt32(&onErrCalls); got != 1 {
		t.Errorf("OnError calls = %d, want 1", got)
	}
}

func TestTransform_CancelledBeforePrimary_ReturnsErr(t *testing.T) {
	primary := &stubTransformer{}
	fb := &echoTransformer{}
	fbt, _ := fallback.New(primary, fb, nil)

	ctx, cancel := context.WithCancel(context.Background())

	// Build an input channel that blocks forever so drain loops.
	in := make(chan transcribe.TranscriptChunk)

	done := make(chan error, 1)
	go func() {
		_, err := fbt.Transform(ctx, in, transform.Options{})
		done <- err
	}()
	// Let drain spin once.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Transform did not return after cancel")
	}
	if atomic.LoadInt32(&primary.calls) != 0 {
		t.Errorf("primary calls = %d, want 0", atomic.LoadInt32(&primary.calls))
	}
	if atomic.LoadInt32(&fb.calls) != 0 {
		t.Errorf("fallback calls = %d, want 0", atomic.LoadInt32(&fb.calls))
	}
}

func TestTransform_NilOnError_SilentlyRetries(t *testing.T) {
	primary := &stubTransformer{factoryErr: errors.New("boom")}
	fb := &echoTransformer{}
	fbt, _ := fallback.New(primary, fb, nil)

	out, err := fbt.Transform(context.Background(), inputChunks(
		transcribe.TranscriptChunk{Text: "raw"},
	), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := drain(out)
	if len(got) != 1 || got[0].Text != "raw" {
		t.Errorf("got = %+v, want buffered raw", got)
	}
}

// TestTransform_CancelledDuringPrimary_NoFallback asserts that
// cancelling the context while the primary is mid-stream closes the
// output without invoking the fallback. The streaming contract says
// a closed channel on a cancelled context is the terminal signal —
// callers observe ctx.Err themselves, they do not see an error chunk
// from the fallback.
func TestTransform_CancelledDuringPrimary_NoFallback(t *testing.T) {
	unblock := make(chan struct{})
	primary := &stubTransformer{
		emit: []transcribe.TranscriptChunk{
			{Text: "first"},
			{Text: "second"}, // blocked until unblock closes
		},
		block: unblock,
	}
	fb := &echoTransformer{}
	fbt, _ := fallback.New(primary, fb, nil)

	ctx, cancel := context.WithCancel(context.Background())
	out, err := fbt.Transform(ctx, inputChunks(transcribe.TranscriptChunk{Text: "raw"}), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	// Let the first chunk land in the staged buffer (it will be
	// blocked by stubTransformer on the second send) then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(unblock)

	done := make(chan struct{})
	go func() {
		for range out {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("output not closed after ctx cancel")
	}
	if got := atomic.LoadInt32(&fb.calls); got != 0 {
		t.Errorf("fallback calls = %d, want 0 (ctx cancel must not trigger fallback)", got)
	}
}

// TestTransform_ImplausibleExpansion_FallsBackToRaw covers the failure
// the guard exists for: the primary succeeded, emitted no error, and
// answered its prompt instead of repairing the transcript. The output
// is split across two chunks so the check is also shown to measure the
// whole staged stream rather than the last chunk.
func TestTransform_ImplausibleExpansion_FallsBackToRaw(t *testing.T) {
	primary := &stubTransformer{
		emit: []transcribe.TranscriptChunk{
			{Text: "I'm ready to repair speech-to-text transcripts. "},
			{Text: "Please provide the transcript you'd like me to fix.", IsFinal: true},
		},
	}
	fb := &echoTransformer{}
	var onErrCalls int32
	var captured error
	fbt, _ := fallback.New(primary, fb, func(err error) {
		atomic.AddInt32(&onErrCalls, 1)
		captured = err
	})

	raw := transcribe.TranscriptChunk{Text: " speech to text.", IsFinal: true}
	out, err := fbt.Transform(context.Background(), inputChunks(raw), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := drain(out)
	if len(got) != 1 || got[0].Text != " speech to text." {
		t.Errorf("got = %+v, want the raw transcript replayed, not the primary's reply", got)
	}
	if n := atomic.LoadInt32(&fb.calls); n != 1 {
		t.Errorf("fallback calls = %d, want 1", n)
	}
	if n := atomic.LoadInt32(&onErrCalls); n != 1 {
		t.Errorf("OnError calls = %d, want 1", n)
	}
	if !errors.Is(captured, fallback.ErrImplausibleExpansion) {
		t.Errorf("captured err = %v, want ErrImplausibleExpansion", captured)
	}
}

// TestTransform_GenuineRepair_NotRejected pins the other side of the
// threshold. A real repair tracks its input closely — it trims and
// punctuates, so it tends to shrink — and must reach the caller
// untouched with the fallback never invoked.
func TestTransform_GenuineRepair_NotRejected(t *testing.T) {
	primary := &stubTransformer{
		emit: []transcribe.TranscriptChunk{
			{Text: "Speech to text.", IsFinal: true},
		},
	}
	fb := &echoTransformer{}
	var onErrCalls int32
	fbt, _ := fallback.New(primary, fb, func(error) { atomic.AddInt32(&onErrCalls, 1) })

	raw := transcribe.TranscriptChunk{Text: " speech to text.\n", IsFinal: true}
	out, err := fbt.Transform(context.Background(), inputChunks(raw), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := drain(out)
	if len(got) != 1 || got[0].Text != "Speech to text." {
		t.Errorf("got = %+v, want the primary's repair", got)
	}
	if n := atomic.LoadInt32(&fb.calls); n != 0 {
		t.Errorf("fallback calls = %d, want 0", n)
	}
	if n := atomic.LoadInt32(&onErrCalls); n != 0 {
		t.Errorf("OnError calls = %d, want 0", n)
	}
}

// TestTransform_ShortInputTolerance covers the floor. Two words gain
// a capital and a full stop and blow past 2x on ratio alone, which is
// why the threshold has an absolute floor as well — but a tiny input
// answered with a paragraph must still be caught.
func TestTransform_ShortInputTolerance(t *testing.T) {
	cases := []struct {
		name         string
		in, out      string
		wantFallback bool
	}{
		{
			name: "punctuating two words is not an expansion",
			in:   "ok then", out: "OK, then.",
			wantFallback: false,
		},
		{
			name: "a paragraph answering a fragment still trips",
			in:   "ok then",
			out: "Certainly! I can help you with that. Could you tell me a " +
				"little more about what you would like me to do next?",
			wantFallback: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primary := &stubTransformer{
				emit: []transcribe.TranscriptChunk{{Text: tc.out, IsFinal: true}},
			}
			fb := &echoTransformer{}
			fbt, _ := fallback.New(primary, fb, nil)

			out, err := fbt.Transform(context.Background(), inputChunks(
				transcribe.TranscriptChunk{Text: tc.in, IsFinal: true},
			), transform.Options{})
			if err != nil {
				t.Fatalf("Transform: %v", err)
			}
			got := drain(out)
			if len(got) != 1 {
				t.Fatalf("got %d chunks, want 1: %+v", len(got), got)
			}
			want := tc.out
			if tc.wantFallback {
				want = tc.in
			}
			if got[0].Text != want {
				t.Errorf("text = %q, want %q", got[0].Text, want)
			}
			wantCalls := int32(0)
			if tc.wantFallback {
				wantCalls = 1
			}
			if n := atomic.LoadInt32(&fb.calls); n != wantCalls {
				t.Errorf("fallback calls = %d, want %d", n, wantCalls)
			}
		})
	}
}

// TestTransform_ExpansionError_CarriesDroppedText pins the payload the
// daemon logs. The rejected text exists nowhere else once the fallback
// has replaced it, so OnError is the only chance to record what the
// model said instead of repairing — and the error must still classify
// as ErrImplausibleExpansion for callers that only want the category.
func TestTransform_ExpansionError_CarriesDroppedText(t *testing.T) {
	const partA = "I'm ready to repair speech-to-text transcripts. "
	const partB = "Please provide the transcript you'd like me to fix."
	primary := &stubTransformer{
		emit: []transcribe.TranscriptChunk{
			{Text: partA},
			{Text: partB, IsFinal: true},
		},
	}
	fb := &echoTransformer{}
	var captured error
	fbt, _ := fallback.New(primary, fb, func(err error) { captured = err })

	raw := " speech to text."
	out, err := fbt.Transform(context.Background(), inputChunks(
		transcribe.TranscriptChunk{Text: raw, IsFinal: true},
	), transform.Options{})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	drain(out)

	if !errors.Is(captured, fallback.ErrImplausibleExpansion) {
		t.Fatalf("errors.Is(ErrImplausibleExpansion) = false for %v", captured)
	}
	var ee *fallback.ExpansionError
	if !errors.As(captured, &ee) {
		t.Fatalf("errors.As(*ExpansionError) = false for %v", captured)
	}
	if want := partA + partB; ee.Dropped != want {
		t.Errorf("Dropped = %q, want the whole staged output %q", ee.Dropped, want)
	}
	if want := len([]rune(raw)); ee.In != want {
		t.Errorf("In = %d, want %d", ee.In, want)
	}
	if want := len([]rune(partA + partB)); ee.Out != want {
		t.Errorf("Out = %d, want %d", ee.Out, want)
	}
}

// Ensure the decorator still satisfies transform.Transformer.
func TestTransform_InterfaceSatisfied(t *testing.T) {
	var _ transform.Transformer = (*fallback.Transformer)(nil)
}
