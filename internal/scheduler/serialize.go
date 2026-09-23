package scheduler

// SerializeOverlapFromEnv reports whether TTORCH_SERIALIZE_OVERLAP has switched dispatch back to
// serialize-on-overlap. It is the same reading the daemon's dispatch pass uses, exported so a
// dispatch started elsewhere (the board's dispatch action) follows the daemon's overlap policy
// instead of choosing its own.
func SerializeOverlapFromEnv() bool { return serializeOverlapFromEnv() }
