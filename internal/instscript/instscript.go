// Package instscript generates the command file loaded by IRIX inst. It
// opens supplemental sets in order, reopens the primary set last, applies
// the tested selections, and starts installation. A second pass can then
// install add-on products on top of the installed operating system, and
// post-install commands can then run in the installed system.
package instscript

import (
	"fmt"
	"strings"
)

// Selection is the customization a named install script layers on top of the
// proven baseline. Install, Keep, and Remove add inst install/keep/remove lines
// after the baseline; Stream picks the IRIX release stream. The zero value adds
// nothing, so Commands then reproduces the baseline command file verbatim.
type Selection struct {
	// Stream selects the release stream. "maintenance" emits "install maint"
	// to switch off inst's default feature stream; any other value ("" or
	// "feature") keeps the default and emits no stream directive.
	Stream string
	// Install, Keep, and Remove are inst product/subsystem tokens, each
	// emitted as one "install"/"keep"/"remove" line.
	Install []string
	Keep    []string
	Remove  []string
}

// Params fills in an inst command file with one server's configuration.
type Params struct {
	// ServerIP is the netinstall server's address, used as the remote
	// host in every "open" command.
	ServerIP string
	// Sets holds the served dist path of every enabled install set, in
	// open order, e.g. ["/6.5.30/dist", "/foundations/dist"]. Must not be
	// empty; Sets[0] is the primary release, already open, and reopened
	// last by Commands.
	Sets []string
	// Selection extends the baseline for a named install script. The zero
	// value is the baseline install.cmds.
	Selection Selection
	// StartPath hands off to a generated file that runs go and reports its
	// return. Empty keeps the standalone command sequence unchanged.
	StartPath string
}

// Commands returns inst(1M)'s admin-source sequence, one bare command per
// line: open each later set, reopen the primary, run the proven baseline
// selection, then apply the Selection's stream switch and extra
// install/keep/remove lines. With a zero Selection it emits only the baseline
// (no "install feature/maint/prereqs", "conflicts", or "quit"); a configured
// script re-enables that vocabulary on purpose. "go" is safe because the
// java_dev.sw.base conflict is resolved by name.
func Commands(p Params) string {
	var b strings.Builder
	for _, set := range p.Sets[1:] {
		fmt.Fprintf(&b, "open %s:%s\n", p.ServerIP, set)
	}
	// The miniroot has the primary source open already, but a later
	// foundation source can select an older same-named product. Reopening
	// the primary source makes the 6.5.30 release metadata win.
	fmt.Fprintf(&b, "open %s:%s\n", p.ServerIP, p.Sets[0])
	// Reopening the primary source pauses at an interactive distribution
	// menu. The operator must select its numbered done item on the console.
	// A scripted "done" runs only after that menu closes and is rejected.
	// Switch the release stream before selecting, so the standard selection
	// resolves to the maintenance versions. The feature stream is inst's
	// default and needs no directive.
	if p.Selection.Stream == "maintenance" {
		b.WriteString("install maint\n")
	}
	b.WriteString("keep *\n")
	b.WriteString("install standard\n")
	// The two Java base products conflict. Keep the development product
	// deselected so the standard Java EOE runtime remains selected.
	b.WriteString("keep java_dev.sw.base\n")
	for _, s := range p.Selection.Install {
		fmt.Fprintf(&b, "install %s\n", s)
	}
	for _, s := range p.Selection.Keep {
		fmt.Fprintf(&b, "keep %s\n", s)
	}
	for _, s := range p.Selection.Remove {
		fmt.Fprintf(&b, "remove %s\n", s)
	}
	if p.StartPath == "" {
		b.WriteString("go\n")
	} else {
		fmt.Fprintf(&b, "admin source %s:%s\n", p.ServerIP, p.StartPath)
	}
	return b.String()
}

// ThenParams fills in the second pass of a script, run after the first go
// has returned to Inst>.
type ThenParams struct {
	// ServerIP is the netinstall server's address.
	ServerIP string
	// Sets holds the served dist path of every add-on set the pass opens.
	Sets []string
	// Install lists the inst product/subsystem tokens to install from them.
	Install []string
	// StartPath hands off to a generated file that runs go and reports its
	// return. Empty ends the pass with a plain go.
	StartPath string
}

// ThenCommands returns the second pass: open the add-on sets, keep
// everything already installed, select only the named add-on products and
// start installation. Running it as its own go installs the operating
// system first, so the add-on's files go through the symbolic links the
// operating system's products create, such as /var/sysgen/boot, instead
// of making real directories in their place.
func ThenCommands(p ThenParams) string {
	var b strings.Builder
	for _, set := range p.Sets {
		fmt.Fprintf(&b, "open %s:%s\n", p.ServerIP, set)
	}
	b.WriteString("keep *\n")
	for _, s := range p.Install {
		fmt.Fprintf(&b, "install %s\n", s)
	}
	if p.StartPath == "" {
		b.WriteString("go\n")
	} else {
		fmt.Fprintf(&b, "admin source %s:%s\n", p.ServerIP, p.StartPath)
	}
	return b.String()
}

// PostInstallDone is the console line printed after every post-install
// command has succeeded. A failing command prints "(command failed)"
// instead, and inst skips the rest of the file.
const PostInstallDone = "instigator: post-install commands finished"

// PostInstallCommands returns a command file that runs each command in the
// installed system, then prints PostInstallDone. inst's shroot runs its
// arguments as one command in a chroot of the target, which exists only
// after go has installed it. shroot uses inst's shell preference, /bin/csh
// by default, so select /bin/sh for POSIX syntax. inst saves preferences in
// the target's /var/inst/.swmgrrc, so put the default back afterwards.
func PostInstallCommands(commands []string) string {
	var b strings.Builder
	b.WriteString("set shell /bin/sh\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "shroot %s\n", c)
	}
	b.WriteString("set shell /bin/csh\n")
	fmt.Fprintf(&b, "sh echo %s\n", PostInstallDone)
	return b.String()
}

// RelatedDists returns the ".related_dists" marker served alongside the
// primary distribution. The stock IRIX marker "CD" suppresses inst's
// interactive related-distribution prompt; Commands opens every enabled set
// explicitly, so listing them here would make admin source pause.
func RelatedDists(sets []string) string {
	if len(sets) == 0 {
		return ""
	}
	return "CD\n"
}
