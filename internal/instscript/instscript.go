// Package instscript generates the IRIX inst(1M) install runbook that
// instigator serves alongside a distribution: the exact command
// sequence an operator (or a scripted serial-console driver) types at
// the Inst> prompt to take a fresh machine from PROM to a booted IRIX
// 6.5.x install, with the server's real address and the served install
// sets' real paths filled in — no placeholders.
//
// # The four-install-set contract
//
// instigator serves a fixed profile of install sets in a fixed order —
// the base release, then foundations, applications, and development —
// each as its own dist tree at its own served path. inst has no way to
// discover that grouping on its own, so the runbook and Commands both
// open every set explicitly: "from" on the first set, then "open" for
// each set after it. There is no synthesized "one from covers the set"
// shortcut and no reliance on inst following a generated
// ".related_dists" — that file (see RelatedDists) is served purely as a
// menu aid for an operator browsing by hand; it does not make inst open
// anything by itself.
//
// Once every set is open, one fixed selection block runs regardless of
// which sets are present: clear any stale prior selection, keep
// everything, install the standard product set, keep incomplete
// overlays (products with an unopened distribution) rather than let
// inst try to install placeholder stubs for them, drop the Java dev
// kit and plugin overlays as unwanted bulk, then list conflicts for the
// operator to review. This replaces the old per-install release-stream
// prompt ("install feature"/"install maint") entirely — that choice
// doesn't exist under this contract.
//
// inst(1M)'s "admin source <host>:<path>" command loads a remote text
// file and replays each line as if typed at the Inst> prompt, using the
// same interactive vocabulary as the Main Menu — "from", "open",
// "keep", "install", "remove", "conflicts" — not a distinct file
// syntax. Commands' output is exactly what admin source can safely
// replay unattended. Two things still can't be scripted this way, so
// they're left to the operator (or a scripted console driver) in
// Generate's runbook: reviewing what "conflicts" actually reports, and
// the "go" that starts the real install. Both intentionally stop short
// of "quit" and the post-install restart prompt too — this package's
// job ends once product selection and conflict listing are underway.
package instscript

import (
	"fmt"
	"strings"
)

// Params fills in an inst runbook with one server's real configuration.
type Params struct {
	// ServerIP is the netinstall server's address, used as the remote
	// host in every "from"/"open" command.
	ServerIP string
	// Sets holds the served dist path of every enabled install set, in
	// open order, e.g. ["/6.5.30/dist", "/foundations/dist",
	// "/applications/dist", "/development/dist"]. Sets[0] is opened with
	// "from"; every later entry is opened with "open". Sets must not be
	// empty.
	Sets []string
	// BootPath is the served path of the fx partitioner the PROM boots
	// over the network, e.g. "/6.5.30/stand/fx.64". Empty if no boot
	// artifact exists for this profile, in which case Generate omits the
	// PROM boot line entirely rather than guess at one.
	BootPath string
	// RemoteDir is the value to enter at the PROM's "Remote Directory"
	// prompt, WITH the trailing slash the PROM requires, e.g.
	// "6.5.30/dist/". Printed exactly as given.
	RemoteDir string
}

// setName recovers a set's short name from its served dist path, e.g.
// "/6.5.30/dist" -> "6.5.30", for use in a RelatedDists line.
func setName(setPath string) string {
	name := strings.TrimSuffix(setPath, "/")
	name = strings.TrimSuffix(name, "/dist")
	name = strings.TrimPrefix(name, "/")
	return name
}

// Commands returns the exact sequence inst(1M) can load unattended via
// "admin source <host>:<path>": one bare command per line, trailing
// newline. It opens every set in Params.Sets — "from" on the first,
// "open" on each after it — then runs the fixed selection block:
//
//	return
//	keep *
//	install standard
//	keep incompleteoverlays
//	remove java_dev*
//	remove java2_plugin*
//	conflicts
//
// This never includes "install feature", "install maint", "install
// prereqs", "go", or "quit" — the release-stream prompt doesn't exist
// under the four-set contract, and admin source has no way to trigger
// the install or resolve a conflict from a file. Reviewing the
// "conflicts" output and typing "go" remain the operator's step; see
// Generate.
//
// instigator serves this same byte sequence as both inst.init (so a
// machine that PXE/bootp-boots straight into inst picks it up
// automatically) and the admin-source file an operator loads by hand,
// so Commands is the single source for both.
func Commands(p Params) string {
	var b strings.Builder
	fmt.Fprintf(&b, "from %s:%s\n", p.ServerIP, p.Sets[0])
	for _, set := range p.Sets[1:] {
		fmt.Fprintf(&b, "open %s:%s\n", p.ServerIP, set)
	}
	b.WriteString("return\n")
	b.WriteString("keep *\n")
	b.WriteString("install standard\n")
	b.WriteString("keep incompleteoverlays\n")
	b.WriteString("remove java_dev*\n")
	b.WriteString("remove java2_plugin*\n")
	b.WriteString("conflicts\n")
	return b.String()
}

// Generate returns the full install runbook: a header, the PROM section
// (boot line, when there is one, and the Remote Directory value), then
// the Inst> section that opens every set explicitly and runs the fixed
// selection block, then a closing note that the operator reviews what
// "conflicts" reports and types "go" once satisfied. It is
// copy-pasteable at an IRIX serial console.
func Generate(p Params) string {
	var b strings.Builder

	fmt.Fprintf(&b, "IRIX network install runbook\n")
	fmt.Fprintf(&b, "server %s, %d install set(s)\n", p.ServerIP, len(p.Sets))
	b.WriteString(strings.Repeat("=", 40))
	b.WriteString("\n\n")

	b.WriteString("This is an operator runbook, not an unattended installer: inst(1M)'s\n")
	b.WriteString("\"admin source\" file mechanism has no way to resolve package conflicts\n")
	b.WriteString("or trigger the install, so a human (or a scripted serial-console\n")
	b.WriteString("driver) needs to be present for those steps. See Commands() for the\n")
	b.WriteString("subset inst can load from a file unattended, up through opening every\n")
	b.WriteString("set and listing conflicts.\n\n")

	step := 1

	if p.BootPath != "" {
		fmt.Fprintf(&b, "%d. PROM command monitor — boot the disk partitioner over the network:\n\n", step)
		fmt.Fprintf(&b, "     boot -f bootp():%s\n\n", p.BootPath)
		fmt.Fprintf(&b, "   At the PROM's Remote Directory prompt, enter:\n\n")
		fmt.Fprintf(&b, "     %s\n\n", p.RemoteDir)
		b.WriteString("   Partition the disk with fx, then let it continue into the miniroot\n")
		b.WriteString("   installer and the Inst> prompt.\n\n")
		step++
	}

	fmt.Fprintf(&b, "%d. At the Inst> prompt, open every install set. The first set uses\n", step)
	b.WriteString("   \"from\"; each additional set uses \"open\":\n\n")
	fmt.Fprintf(&b, "     from %s:%s\n", p.ServerIP, p.Sets[0])
	for _, set := range p.Sets[1:] {
		fmt.Fprintf(&b, "     open %s:%s\n", p.ServerIP, set)
	}
	b.WriteString("\n")
	b.WriteString("   Each served set also carries a .related_dists file listing the\n")
	b.WriteString("   others, for an operator browsing the Inst \"Open Dist\" menu by hand —\n")
	b.WriteString("   it does not open anything by itself. This runbook opens every set\n")
	b.WriteString("   explicitly instead of relying on it.\n\n")
	step++

	fmt.Fprintf(&b, "%d. Clear any stale prior selection and select the standard product\n", step)
	b.WriteString("   set, keeping incomplete overlays and dropping the Java dev kit and\n")
	b.WriteString("   plugin overlays:\n\n")
	b.WriteString("   Either type the lines below directly, or save Commands()'s output to\n")
	b.WriteString("   a file on the server and load it in one shot with\n")
	b.WriteString("   \"admin source <host>:<path>\":\n\n")
	b.WriteString("     return\n")
	b.WriteString("     keep *\n")
	b.WriteString("     install standard\n")
	b.WriteString("     keep incompleteoverlays\n")
	b.WriteString("     remove java_dev*\n")
	b.WriteString("     remove java2_plugin*\n\n")
	step++

	fmt.Fprintf(&b, "%d. List conflicts and review them:\n\n", step)
	b.WriteString("     conflicts\n\n")
	step++

	fmt.Fprintf(&b, "%d. Once you're satisfied with the selection, start the install:\n\n", step)
	b.WriteString("     go\n")

	return b.String()
}

// RelatedDists returns the ".related_dists" menu-aid content served
// alongside sets[0]: one "../../<setname>/dist" line per OTHER set in
// sets, in order, trailing newline. It is a hint for an operator
// browsing the Inst "Open Dist" menu by hand — inst does not read or
// follow it on its own, and this package never opens a set by relying
// on it; see Commands and Generate, which open every set with an
// explicit "from"/"open" command instead.
func RelatedDists(sets []string) string {
	var b strings.Builder
	for _, set := range sets[1:] {
		fmt.Fprintf(&b, "../../%s/dist\n", setName(set))
	}
	return b.String()
}
