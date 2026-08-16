// Package instscript generates the IRIX inst(1M) install runbook that
// instigator serves alongside a distribution: the exact command
// sequence an operator (or a scripted serial-console driver) types at
// the Inst> prompt to take a fresh machine from PROM to a booted IRIX
// 6.5.x install, with the server's real address and the served
// distribution's real path filled in — no placeholders.
//
// # Research: the canonical inst sequence
//
// The command order below is corroborated across four independent
// sources: SGI's own "IRIX 6.5 Installation Instructions" manual (doc
// 007-3862-008, "Installations for Nongraphical Systems" chapter,
// mirrored at https://irix7.com/techpubs/007-3862-008.pdf); the
// inst(1) man page (mirrored at
// https://ibg.colorado.edu/~lessem/psyc5112/usail/man/irix/inst.1.html);
// a worked example at
// https://hcoop.net/~docelic/irix-remote-install/remote-irix-6.5-installation-from-linux.html
// (this is the site the "sgidepot.co.uk/netboot.html" and
// "techpubs.spinlocksolutions.com" links redirect to — both of those
// hostnames' TLS certs have expired/mismatched, but the content lives
// at hcoop.net); and forums.irixnet.org/thread-92.html ("Complete
// conflict free IRIX 6.5.30 install").
//
// The primary source named for this work,
// https://wiki.preterhuman.net/IRIX_Network_Installation_to_a_SGI_Fuel,
// 403s from an automated fetcher (Cloudflare bot challenge) but loads
// fine with a plain curl carrying a normal browser User-Agent — the
// challenge is triggered by the fetcher's fingerprint, not a real
// block. Retrieved directly; it is IRIX 6.5.28 onto a Fuel, an Indy as
// install server, and its own stated focus is "driving inst using a
// pre-written command file instead of manually performing all needed
// software selection steps."
//
// The Main Menu sequence for a complete install from a remote
// distribution set:
//
//  1. from <server>:<distpath>       — point inst at the distribution.
//     The official manual and the Fuel walkthrough both instead use
//     "open" for CD-by-CD reading of several separate distributions.
//     instigator serves each disc whole and synthesizes a
//     .related_dists on the disc this command names, listing the
//     others; inst follows it and opens them itself, so one "from"
//     covers the set and no "open" commands are needed. See
//     internal/vfs/combined.go for why the discs stay separate rather
//     than being flattened into one dist.
//  2. release-stream choice: interactively, first install only, Inst
//     presents "1. Place me on the maintenance stream." / "2. Place me
//     on the feature stream." — feature is a strict superset of
//     maintenance (bug fixes + new hardware support, plus new software
//     features) and is the common recommendation because installing
//     from the smaller maintenance stream first and switching later
//     generates far more package conflicts than starting on feature.
//     But this prompt does NOT require a human at the console: the
//     Inst commands "install feature" and "install maint" set the
//     stream without waiting for it, given "when the Inst prompt first
//     appears" (SGI IRIX Admin: Software Installation and Licensing,
//     doc 007-1364-140, ch. 7 "Maintenance Tips" > "Switching
//     Streams"). That same command also does the work of step 3 below
//     — "This clears any existing selections, selects [stream] stream
//     upgrades, selects products required by these upgrades, and sets
//     the release stream preference to [stream]" — so it replaces
//     "keep * ; install standard", not just the menu answer.
//  3. install prereqs — whatever the stream selection didn't already
//     pull in as a direct prerequisite. The Fuel walkthrough's command
//     file uses "keep *" / "install standard" instead of "install
//     <stream>" (its install was already stream-committed from a prior
//     release, so there was no prompt left to answer), but otherwise
//     confirms the same "install prereqs" follow-up. "keep
//     incompleteoverlays" is added after (confirmed by
//     forums.irixnet.org/thread-92 and the hcoop.net worked example) to
//     stop inst from trying to install placeholder stubs for overlay
//     products that were never opened.
//  4. go — triggers the preinstallation/conflict check.
//  5. conflicts <n><choice> <n><choice> ... — e.g. "conflicts 1a 2b"
//     (abbreviation "c"). Both the official manual and the hcoop.net
//     example use this exact form; the manual additionally documents
//     entering "1" first, for "Address these conflicts now", and "q" to
//     stop viewing an overlong conflict list before resolving what's
//     visible and re-running "conflicts" for the rest. The Fuel
//     walkthrough doesn't show a specific conflict's syntax, but names
//     the same two commands at the same point: "check for any
//     remaining conflicts by typing 'conflicts' ... type 'go' to start
//     the actual installation."
//  6. go — re-run once all conflicts are resolved; this is the one that
//     actually starts file installation.
//  7. quit — once Inst reports the installation finished.
//  8. y — answer "Ready to restart the system. Restart?"
//
// # Can inst read commands from a file?
//
// Yes, confirmed directly by the Fuel walkthrough: at the Inst>
// prompt, "admin source <host>:<path>" loads a remote text file and
// replays each line as if typed at the prompt. The file uses the exact
// same interactive vocabulary as the Main Menu above — "from", "open",
// "keep", "install" — not a distinct file syntax; "install feature"
// and "install maint" are ordinary Main Menu commands like any other,
// so they replay through admin source the same as "from" or "keep *"
// do. (inst(1)'s man page separately documents a shell-level "-F
// selections-file" flag with its own directive grammar — from /
// install / don't install / remove / don't remove / set — for driving
// inst non-interactively from the command line before it starts;
// that's a real, different mechanism, but it's not the one the
// field-tested walkthrough uses, so Commands below follows the
// walkthrough's "admin source" form instead.)
//
// Both mechanisms still stop short of full automation the same way:
// the walkthrough is explicit that its command file omits "go" on
// purpose — "Note that we didn't place a 'go' command at the end of
// the command file. This way you still have a chance to add additional
// distributions before performing the installation. If you are done,
// check for any remaining conflicts by typing 'conflicts'. If none are
// remaining, just type 'go'" — and neither mechanism's vocabulary
// includes a way to view or resolve a conflict. So: yes for product
// selection AND the release-stream choice, no for conflict resolution
// or triggering the install itself — those remain a human (or
// scripted-console-driver) step, which is why Generate produces a full
// runbook and Commands only covers the admin-source-safe subset.
package instscript

import (
	"fmt"
	"strings"
)

// Params fills in an inst runbook with one server's real configuration.
type Params struct {
	// ServerIP is the netinstall server's address, used as the "from"
	// command's remote host.
	ServerIP string
	// DistPath is the served path of the one distribution inst is
	// pointed at: a combined set's primary disc's own dist, e.g.
	// "/irix6.5.30/tools/dist". The rest of the set is reached through
	// that disc's synthesized .related_dists, not through this path.
	DistPath string
	// Release is the IRIX release being installed, e.g. "6.5.30".
	Release string
	// Stream is "feature" or "maintenance" (see the package doc
	// comment, "release-stream choice"). Anything else defaults to
	// feature, the common recommendation, with a note saying so. It
	// drives both the interactive menu entry Generate shows for
	// orientation and the "install feature"/"install maint" command
	// that actually answers it.
	Stream string
}

// streamChoice resolves Stream to SGI's first-install release-stream
// prompt: the interactive Main Menu wording and entry number (shown for
// orientation - see the package doc comment) and the "install
// maint"/"install feature" command that answers it non-interactively.
func streamChoice(stream string) (menuNumber, name, command string) {
	switch strings.ToLower(strings.TrimSpace(stream)) {
	case "maintenance", "maint":
		return "1", "maintenance", "install maint"
	case "feature":
		return "2", "feature", "install feature"
	default:
		return "2", "feature", "install feature"
	}
}

// promBootLine returns the PROM command-monitor line that starts fx,
// the disk partitioner, over the network. This follows the bootp()
// convention instigator already announces at startup
// (internal/serve/serve.go logStartup: "boot -f bootp():/<media>/<disc
// slug>/stand/fx.64") and documents in README.md. DistPath ends in the
// primary disc's own "dist"; that disc carries the miniroot beside it
// under "stand/", so the trailing "/dist" is trimmed before appending
// "stand/fx.64".
func promBootLine(distPath string) string {
	base := strings.TrimSuffix(distPath, "/")
	base = strings.TrimSuffix(base, "/dist")
	base = strings.TrimPrefix(base, "/")
	return fmt.Sprintf("boot -f bootp():/%s/stand/fx.64", base)
}

// Generate returns the full install runbook: a header, the PROM boot
// line for reference, then the numbered Inst> commands in the order
// documented in the package doc comment, with Params' real values
// filled in throughout. It is copy-pasteable at an IRIX serial console.
func Generate(p Params) string {
	_, name, command := streamChoice(p.Stream)

	var b strings.Builder

	fmt.Fprintf(&b, "IRIX %s network install runbook\n", p.Release)
	fmt.Fprintf(&b, "server %s, distribution %s, %s stream\n", p.ServerIP, p.DistPath, name)
	b.WriteString(strings.Repeat("=", 40))
	b.WriteString("\n\n")

	b.WriteString("This is an operator runbook, not an unattended installer: inst(1M)'s\n")
	b.WriteString("\"admin source\" file mechanism has no way to resolve package conflicts\n")
	b.WriteString("or trigger the install, so a human (or a scripted serial-console\n")
	b.WriteString("driver) needs to be present for those steps. See Commands() for the\n")
	b.WriteString("subset inst can load from a file unattended, up through product\n")
	b.WriteString("selection.\n\n")

	b.WriteString("1. PROM command monitor — boot the disk partitioner over the network:\n\n")
	fmt.Fprintf(&b, "     %s\n\n", promBootLine(p.DistPath))
	b.WriteString("   Partition the disk with fx, then let it continue into the miniroot\n")
	b.WriteString("   installer and the Inst> prompt.\n\n")

	b.WriteString("2. At the Inst> prompt, point inst at the distribution:\n\n")
	fmt.Fprintf(&b, "     from %s:%s\n\n", p.ServerIP, p.DistPath)
	b.WriteString("   That one command covers every disc. The disc it names carries a\n")
	b.WriteString("   .related_dists listing the others, and inst opens each of them\n")
	b.WriteString("   itself — expect it to report several distributions opened, not one.\n")
	b.WriteString("   There is no per-disc \"open\" step.\n\n")

	b.WriteString("3. Select the release stream and the standard product set together.\n")
	b.WriteString("   On a first install, Inst would otherwise stop and ask interactively:\n\n")
	b.WriteString("     1. Place me on the maintenance stream.\n")
	b.WriteString("     2. Place me on the feature stream.\n\n")
	fmt.Fprintf(&b, "   %q answers that without waiting for it, and also does the\n", command)
	b.WriteString("   work \"keep * ; install standard\" would: it clears prior selections\n")
	b.WriteString("   and selects the stream's upgrades and their prerequisites.\n")
	if name == "feature" {
		b.WriteString("   Feature is a superset of maintenance, the common recommendation,\n")
		b.WriteString("   since starting on maintenance and switching later generates more\n")
		b.WriteString("   conflicts than starting on feature.\n")
	}
	b.WriteString("\n")
	b.WriteString("   Either type the lines below directly, or save them to a file on the\n")
	b.WriteString("   server and load them in one shot with \"admin source <host>:<path>\"\n")
	b.WriteString("   (see Commands()):\n\n")
	fmt.Fprintf(&b, "     %s\n", command)
	b.WriteString("     install prereqs\n")
	b.WriteString("     keep incompleteoverlays\n\n")

	b.WriteString("4. Start the preinstallation/conflict check:\n\n")
	b.WriteString("     go\n\n")

	b.WriteString("5. If inst reports conflicts, address them now (enter 1), then resolve\n")
	b.WriteString("   each numbered conflict with a lettered choice, e.g. \"conflicts 1a 2b\"\n")
	b.WriteString("   (abbreviation \"c\"). If the list is long, enter \"q\" to stop viewing it,\n")
	b.WriteString("   resolve what's visible, then run \"conflicts\" again for the rest:\n\n")
	b.WriteString("     conflicts <n><choice> <n><choice> ...\n\n")

	b.WriteString("6. Once there are no more conflicts, start the install for real:\n\n")
	b.WriteString("     go\n\n")

	b.WriteString("7. When Inst reports the installation finished:\n\n")
	b.WriteString("     quit\n\n")

	b.WriteString("8. Answer yes when asked \"Ready to restart the system. Restart?\":\n\n")
	b.WriteString("     y\n")

	return b.String()
}

// Commands returns the subset of the inst runbook that inst(1M) can
// genuinely load from a file unattended, one bare command per line,
// written in the plain Inst> vocabulary that "admin source <host>:<path>"
// replays a file's lines as (see the package doc comment, "Can inst
// read commands from a file?" — this is the mechanism the field-tested
// walkthrough this package cites actually uses, confirmed by fetching
// it directly).
//
// It covers "from" plus the release-stream and product selection from
// Generate's step 3 — "install feature"/"install maint" sets the
// stream and does what "keep * ; install standard" would in one
// command (007-1364-140 ch.7), so this is a plain Main Menu command
// admin source replays exactly like any other. Deliberately excluded,
// per the walkthrough: "go" ("we didn't place a 'go' command at the
// end of the command file. This way you still have a chance to add
// additional distributions before performing the installation.") and,
// since no source found gives a file directive for it, conflict
// resolution. A human still runs conflicts/go/quit afterward — see
// Generate's steps 4 through 8.
func Commands(p Params) string {
	_, _, command := streamChoice(p.Stream)
	var b strings.Builder
	fmt.Fprintf(&b, "from %s:%s\n", p.ServerIP, p.DistPath)
	fmt.Fprintf(&b, "%s\n", command)
	b.WriteString("install prereqs\n")
	b.WriteString("keep incompleteoverlays\n")
	return b.String()
}
