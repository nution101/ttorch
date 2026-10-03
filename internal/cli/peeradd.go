package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nution101/ttorch/internal/db"
	"github.com/nution101/ttorch/internal/paths"
	"github.com/nution101/ttorch/internal/peer"
	"github.com/nution101/ttorch/internal/proc"
)

// Provisioning a peer from the parent: `ttorch peer add` and `ttorch peer adopt`, the control
// key they make, and the settings they hand a new peer.env. The commands that use a provisioned
// peer are in peerclient.go.

// Seams. A test points peerSSH at a stand-in for ssh; the timeouts bound one control call and
// one init session.
var (
	peerSSH         = "ssh"
	peerKeygen      = "ssh-keygen"
	peerCallTimeout = peer.DefaultTimeout
	peerInitTimeout = peer.InitTimeout
)

// keygenTimeout bounds generating a control key.
const keygenTimeout = 30 * time.Second

// peerKeyDir is where a peer's control key lives: <ttorch home>/peers/<name>.
func peerKeyDir(name string) string { return filepath.Join(paths.Default().Home, "peers", name) }

// cmdPeerProvision is `ttorch peer add` and, with adopt, `ttorch peer adopt --force`.
//
// It refuses a worker context and a stdin that is not a terminal first, before it parses a flag,
// opens the store or starts a process. Then:
//
//  1. It finds or makes the control key under <ttorch home>/peers/<name>/ (directory 0700,
//     private key 0600), with ssh-keygen, an ed25519 key with no passphrase, since nobody is there
//     to type one when the scheduler polls.
//  2. It registers the peer as provisioning.
//  3. Over the lead's own ssh session to --approve-dest it runs `ttorch peer init`, which records
//     this coordinator as the peer's parent, writes peer.env if there is none (with this shell's
//     TTORCH_* settings), and admits the control key with a forced command.
//  4. It proves the key: a `version` call over the control channel alone, which must report the
//     peer under this name with this coordinator as its parent. Then the peer is live.
//
// A failure leaves the peer provisioning with the reason recorded; running add again resumes,
// reusing the key. adopt is the same with force set, for a peer another parent provisioned, and
// re-registers a peer this coordinator already has.
func cmdPeerProvision(args []string, adopt bool, stdin *os.File) error {
	action := "add a peer"
	if adopt {
		action = "adopt a peer"
	}
	if err := checkLeadCaller(action, stdin); err != nil {
		return err
	}
	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return errors.New(peerClientUsage)
	}
	name, controlDest := args[0], args[1]
	verb := "add"
	if adopt {
		verb = "adopt"
	}
	fs := flag.NewFlagSet("peer "+verb, flag.ContinueOnError)
	approveDest := fs.String("approve-dest", "", "the destination you use interactively (default: <control-dest>)")
	remote := fs.String("remote-ttorch", peer.DefaultRemoteTtorch, "ttorch on the peer, relative to its home")
	force := fs.Bool("force", false, "adopt: move a peer another parent provisioned to this one")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New(peerClientUsage)
	}
	if adopt && !*force {
		return errors.New("peer adopt moves a peer from the parent that provisioned it to this one, and needs --force to say so")
	}
	if !adopt && *force {
		return errors.New("--force is for ttorch peer adopt")
	}
	if *approveDest == "" {
		*approveDest = controlDest
	}
	if err := db.ValidPeerName(name); err != nil {
		return fmt.Errorf("peer %s: %w", verb, err)
	}
	for _, d := range []string{controlDest, *approveDest} {
		if err := peer.ValidDest(d); err != nil {
			return fmt.Errorf("peer %s: %w", verb, err)
		}
	}
	if err := peer.ValidRemoteTtorch(*remote); err != nil {
		return fmt.Errorf("peer %s: --remote-ttorch: %w", verb, err)
	}

	ctx := context.Background()
	store, err := peerStore()
	if err != nil {
		return err
	}
	defer store.Close()
	self, err := store.GetCoordinator(ctx)
	if err != nil {
		return err
	}
	if cur, ok, err := store.GetPeer(ctx, name); err != nil {
		return err
	} else if ok && !adopt && cur.Status != db.PeerProvisioning && cur.Status != db.PeerRetired {
		return fmt.Errorf("peer %s is already registered (%s, control %s); see ttorch peer ls, or ttorch peer retire %s first", name, cur.Status, cur.ControlDest, name)
	}
	key, pub, err := controlKey(ctx, name, self.CoordID)
	if err != nil {
		return err
	}
	if _, err := store.RegisterPeer(ctx, db.Peer{Name: name, ControlDest: controlDest, ApproveDest: *approveDest, ControlKey: key}, adopt); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = store.RecordPeerError(ctx, name, peer.SafeText(err.Error()))
		return fmt.Errorf("peer %s %s: %w\nthe peer stays provisioning; run ttorch peer %s again once this is fixed", verb, name, err, verb)
	}

	settings := leadSettings()
	fmt.Fprintf(os.Stderr, "running ttorch peer init on %s over your own ssh session...\n", *approveDest)
	res, err := peer.RunInit(ctx, peerSSH, *approveDest, *remote, peerInitTimeout, peer.InitRequest{
		Name: name, ParentID: self.CoordID, ControlKey: pub, Force: adopt, Settings: settings,
	}, os.Stderr)
	if err != nil {
		return fail(err)
	}
	if res.Role != db.CoordinatorPeer || res.ParentID != self.CoordID || res.Name != name {
		return fail(fmt.Errorf("peer init reports %s %q with parent %s, not peer %q of this coordinator (%s)", res.Role, res.Name, res.ParentID, name, self.CoordID))
	}

	client := peer.Client{SSH: peerSSH, Dest: controlDest, Key: key, Timeout: peerCallTimeout}
	var v peer.VersionResult
	if err := client.Call(ctx, peer.VerbVersion, nil, &v); err != nil {
		return fail(fmt.Errorf("the control key does not work yet: %w", err))
	}
	if v.Coordinator.Role != db.CoordinatorPeer || v.Coordinator.Name != name || v.Coordinator.ParentID != self.CoordID {
		return fail(fmt.Errorf("the control channel at %s answers as %s %q with parent %s, not as peer %q of this coordinator; it reads another store than the one init wrote", controlDest, v.Coordinator.Role, v.Coordinator.Name, v.Coordinator.ParentID, name))
	}
	// The key's forced command must name this coordinator too, or the peer refuses every task,
	// goal and answer sent through it. A peer binary older than the --parent binding writes a
	// line without one.
	if v.KeyParent != self.CoordID {
		bound := "no parent"
		if v.KeyParent != "" {
			bound = "parent " + v.KeyParent
		}
		return fail(fmt.Errorf("the control key at %s is bound to %s, not to this coordinator (%s); the peer would refuse its tasks, goals and answers. Update ttorch on the peer, then run ttorch peer adopt --force", controlDest, bound, self.CoordID))
	}
	if err := store.MarkPeerLive(ctx, name, v.Protocol, v.Version); err != nil {
		return err
	}

	fmt.Printf("peer %s is live: ttorch %s (protocol %d), control key %s\n", name, v.Version, v.Protocol, key)
	fmt.Printf("  forced command: %s peer serve --parent %s (restrict), authorized_keys %s\n", res.Binary, self.CoordID, res.AuthorizedKeys)
	if res.PreviousParent != "" && res.PreviousParent != self.CoordID {
		fmt.Printf("  moved from parent %s\n", res.PreviousParent)
	}
	if res.StaleControlKeys > 0 {
		fmt.Printf("  authorized_keys still holds %d other control key line(s) (comment ttorch-peer-control:...); remove any you no longer use\n", res.StaleControlKeys)
	}
	switch res.PeerEnv {
	case "written":
		keys := make([]string, 0, len(settings))
		for k := range settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("  peer.env written at %s: PATH and %d setting(s) from this shell %v\n", res.PeerEnvPath, len(keys), keys)
	default:
		fmt.Printf("  peer.env kept as it is at %s\n", res.PeerEnvPath)
	}
	if len(res.Missing) > 0 {
		fmt.Printf("  not on the peer's PATH: %s; add their directories to PATH in %s\n", strings.Join(res.Missing, ", "), res.PeerEnvPath)
	}
	return nil
}

// leadSettings are the TTORCH_* settings of the shell running `peer add` that peer.env may hold:
// the lead's policy (models, efforts, limits), handed to a new peer.env. Paths, identities and
// worker tabs are left out (peerEnvKey), and so is anything not printable.
func leadSettings() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "PATH" || !peerEnvKey(k) || v == "" || !utf8.ValidString(v) ||
			strings.IndexFunc(v, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
			continue
		}
		out[k] = v
	}
	return out
}

// controlKey returns the path of name's control key and its public half, generating the key if
// there is none. The directory is created 0700 and must otherwise be private; a key that exists
// must be a private regular file (no group or other bits at all) whose public half reads as one
// ed25519 key.
func controlKey(ctx context.Context, name, coordID string) (key, pub string, err error) {
	dir := peerKeyDir(name)
	uid := os.Getuid()
	if err := privateDir(filepath.Dir(dir), uid); err != nil {
		return "", "", err
	}
	if err := privateDir(dir, uid); err != nil {
		return "", "", err
	}
	key = filepath.Join(dir, "control")
	_, keyErr := os.Lstat(key)
	_, pubErr := os.Lstat(key + ".pub")
	switch {
	case errors.Is(keyErr, fs.ErrNotExist) && errors.Is(pubErr, fs.ErrNotExist):
		ctx, cancel := context.WithTimeout(ctx, keygenTimeout)
		defer cancel()
		cmd := proc.Command(ctx, peerKeygen, "-q", "-t", "ed25519", "-N", "", "-C", peer.ControlKeyComment(coordID), "-f", key)
		if out, err := proc.CombinedOutput(cmd); err != nil {
			return "", "", fmt.Errorf("generating the control key with %s: %v: %s", peerKeygen, err, peer.SafeText(strings.TrimSpace(string(out))))
		}
	case keyErr != nil || pubErr != nil:
		return "", "", fmt.Errorf("%s holds half a control key; remove the directory and run the command again", dir)
	}
	fi, err := os.Lstat(key)
	if err != nil {
		return "", "", err
	}
	if err := peer.CheckPrivate(key, fi, uid, false); err != nil {
		return "", "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", "", fmt.Errorf("%s is readable by its group or by others (mode %s); a private key must be 0600", key, fi.Mode().Perm())
	}
	b, err := readCapped(key+".pub", 16<<10)
	if err != nil {
		return "", "", err
	}
	if _, err := peer.ParseControlKey(string(b)); err != nil {
		return "", "", fmt.Errorf("%s.pub: %w", key, err)
	}
	return key, strings.TrimSpace(string(b)), nil
}
