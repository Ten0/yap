// Package exec is a hint provider that runs a user-configured command
// and takes its stdout as conversation context.
//
// It exists because conversation state lives in too many places for yap
// to know them all: an editor's session database, a chat client's local
// cache, a remote machine reached over ssh. Rather than grow a provider
// per application, this provider hands the focused window's description
// to a helper program and accepts a small JSON answer back. The helper
// is where application knowledge lives; the provider stays generic.
//
// # The v1 contract
//
// Input travels one way, as environment variables. stdin is /dev/null.
// Every variable below is always set; an empty value means "unknown",
// never "absent", so a helper can tell the two apart:
//
//	YAP_HINT_API           contract version, currently "1". A helper
//	                       that does not recognize the value must answer
//	                       status=error instead of guessing.
//	YAP_HINT_WINDOW_ID     opaque, platform-specific window identifier,
//	                       passed through verbatim. Usually a pid, but
//	                       not always — see Fetch.
//	YAP_HINT_WINDOW_TITLE  focused window title; empty when unknown.
//	YAP_HINT_APP_TYPE      generic, terminal, electron or browser.
//	YAP_HINT_ROOT_PATH     project directory signal.
//	YAP_HINT_MAX_BYTES     conversation budget, so the helper can
//	                       truncate at the source instead of shipping
//	                       megabytes for yap to throw away.
//	YAP_HINT_DEADLINE_MS   milliseconds left before yap stops waiting.
//	                       Helpers should budget their own subprocess
//	                       timeouts from it and answer within it.
//
// The child environment is yap's own, minus every variable that carries
// a secret (see config.SecretEnvNames) and minus any pre-existing
// YAP_HINT_* variable, so a helper can trust that what it reads under
// that prefix came from yap and not from whatever launched the daemon.
//
// Output is exactly one JSON object on stdout:
//
//	{"schema":1,
//	 "status":"ok"|"no_match"|"ambiguous"|"degraded"|"error",
//	 "conversation":"user: ...\n\nassistant: ...",
//	 "reason":"one human sentence",
//	 "diagnostics":{...}}
//
// conversation uses the same "user: %s\n\n" / "assistant: %s\n\n" shape
// the other providers emit, so the transform prompt sees one format
// whatever produced it. diagnostics is opaque to yap: it is logged
// verbatim as a single attribute and never parsed, never part of a
// decision. That is the seam — anything application-specific belongs in
// there, which is what keeps this package generic.
//
// The helper exits 0 in every normal case, status=error included. A
// non-zero exit means the helper itself broke, and is reported as such.
//
// A helper may leave background processes behind. yap waits for the
// helper, not for its descendants: once the helper exits, whatever it
// spawned is given a brief grace period to stop writing and is then
// left to itself, and the answer already on stdout is used. What a
// helper must not do is exit while its answer is still unwritten —
// stdout is read as one complete JSON object, and half of one is an
// error like any other malformed reply.
package exec
