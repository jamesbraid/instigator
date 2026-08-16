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
// conflict free IRIX 6.5.30 install"). The primary source named in the
// task, https://wiki.preterhuman.net/IRIX_Network_Installation_to_a_SGI_Fuel,
// sits behind Cloudflare's bot challenge and returned HTTP 403 to every
// fetch attempt (direct, cache-buster query string, and two read-it-later
// proxies); what's used below instead is its search-result summaries
// (confirmed by Google's indexed text: it "demonstrate[s] the feature of
// driving inst using a pre-written command file instead of manually
// performing all needed software selection steps") plus one verbatim
// quote surfaced by search: "Note that we didn't place a 'go' command at
// the end of the command file... If none are remaining, just type 'go'
// to start the actual installation." That quote corroborates the
// Commands/-F finding below independently of the sources that were
// actually fetchable.
//
// The Main Menu sequence for a complete install from a remote/unified
// distribution:
//
//  1. from <server>:<distpath>       — point inst at the distribution
//     (the official manual instead uses "open" for CD-by-CD reading of
//     several separate distributions; instigator serves one already-
//     unified dist tree, so a single "from" covers it and no further
//     "open" commands are needed)
//  2. release-stream choice, first install only:
//     "1. Place me on the maintenance stream." / "2. Place me on the
//     feature stream." — feature is a strict superset of maintenance
//     (bug fixes + new hardware support, plus new software features)
//     and is the common recommendation because installing from the
//     smaller maintenance stream first and switching later generates
//     far more package conflicts than starting on feature.
//  3. keep * ; install standard ; install prereqs — the standard
//     product set plus whatever it prerequires. "keep incompleteoverlays"
//     is added after these (confirmed by forums.irixnet.org/thread-92
//     and the hcoop.net worked example) to stop inst from trying to
//     install placeholder stubs for overlay products that were never
//     opened.
//  4. go — triggers the preinstallation/conflict check.
//  5. conflicts <n><choice> <n><choice> ... — e.g. "conflicts 1a 2b"
//     (abbreviation "c"). Both the official manual and the hcoop.net
//     example use this exact form; the manual additionally documents
//     entering "1" first, for "Address these conflicts now", and "q" to
//     stop viewing an overlong conflict list before resolving what's
//     visible and re-running "conflicts" for the rest.
//  6. go — re-run once all conflicts are resolved; this is the one that
//     actually starts file installation.
//  7. quit — once Inst reports the installation finished.
//  8. y — answer "Ready to restart the system. Restart?"
//
// # Can inst read commands from a file?
//
// Partially. inst(1)'s SYNOPSIS documents -F selections-file: "used to
// pre-select subsystems for installation or removal; or as a script for
// an automatic installation using inst(1M)." Its directive grammar is
// from / install / don't install / remove / don't remove / set — not
// the interactive vocabulary (no "keep", "conflicts", "go", or "quit").
// Two things are missing from that grammar and confirmed missing by two
// independent sources: there is no directive for the first-install
// release-stream prompt, and none for conflict resolution — the
// wiki excerpt quoted above shows an experienced operator using a
// command file for product selection but still typing "conflicts" and
// "go" by hand afterward, and the -a ("automatic mode", no menus)
// / -u (install-action) flags documented alongside -F do not name a
// resource that answers either prompt. So: yes for the product-selection
// step, no for the release-stream choice or conflict resolution — this
// is fundamentally a human (or a scripted-expect-style) runbook for
// those two steps, which is why Generate below produces a full runbook
// and Commands only covers the part that is genuinely unattended-safe.
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
	// DistPath is the unified distribution path instigator serves,
	// e.g. "/irix6.5.30/dist".
	DistPath string
	// Release is the IRIX release being installed, e.g. "6.5.30".
	Release string
	// Stream is "feature" or "maintenance" (see the package doc
	// comment, "Choosing Release Streams"). Anything else defaults to
	// feature, the common recommendation, with a note saying so.
	Stream string
}

// streamMenu resolves Stream to the exact wording and Main Menu entry
// number of SGI's first-install release-stream prompt (see the package
// doc comment).
func streamMenu(stream string) (number, name string) {
	switch strings.ToLower(strings.TrimSpace(stream)) {
	case "maintenance", "maint":
		return "1", "maintenance"
	case "feature":
		return "2", "feature"
	default:
		return "2", "feature"
	}
}

// promBootLine returns the PROM command-monitor line that starts fx,
// the disk partitioner, over the network. This follows the bootp()
// convention instigator already announces at startup
// (internal/serve/serve.go logStartup: "boot -f bootp():/<media>/<disc
// slug>/stand/fx.64") and documents in README.md. DistPath is the
// unified distribution's "<name>/dist" leaf; fx.64 lives alongside it
// under "stand/", so a trailing "/dist" is trimmed before appending
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
	num, name := streamMenu(p.Stream)

	var b strings.Builder

	fmt.Fprintf(&b, "IRIX %s network install runbook\n", p.Release)
	fmt.Fprintf(&b, "server %s, distribution %s, %s stream\n", p.ServerIP, p.DistPath, name)
	b.WriteString(strings.Repeat("=", 40))
	b.WriteString("\n\n")

	b.WriteString("This is an operator runbook, not an unattended installer: inst(1M)\n")
	b.WriteString("has no documented way to answer the release-stream prompt or resolve\n")
	b.WriteString("package conflicts from a file, so a human (or a scripted serial-console\n")
	b.WriteString("driver) needs to be present for those two steps. See Commands() for the\n")
	b.WriteString("subset of this that inst can take from a file unattended.\n\n")

	b.WriteString("1. PROM command monitor — boot the disk partitioner over the network:\n\n")
	fmt.Fprintf(&b, "     %s\n\n", promBootLine(p.DistPath))
	b.WriteString("   Partition the disk with fx, then let it continue into the miniroot\n")
	b.WriteString("   installer and the Inst> prompt.\n\n")

	b.WriteString("2. At the Inst> prompt, point inst at the distribution:\n\n")
	fmt.Fprintf(&b, "     from %s:%s\n\n", p.ServerIP, p.DistPath)

	b.WriteString("3. First install only: choose a release stream.\n\n")
	b.WriteString("     1. Place me on the maintenance stream.\n")
	b.WriteString("     2. Place me on the feature stream.\n\n")
	fmt.Fprintf(&b, "   Enter %s for the %s stream", num, name)
	if name == "feature" {
		b.WriteString(" (feature is a superset of maintenance — the common\n")
		b.WriteString("   recommendation, since starting on maintenance and switching later\n")
		b.WriteString("   generates more conflicts than starting on feature).\n\n")
	} else {
		b.WriteString(".\n\n")
	}

	b.WriteString("4. Select the standard product set and its prerequisites:\n\n")
	b.WriteString("     keep *\n")
	b.WriteString("     install standard\n")
	b.WriteString("     install prereqs\n")
	b.WriteString("     keep incompleteoverlays\n\n")

	b.WriteString("5. Start the preinstallation/conflict check:\n\n")
	b.WriteString("     go\n\n")

	b.WriteString("6. If inst reports conflicts, address them now (enter 1), then resolve\n")
	b.WriteString("   each numbered conflict with a lettered choice, e.g. \"conflicts 1a 2b\"\n")
	b.WriteString("   (abbreviation \"c\"). If the list is long, enter \"q\" to stop viewing it,\n")
	b.WriteString("   resolve what's visible, then run \"conflicts\" again for the rest:\n\n")
	b.WriteString("     conflicts <n><choice> <n><choice> ...\n\n")

	b.WriteString("7. Once there are no more conflicts, start the install for real:\n\n")
	b.WriteString("     go\n\n")

	b.WriteString("8. When Inst reports the installation finished:\n\n")
	b.WriteString("     quit\n\n")

	b.WriteString("9. Answer yes when asked \"Ready to restart the system. Restart?\":\n\n")
	b.WriteString("     y\n")

	return b.String()
}

// Commands returns the subset of the inst runbook that inst(1M) can
// genuinely take from a file unattended, one bare command per line,
// suitable for an inst -F selections-file (see the package doc comment,
// "Can inst read commands from a file?"). It covers only product
// selection — the release-stream prompt and conflict resolution have no
// -F directive and are not included; feeding this file still requires
// an operator (or scripted driver) present to answer those and to enter
// go/quit, which is exactly what Generate's runbook is for.
//
// "keep" is Inst's interactive-menu word for "leave unmarked"; the -F
// grammar spells the same thing "don't install"/"don't remove", so
// "keep incompleteoverlays" becomes "don't install incompleteoverlays"
// below. "keep *" itself is omitted: it resets marks accumulated
// through the interactive CD/distribution reading process, which does
// not happen when driving inst from a blank -F file.
func Commands(p Params) string {
	var b strings.Builder
	fmt.Fprintf(&b, "from %s:%s\n", p.ServerIP, p.DistPath)
	b.WriteString("install standard\n")
	b.WriteString("install prereqs\n")
	b.WriteString("don't install incompleteoverlays\n")
	return b.String()
}
