package fallback

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Enriquefft/yap/pkg/yap/transcribe"
	"github.com/Enriquefft/yap/pkg/yap/transform"
)

// expansionError is a string-backed error type so the sentinel below
// can be a const. A package-level var would trip this package's own
// noglobals guard, and the invariant it protects — all state lives on
// the Transformer — is worth keeping.
type expansionError string

func (e expansionError) Error() string { return string(e) }

// ErrImplausibleExpansion is handed to OnError when the primary
// returned far more text than it was given. Callers match it with
// errors.Is to distinguish "the backend failed" from "the backend
// answered instead of transforming".
const ErrImplausibleExpansion = expansionError(
	"transform: output far larger than input; treated as a reply, not a repair")

// ExpansionError is the error OnError actually receives when output is
// rejected. It carries the text that was thrown away, because that text
// is the whole evidence of what went wrong: a caller looking at a
// journal afterwards wants to read what the model said instead of
// repairing, and by then it exists nowhere else.
//
// It unwraps to ErrImplausibleExpansion, so callers that only want to
// classify the failure keep using errors.Is and never see this type.
type ExpansionError struct {
	// Dropped is the primary's output, discarded in favour of the raw
	// input. Callers that log it should treat it as they would a
	// transcript: it is the user's dictation refracted through a model.
	Dropped string
	// In and Out are the rune counts that failed the check, so a log
	// line can carry the shape of the failure without the text.
	In, Out int
}

func (e *ExpansionError) Error() string { return ErrImplausibleExpansion.Error() }

func (e *ExpansionError) Unwrap() error { return ErrImplausibleExpansion }

// Transformer is a transform.Transformer decorator that runs Primary
// first and falls back to Fallback on failure. See the package doc
// for the exact semantics.
//
// Both fields are required; New is the preferred constructor. OnError
// is optional — nil means "failures are still retried through the
// fallback, but no user-visible notification is raised".
type Transformer struct {
	Primary  transform.Transformer
	Fallback transform.Transformer
	OnError  func(error)
}

// New constructs a Transformer and validates that both the primary
// and fallback transformers are non-nil. Passing a nil OnError is
// allowed — the caller can wire a notification later.
func New(primary, fallback transform.Transformer, onError func(error)) (*Transformer, error) {
	if primary == nil {
		return nil, errors.New("fallback: primary transformer is required")
	}
	if fallback == nil {
		return nil, errors.New("fallback: fallback transformer is required")
	}
	return &Transformer{Primary: primary, Fallback: fallback, OnError: onError}, nil
}

// Transform drains the input into a slice, runs it through Primary,
// and on primary failure replays the buffered slice through
// Fallback. opts is threaded to both the primary and the fallback so
// the context reference block reaches whichever stage runs.
func (t *Transformer) Transform(ctx context.Context, in <-chan transcribe.TranscriptChunk, opts transform.Options) (<-chan transcribe.TranscriptChunk, error) {
	buffered, upstream, ok := drain(ctx, in)
	if !ok {
		// Context cancelled while draining the input.
		out := make(chan transcribe.TranscriptChunk)
		close(out)
		return out, ctx.Err()
	}
	if upstream != nil {
		// Upstream error: propagate directly without running either
		// transformer. Transcription failures are not a transform
		// fallback concern.
		out := make(chan transcribe.TranscriptChunk, 1)
		out <- *upstream
		close(out)
		return out, nil
	}

	// Try the primary.
	primaryOut, err := t.Primary.Transform(ctx, replay(buffered), opts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return primaryOut, err
		}
		t.notify(err)
		return t.Fallback.Transform(ctx, replay(buffered), opts)
	}

	out := make(chan transcribe.TranscriptChunk)
	go func() {
		defer close(out)
		t.forwardPrimary(ctx, primaryOut, out, buffered, opts)
	}()
	return out, nil
}

// forwardPrimary copies the primary stream through to out. On the
// primary's error chunk, OnError fires and the buffered input is
// replayed through the fallback.
//
// Primary emission is staged into a slice and replayed only after the
// primary stream terminates cleanly. That way a primary error chunk
// mid-stream triggers a clean fallback instead of a half-transformed,
// half-raw output. The trade-off is that no primary chunk is emitted
// downstream until the primary completes successfully — see the
// package doc for the rationale and the daemon-side stream_partials
// escape hatch for callers that want streaming over recovery.
func (t *Transformer) forwardPrimary(
	ctx context.Context,
	primaryOut <-chan transcribe.TranscriptChunk,
	out chan<- transcribe.TranscriptChunk,
	buffered []transcribe.TranscriptChunk,
	opts transform.Options,
) {
	var staged []transcribe.TranscriptChunk
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, open := <-primaryOut:
			if !open {
				// Primary finished without error. Before committing
				// its output, check it is plausibly a repair of the
				// input rather than a reply to it.
				inLen, outLen := runeLen(buffered), runeLen(staged)
				if implausibleExpansion(inLen, outLen) {
					t.runFallback(ctx, &ExpansionError{
						Dropped: chunkText(staged),
						In:      inLen,
						Out:     outLen,
					}, buffered, out, opts)
					return
				}
				// Drain staged chunks to the caller.
				for _, c := range staged {
					select {
					case <-ctx.Done():
						return
					case out <- c:
					}
				}
				return
			}
			if chunk.Err != nil {
				// Primary failed mid-stream. Drain the rest of
				// primaryOut so its goroutine can terminate cleanly,
				// then run the fallback. Partial output is not mixed
				// with fallback output.
				drainRemaining(ctx, primaryOut)
				t.runFallback(ctx, chunk.Err, buffered, out, opts)
				return
			}
			staged = append(staged, chunk)
		}
	}
}

// runFallback notifies of the primary failure, invokes the fallback
// transformer with a fresh replay of the buffered input, and forwards
// the fallback's output to out. On a fallback factory error a single
// terminal error chunk is emitted instead so the consumer always sees
// the fallback's verdict.
func (t *Transformer) runFallback(
	ctx context.Context,
	primaryErr error,
	buffered []transcribe.TranscriptChunk,
	out chan<- transcribe.TranscriptChunk,
	opts transform.Options,
) {
	t.notify(primaryErr)
	fbOut, err := t.Fallback.Transform(ctx, replay(buffered), opts)
	if err != nil {
		select {
		case <-ctx.Done():
		case out <- transcribe.TranscriptChunk{IsFinal: true, Err: err}:
		}
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, open := <-fbOut:
			if !open {
				return
			}
			select {
			case <-ctx.Done():
				return
			case out <- chunk:
			}
		}
	}
}

// chunkText joins a chunk slice back into the string it represents.
// Only reached on the rejection path, so the allocation is paid once
// per failure rather than once per recording.
func chunkText(chunks []transcribe.TranscriptChunk) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// runeLen totals the text length of a chunk slice. Runes rather than
// bytes: the comparison is about how much was said, and a transcript
// in a non-ASCII language would otherwise look inflated against its
// own repair.
func runeLen(chunks []transcribe.TranscriptChunk) int {
	n := 0
	for _, c := range chunks {
		n += utf8.RuneCountInString(c.Text)
	}
	return n
}

// implausibleExpansion reports whether the primary's output is too
// much larger than its input to be a repair of it.
//
// A transform corrects wording, punctuation and capitalisation, so its
// output tracks its input closely. Measured over real dictation, every
// genuine repair landed between 0.75x and 0.99x of its input, while the
// two observed failures — a model continuing the conversation it had
// been given as reference context, and a model replying "I'm ready to
// repair transcripts…" to a short fragment — came in at 5.8x and 7.0x.
// Nothing legitimate was observed in between, so this threshold sits in
// an empty band rather than on a judgement call.
//
// The +40 floor keeps very short transcripts out of it: capitalising
// and punctuating a couple of words can legitimately exceed 2x when the
// input is only a handful of runes.
func implausibleExpansion(in, out int) bool {
	limit := 2 * in
	if floor := in + 40; floor > limit {
		limit = floor
	}
	return out > limit
}

// drainRemaining consumes whatever is still sitting in the primary
// channel so the backend goroutine can exit. Errors are intentionally
// discarded — we already committed to fallback.
func drainRemaining(ctx context.Context, ch <-chan transcribe.TranscriptChunk) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-ch:
			if !open {
				return
			}
		}
	}
}

// notify calls OnError if set. Swallowing a nil OnError keeps the
// decorator usable as a pure "always retry through fallback"
// wrapper.
func (t *Transformer) notify(err error) {
	if t.OnError != nil {
		t.OnError(err)
	}
}

// drain reads the full input channel into a slice. The second return
// is a non-nil *chunk when an upstream error chunk was observed
// (indicating the caller should propagate it rather than running
// either transformer). The third return is false when ctx was
// cancelled while draining.
func drain(ctx context.Context, in <-chan transcribe.TranscriptChunk) ([]transcribe.TranscriptChunk, *transcribe.TranscriptChunk, bool) {
	var buffered []transcribe.TranscriptChunk
	for {
		select {
		case <-ctx.Done():
			return nil, nil, false
		case chunk, open := <-in:
			if !open {
				return buffered, nil, true
			}
			if chunk.Err != nil {
				c := chunk
				return buffered, &c, true
			}
			buffered = append(buffered, chunk)
		}
	}
}

// replay turns a buffered slice back into a closed channel. The
// returned channel is fully drained on read — no goroutine spin.
func replay(buffered []transcribe.TranscriptChunk) <-chan transcribe.TranscriptChunk {
	ch := make(chan transcribe.TranscriptChunk, len(buffered))
	for _, c := range buffered {
		ch <- c
	}
	close(ch)
	return ch
}
