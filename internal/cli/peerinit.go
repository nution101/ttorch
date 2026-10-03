package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/peer"
)

// fleetPrograms are what a peer's fleet runs. init reports the ones peer.env's PATH does not
// reach, since the control channel runs with that PATH and nothing else.
var fleetPrograms = []string{"tmux", "claude", "git"}

// cmdPeerInit provisions this machine as a peer of the parent named in the request on stdin, and
// answers one response on stdout (internal/peer/init.go). `ttorch peer add` on the parent runs it
// over the lead's own ssh session.
func cmdPeerInit(stdin io.Reader, stdout, stderr io.Writer) int {
	res, err := peerInit(context.Background(), stdin)
	return peer.Respond(stdout, stderr, peer.VerbInit, res, err)
}

// peerInit does init's work, in an order that admits the control key last:
//
//  1. Refuse a worker context, then read and check the whole request before touching anything.
//  2. Find the account the way the control channel does (peerAccount), so init writes the store,
//     peer.env and authorized_keys that `peer serve` will read, whatever TTORCH_HOME or TTORCH_DB
//     this session has. The ttorch home is created 0700 if missing and must otherwise be private.
//  3. Read an existing peer.env with the channel's own reader, so one the channel would refuse
//     fails here, at the lead's terminal, and not on every later call.
//  4. Record the parent on the coordinator row (db.ProvisionAsPeer). Another parent is refused
//     unless the request forces it.
//  5. Write peer.env if there is none: the default PATH plus this session's, and the parent's
//     settings. An existing one is kept as it is.
//  6. Install the control key's forced-command line in ~/.ssh/authorized_keys
//     (peer.ProvisionControlKey).
//
// Every step can run again: a retried add finds the row, peer.env and the line already there.
func peerInit(ctx context.Context, stdin io.Reader) (peer.InitResult, error) {
	if signal := workerContextSignal(); signal != "" {
		return peer.InitResult{}, peer.Refuse(peer.CodeWorkerContext, "refusing to provision a peer from inside a worker context (%s): ttorch peer add runs this over the lead's own ssh session", signal)
	}
	req, err := peer.ReadInitRequest(stdin)
	if err != nil {
		return peer.InitResult{}, err
	}
	if err := checkInitRequest(req); err != nil {
		return peer.InitResult{}, err
	}
	u, err := peerAccount()
	if err != nil {
		return peer.InitResult{}, err
	}
	uid := os.Getuid()
	if err := ownedDir(u.home, false); err != nil {
		return peer.InitResult{}, fmt.Errorf("home directory %s: %w", u.home, err)
	}
	home, err := peer.OpenPrivateDir(u.ttorchHome, uid, true)
	if err != nil {
		return peer.InitResult{}, peer.Refuse(peer.CodeUnavailable, "the ttorch home: %v", err)
	}
	defer home.Close()
	conf, err := readPeerEnv(u.ttorchHome, uid)
	if err != nil {
		return peer.InitResult{}, peer.Refuse(peer.CodeUnavailable, "the control channel would refuse this peer.env: %v", err)
	}

	store, err := db.Open(filepath.Join(u.ttorchHome, "state.db"))
	if err != nil {
		return peer.InitResult{}, err
	}
	prov, err := store.ProvisionAsPeer(ctx, req.Name, req.ParentID, req.Force)
	store.Close()
	if errors.Is(err, db.ErrOtherParent) {
		return peer.InitResult{}, peer.Refuse(peer.CodeWrongParent, "%v", err)
	}
	if err != nil {
		return peer.InitResult{}, err
	}

	envPath := filepath.Join(u.ttorchHome, peerEnvFile)
	envState := "kept"
	fresh := map[string]string{"PATH": initPath(u, os.Getenv("PATH"))}
	for k, v := range req.Settings {
		fresh[k] = v
	}
	switch err := writePeerEnv(home, fresh); {
	case err == nil:
		if conf, err = readPeerEnv(u.ttorchHome, uid); err != nil {
			return peer.InitResult{}, fmt.Errorf("reading back the peer.env just written: %w", err)
		}
		envState = "written"
	case !errors.Is(err, fs.ErrExist):
		return peer.InitResult{}, err
	}
	path := conf["PATH"]
	if path == "" {
		path = defaultPeerPath(u)
	}

	bin, inst, err := peer.ProvisionControlKey(filepath.Join(u.home, ".ssh"), req.ControlKey, req.ParentID, uid)
	if err != nil {
		return peer.InitResult{}, err
	}
	keys := "present"
	if inst.Added {
		keys = "added"
	}
	c := prov.Coordinator
	return peer.InitResult{
		CoordID: c.CoordID, Name: c.Name, Role: c.Role, ParentID: c.ParentID, PreviousParent: prov.PreviousParent,
		Binary: bin, AuthorizedKeys: keys, StaleControlKeys: inst.StaleControlKeys,
		PeerEnv: envState, PeerEnvPath: envPath, Missing: missingPrograms(path),
	}, nil
}

// checkInitRequest refuses a request with a field out of bounds, before anything is opened. A
// setting must be one peer.env may hold: a TTORCH_* key that is not a path or an identity, with a
// printable value. PATH is the peer's own and never comes from the parent.
func checkInitRequest(req peer.InitRequest) error {
	if err := db.ValidPeerName(req.Name); err != nil {
		return peer.Refuse(peer.CodeBadRequest, "name: %v", err)
	}
	if err := db.ValidCoordID(req.ParentID); err != nil {
		return peer.Refuse(peer.CodeBadRequest, "parent_id: %v", err)
	}
	if _, err := peer.ParseControlKey(req.ControlKey); err != nil {
		return peer.Refuse(peer.CodeBadRequest, "control_key: %v", err)
	}
	for k, v := range req.Settings {
		if k == "PATH" || !peerEnvKey(k) {
			return peer.Refuse(peer.CodeBadRequest, "settings: %q is not a setting peer.env may hold from the parent", k)
		}
		if !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
			return peer.Refuse(peer.CodeBadRequest, "settings: the value of %s holds a non-printing character", k)
		}
	}
	return nil
}

// privateDir creates dir 0700 if it is missing, and otherwise requires it to be a directory, not
// a symlink, that uid owns and no one else can write (peer.OpenPrivateDir).
func privateDir(dir string, uid int) error {
	f, err := peer.OpenPrivateDir(dir, uid, true)
	if f != nil {
		f.Close()
	}
	return err
}

// initPath is the PATH a new peer.env gets: the control channel's default (defaultPeerPath),
// then each absolute directory of this session's PATH it does not already hold. A claude
// installed under a version manager or a custom prefix is on the lead's PATH and on none of the
// defaults.
func initPath(u peerUser, session string) string {
	dirs := filepath.SplitList(defaultPeerPath(u))
	seen := map[string]bool{}
	for _, d := range dirs {
		seen[d] = true
	}
	for _, d := range filepath.SplitList(session) {
		if !filepath.IsAbs(d) || seen[d] || !utf8.ValidString(d) || strings.IndexFunc(d, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// writePeerEnv creates peer.env 0600 in the ttorch home dir was opened as, with conf, PATH first
// and the rest sorted. It never replaces a file: one that exists, a symlink included, fails with
// fs.ErrExist (O_EXCL) and is left as it is.
func writePeerEnv(dir *os.File, conf map[string]string) error {
	var b strings.Builder
	b.WriteString("# Written by ttorch peer init. The PATH and TTORCH_* settings of every process the\n")
	b.WriteString("# control channel (ttorch peer serve) starts. Edit it here: nothing rewrites it.\n")
	keys := make([]string, 0, len(conf))
	for k := range conf {
		if k != "PATH" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if _, ok := conf["PATH"]; ok {
		keys = append([]string{"PATH"}, keys...)
	}
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, conf[k])
	}
	f, err := peer.OpenAt(dir, peerEnvFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// missingPrograms names the fleetPrograms no directory of path holds as an executable file.
func missingPrograms(path string) []string {
	out := []string{}
	for _, prog := range fleetPrograms {
		found := false
		for _, dir := range filepath.SplitList(path) {
			fi, err := os.Stat(filepath.Join(dir, prog))
			if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
				found = true
				break
			}
		}
		if !found {
			out = append(out, prog)
		}
	}
	return out
}
