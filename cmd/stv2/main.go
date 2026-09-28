// Command stv2 is SyncThing V2: a tray app that keeps Syncthing running and
// pairs computers over Tailscale. Without arguments it runs the tray; the
// subcommands install, inspect and configure it (spec §3.10).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/Neutx/syncthing-v2/internal/app"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/doctor"
	"github.com/Neutx/syncthing-v2/internal/glass"
	"github.com/Neutx/syncthing-v2/internal/install"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/single"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
	"github.com/Neutx/syncthing-v2/internal/ui"
	"github.com/Neutx/syncthing-v2/internal/uihost"
)

func init() {
	// The tray (and the macOS/Windows UI loops) must own the main OS thread.
	runtime.LockOSThread()
}

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

const usage = `Usage: stv2 [command] [options]

Without a command, runs the SyncThing V2 tray (or shows the dashboard of the
tray that is already running).

  --background                     tray only, no dashboard (used at login)
  --setup                          tray, dashboard and first-run setup
  --quit                           ask the running tray to exit
  install [--yes] [--no-start]     install SyncThing V2 for this user
  uninstall [--yes] [--remove-syncthing]
                                   remove SyncThing V2 (never Syncthing's config or data)
  pair --list [--json]             list tailnet devices that can be paired
  doctor [--json]                  check the setup; exit code 1 on high findings
  firewall allow                   Windows: allow Syncthing through the firewall (asks for admin)
  profile tailnet|hybrid [--yes]   apply a transport profile to Syncthing
  demo [--port N]                  serve the dashboard with synthetic data
  version                          print the version and the disclaimer
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "", "--background", "-background", "--setup", "-setup":
		if len(args) > 1 {
			attachConsole()
			fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n%s", args[1], usage)
			return exitUsage
		}
		return runTray(strings.TrimLeft(cmd, "-"))
	case "ui-host":
		if err := uihost.RunChild(); err != nil {
			fmt.Fprintf(os.Stderr, "ui-host: %v\n", err)
			return exitFail
		}
		return exitOK
	}

	attachConsole()
	rest := args[1:]
	switch cmd {
	case "--quit", "-quit":
		return cmdQuit()
	case "install":
		return cmdInstall(rest)
	case "uninstall":
		return cmdUninstall(rest)
	case "pair":
		return cmdPair(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "firewall":
		return cmdFirewall(rest)
	case "profile":
		return cmdProfile(rest)
	case "demo":
		return cmdDemo(rest)
	case "version", "--version", "-version", "-v":
		return cmdVersion(os.Stdout)
	case "help", "--help", "-help", "-h":
		fmt.Fprint(os.Stdout, usage)
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
	return exitUsage
}

// newFlags returns a flag set that prints errors and usage to stderr.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parse parses args and rejects positional leftovers beyond maxArgs.
func parse(fs *flag.FlagSet, args []string, maxArgs int) bool {
	if err := fs.Parse(args); err != nil {
		return false
	}
	if fs.NArg() > maxArgs {
		fmt.Fprintf(os.Stderr, "%s: unexpected argument %q\n", fs.Name(), fs.Arg(maxArgs))
		return false
	}
	return true
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "%s: %v\n", brand.BinaryName, err)
	return exitFail
}

// ---------- tray ----------

// runTray runs the tray, or shows the dashboard of the one already running.
func runTray(mode string) int {
	lock, err := app.Acquire()
	if errors.Is(err, single.ErrAlreadyRunning) {
		if mode == "background" {
			return exitOK // autostart while the tray already runs: nothing to do
		}
		if err := app.SignalRunning(context.Background(), "show"); err != nil {
			return fail(fmt.Errorf("the running %s did not answer: %w", brand.DisplayName, err))
		}
		return exitOK
	}
	if err != nil {
		return fail(err)
	}
	if mode == "" {
		// Windows: a copy that is not the installed one offers to install itself.
		if offered, code := offerInstall(lock); offered {
			return code
		}
	}
	err = app.Run(app.Options{
		Background: mode == "background",
		Setup:      mode == "setup",
		Lock:       lock,
		Message:    messageBox(),
	})
	if err != nil {
		return fail(err)
	}
	return exitOK
}

// runInstall installs with a yes/no question for the optional steps.
func runInstall(ctx context.Context, yes, noStart bool, log io.Writer) (install.Result, error) {
	r, err := install.DefaultRoots()
	if err != nil {
		return install.Result{}, err
	}
	return install.Install(ctx, r, install.Options{Yes: yes, NoStart: noStart, Confirm: confirm, Log: log})
}

func cmdQuit() int {
	dataDir, err := osutil.AppDataDir()
	if err != nil {
		return fail(err)
	}
	return quitTray(os.Stdout, dataDir, app.Acquire, app.SignalRunning)
}

// quitTray asks the running tray to exit. The single-instance lock decides
// whether one runs: a tray holds it for its whole life, so when the lock is
// free any instance.json was left by a tray that crashed. That file is
// removed (while the lock is held, so no new tray can be writing it) and the
// answer is "not running" with exit code 0, instead of retrying a refused
// connection and failing, which would abort scripts such as install.sh.
func quitTray(w io.Writer, dataDir string, acquire func() (*single.Lock, error), signal func(context.Context, string) error) int {
	lock, err := acquire()
	switch {
	case err == nil:
		// RemoveInstance also removes an unreadable file; a missing one is fine.
		in, _ := single.ReadInstance(dataDir)
		if err := single.RemoveInstance(dataDir, in.PID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: removing the stale %s: %v\n", single.InstanceFile, err)
		}
		if err := lock.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: releasing the instance lock: %v\n", err)
		}
		fmt.Fprintf(w, "%s is not running\n", brand.DisplayName)
		return exitOK
	case errors.Is(err, single.ErrAlreadyRunning):
		if err := signal(context.Background(), "quit"); err != nil {
			return fail(fmt.Errorf("the running %s did not answer: %w", brand.DisplayName, err))
		}
		return exitOK
	default:
		return fail(err)
	}
}

// ---------- install / uninstall ----------

func cmdInstall(args []string) int {
	fs := newFlags("install")
	yes := fs.Bool("yes", false, "install without asking")
	noStart := fs.Bool("no-start", false, "do not start SyncThing V2 afterwards")
	if !parse(fs, args, 0) {
		return exitUsage
	}
	if !*yes && !confirm(installQuestion()) {
		fmt.Fprintln(os.Stdout, "Installation cancelled.")
		return exitFail
	}
	res, err := runInstall(context.Background(), *yes, *noStart, os.Stdout)
	printNotes(res.Notes)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stdout, "%s is installed at %s\n", brand.DisplayName, res.Exe)
	if !res.Started {
		fmt.Fprintf(os.Stdout, "Start it from the %s menu entry, or run: %s\n", brand.DisplayName, res.Exe)
	}
	return exitOK
}

func installQuestion() string {
	return "Install " + brand.DisplayName + " for " + userName() + "?"
}

func cmdUninstall(args []string) int {
	fs := newFlags("uninstall")
	yes := fs.Bool("yes", false, "uninstall without asking")
	removeST := fs.Bool("remove-syncthing", false, "also remove the Syncthing program SyncThing V2 installed")
	if !parse(fs, args, 0) {
		return exitUsage
	}
	q := "Uninstall " + brand.DisplayName + "? Syncthing's settings, database and synced folders are kept."
	if !*yes && !confirm(q) {
		fmt.Fprintln(os.Stdout, "Uninstall cancelled.")
		return exitFail
	}
	r, err := install.DefaultRoots()
	if err != nil {
		return fail(err)
	}
	res, err := install.Uninstall(context.Background(), r, install.Options{
		Yes: *yes, RemoveSyncthing: *removeST, Confirm: confirm, Log: os.Stdout,
	})
	printNotes(res.Notes)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stdout, "%s was uninstalled.\n", brand.DisplayName)
	return exitOK
}

func printNotes(notes []string) {
	for _, n := range notes {
		fmt.Fprintln(os.Stdout, "Note: "+n)
	}
}

// ---------- pair --list ----------

// candidateOut is one row of `pair --list --json`. Device IDs are cut to
// their 7-character short form.
type candidateOut struct {
	Name      string `json:"name"`
	DNSName   string `json:"dnsName,omitempty"`
	OS        string `json:"os"`
	IP        string `json:"ip"`
	SameOwner bool   `json:"sameOwner"`
	Owner     string `json:"owner,omitempty"`
	ID        string `json:"id,omitempty"`
	Status    string `json:"status"`
}

func cmdPair(args []string) int {
	fs := newFlags("pair")
	list := fs.Bool("list", false, "list the tailnet devices that can be paired")
	asJSON := fs.Bool("json", false, "print JSON")
	if !parse(fs, args, 0) {
		return exitUsage
	}
	if !*list {
		fmt.Fprintln(os.Stderr, "pair: only --list is available on the command line; pair from the dashboard")
		return exitUsage
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c, _, err := app.Connect(ctx)
	if err != nil {
		return fail(fmt.Errorf("syncthing: %w", err))
	}
	// Read-only: listing and probing never change Syncthing or Tailscale.
	svc := pairing.NewService(c, app.Tailnet(), nil)
	cands, err := svc.Discover(ctx)
	if err != nil {
		return fail(err)
	}
	return printCandidates(os.Stdout, cands, *asJSON)
}

func printCandidates(w io.Writer, cands []model.Candidate, asJSON bool) int {
	rows := make([]candidateOut, 0, len(cands))
	for _, c := range cands {
		if c.Status == pairing.StatusSelf {
			continue
		}
		r := candidateOut{
			Name: c.NodeName, DNSName: c.DNSName, OS: c.OS, SameOwner: c.SameOwner,
			ID: deviceid.Short(c.DeviceID), Status: c.Status,
		}
		if c.IP.IsValid() {
			r.IP = c.IP.String()
		}
		if !c.SameOwner {
			r.Owner = c.LoginName
		}
		rows = append(rows, r)
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return fail(err)
		}
		return exitOK
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "No other computers are online in Tailscale.")
		return exitOK
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOS\tIP\tOWNER\tID\tSTATUS")
	for _, r := range rows {
		owner := "you"
		if !r.SameOwner {
			owner = r.Owner
			if owner == "" {
				owner = "other"
			}
		}
		id := r.ID
		if id == "" {
			id = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.OS, r.IP, owner, id, r.Status)
	}
	if err := tw.Flush(); err != nil {
		return fail(err)
	}
	return exitOK
}

// ---------- doctor ----------

func cmdDoctor(args []string) int {
	fs := newFlags("doctor")
	asJSON := fs.Bool("json", false, "print JSON")
	if !parse(fs, args, 0) {
		return exitUsage
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	rep := doctor.Run(ctx, doctor.DefaultEnv())
	var err error
	if *asJSON {
		err = rep.WriteJSON(os.Stdout)
	} else {
		err = rep.WriteText(os.Stdout)
	}
	if err != nil {
		return fail(err)
	}
	return rep.ExitCode()
}

// ---------- firewall allow ----------

func cmdFirewall(args []string) int {
	if len(args) == 0 || args[0] != "allow" {
		fmt.Fprintln(os.Stderr, "usage: stv2 firewall allow")
		return exitUsage
	}
	fs := newFlags("firewall allow")
	bin := fs.String("bin", "", "the Syncthing executable (default: the one SyncThing V2 uses)")
	elevated := fs.Bool(strings.TrimPrefix(install.ElevatedFlag, "--"), false, "internal: the elevated re-launch")
	if !parse(fs, args[1:], 0) {
		return exitUsage
	}
	ctx := context.Background()
	if *bin == "" {
		inst, err := stinstall.Detect(ctx)
		if err != nil {
			return fail(fmt.Errorf("syncthing: %w", err))
		}
		*bin = inst.Bin
	}
	var err error
	if *elevated {
		err = install.AllowFirewallElevated(ctx, *bin)
	} else {
		err = install.AllowFirewall(ctx, *bin)
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stdout, "Added the firewall rule %q for %s\n", install.FirewallRuleName, *bin)
	return exitOK
}

// ---------- profile ----------

func cmdProfile(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: stv2 profile tailnet|hybrid [--yes]")
		return exitUsage
	}
	p, err := stinstall.ParseProfile(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "profile: %v\n", err)
		return exitUsage
	}
	fs := newFlags("profile")
	yes := fs.Bool("yes", false, "apply without asking")
	if !parse(fs, args[1:], 0) {
		return exitUsage
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c, _, err := app.Connect(ctx)
	if err != nil {
		return fail(fmt.Errorf("syncthing: %w", err))
	}
	cur, ok, err := stinstall.CurrentProfile(ctx, c)
	if err != nil {
		return fail(err)
	}
	if ok && cur == p {
		fmt.Fprintf(os.Stdout, "Syncthing at %s already uses the %s profile.\n", c.Host(), p)
		return exitOK
	}
	q := profileQuestion(p, c.Host())
	if !*yes && !confirm(q) {
		fmt.Fprintln(os.Stdout, "No changes made.")
		return exitFail
	}
	if err := stinstall.ApplyProfile(ctx, c, p); err != nil {
		return fail(err)
	}
	if dir, err := osutil.AppDataDir(); err == nil {
		if st, err := prefs.Open(dir); st != nil {
			if err := st.Update(func(pp *prefs.Prefs) error { pp.ProfileChosen = p.String(); return nil }); err != nil {
				fmt.Fprintf(os.Stderr, "warning: saving the choice: %v\n", err)
			}
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "warning: saving the choice: %v\n", err)
		}
	}
	fmt.Fprintf(os.Stdout, "Applied the %s profile to Syncthing at %s.\n", p, c.Host())
	return exitOK
}

func profileQuestion(p stinstall.Profile, host string) string {
	what := "Tailnet only: global discovery, relays and NAT traversal are turned off. Sync then needs Tailscale up, or both computers on the same local network."
	if p == stinstall.Hybrid {
		what = "Hybrid: global discovery, relays and NAT traversal are turned back on (Syncthing's defaults)."
	}
	return fmt.Sprintf("Apply the %s profile to Syncthing at %s?\n\n%s", p, host, what)
}

// ---------- demo ----------

func cmdDemo(args []string) int {
	fs := newFlags("demo")
	port := fs.Int("port", 0, "port on 127.0.0.1 (0 picks a free one)")
	if !parse(fs, args, 0) {
		return exitUsage
	}
	// A glass backdrop rendered from a synthetic wallpaper, never a capture.
	var backdrop []byte
	if png, err := glass.PNG(glass.Render(ui.DemoWallpaper(uihost.DashboardSize.X, uihost.DashboardSize.Y), 1)); err == nil {
		backdrop = png
	}
	srv, err := ui.NewServerPort(ui.DemoWith(backdrop), *port)
	if err != nil {
		return fail(err)
	}
	defer srv.Close()
	fmt.Fprintf(os.Stdout, "%s demo (synthetic data) on %s\n", brand.DisplayName, srv.URL())
	fmt.Fprintf(os.Stdout, "Open (single use, valid 30 s): %s\n", srv.LoginURL(srv.NewLaunchToken(), "/?mode=browser"))
	fmt.Fprintf(os.Stdout, "More login links: POST %s/api/launch with \"Authorization: Bearer %s\" and \"X-STV2: 1\"\n", srv.URL(), srv.ControlToken())
	fmt.Fprintln(os.Stdout, "Press Ctrl+C to stop.")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
	return exitOK
}

// ---------- version ----------

func cmdVersion(w io.Writer) int {
	fmt.Fprintf(w, "%s %s\n", brand.DisplayName, brand.Version)
	fmt.Fprintf(w, "commit %s\n", brand.Commit())
	fmt.Fprintf(w, "%s %s/%s\n\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintln(w, brand.Disclaimer)
	return exitOK
}
