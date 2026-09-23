// Package herdr is a client for Herdr's socket API. Herdr is a separate
// server that owns coding-agent terminals so they survive client
// disconnects and restarts; it speaks newline-delimited JSON over a Unix
// socket, one {id, method, params} request per line, each answered by a
// line echoing the id. This package is only a client of that protocol. It
// targets Herdr v0.9.1 (protocol 22) and covers the subset needed to host
// ttorch workers; nothing in the orchestrator uses it yet.
//
// How each call would map onto a worker session:
//
//   - The session itself. A Herdr session is a running server with its own
//     socket, started by the herdr binary (optionally with --session name),
//     not something the socket API creates. A worker backend would own one
//     named session per ttorch instance and pass the path SessionSocketPath
//     returns for it to New; Ping confirms it is up and reports the version
//     and protocol.
//   - Spawn. CreateWorkspace with Cwd set to the worker's worktree, Label to
//     the task id and Env to the worker's environment gives the worker its
//     own workspace and root pane; CreateTab or SplitPane place further
//     workers in an existing workspace instead. The returned PaneInfo.PaneID
//     is the handle the backend stores, the way it stores a tmux window
//     today.
//   - Launch and brief. Neither create call takes a command, so the backend
//     types one into the pane. That line is fixed and built by ttorch alone
//     (the agent binary and its flags, never text taken from a task or
//     brief), sent with SendLiteral(pane, line, "enter"). The brief itself
//     is not typed: it is written to a file in the worktree that the agent
//     is told to read. A short nudge that must be typed also goes through
//     SendLiteral, which refuses CR, LF, ESC and other control characters
//     and sends the text and its enter in one request so another writer
//     cannot land between them. SendText, SendInput and SendKeys pass input
//     through unfiltered and are for input ttorch fully controls.
//   - Peek. ReadPane with ReadRecent (or ReadRecentUnwrapped for logs)
//     returns the scrollback the manager shows as a worker's recent output.
//   - Liveness and state. Agent returns the pane's semantic status (idle,
//     working, blocked, done, unknown) as Herdr's own detection sees it, in
//     place of inferring state from pane text.
//   - Waiting. WaitAgent(pane, timeout, StatusDone, StatusBlocked) blocks in
//     the server until the agent finishes or needs input. Herdr pins the
//     pane's occupant for the wait, so a replacement agent cannot satisfy
//     it; its timeout surfaces as ErrWaitTimeout.
//   - Watching. Subscribe with AgentStatusChanges for each worker pane, plus
//     EventFilter{Type: "pane.exited"} or "pane.closed", gives the watcher a
//     pushed stream instead of polling.
//   - Teardown. ClosePane ends the worker's pane and its process.
//
// Before dialling, every call checks that the socket is a real socket owned
// by the current user in a directory that user owns and no one else can
// write, and refuses it with an *UnsafeSocketError otherwise, so a socket
// another local user planted never receives a request.
//
// Every request/response call is bounded by the earlier of the context's
// deadline and Client.Timeout; WaitAgent is bounded by its own timeout plus
// WaitGrace. Failures are typed: ErrNoSocket, ErrUnsafeSocket,
// ErrNoConfigDir, ErrUnknownMethod, ErrMismatchedID, ErrMalformedResponse,
// ErrConnectionClosed, ErrWaitTimeout, and *APIError for any other error
// Herdr returns.
package herdr
