// Package instscript generates the IRIX inst(1M) command file that
// instigator serves alongside the configured install sets.
//
// # The install-set contract
//
// instigator serves a fixed profile of install sets in a fixed order —
// the configured release and supplemental sets in order, each as its own
// dist tree at its own served path. The miniroot opens
// the base release before the operator runs "admin source". Commands
// opens the additional sets first, then reopens the base release last so
// its 6.5.30 package metadata wins over older foundation packages with the
// same product names. There is no reliance on
// inst following a generated
// ".related_dists" — that file (see RelatedDists) is served purely as a
// menu aid for an operator browsing by hand; it does not make inst open
// anything by itself.
//
// Once every set is open, one fixed selection block runs regardless of
// which sets are present: install the standard product set and apply the
// known Java package-name choice. It makes no removal choices for the
// operator and does not use positional conflict resolution.
//
// inst(1M)'s "admin source <host>:<path>" command loads a remote text
// file and replays each line as if typed at the Inst> prompt, using the
// same interactive vocabulary as the Main Menu — "open", "keep",
// "install", and "conflicts" — not a distinct file syntax.
// Commands' output is exactly what admin source can safely replay
// unattended. The operator still reviews the final conflicts report and
// types "go" to start the real install. The file intentionally stops short
// of "quit" and the post-install restart prompt.
package instscript

import (
	"fmt"
	"strings"
)

// Params fills in an inst command file with one server's configuration.
type Params struct {
	// ServerIP is the netinstall server's address, used as the remote
	// host in every "open" command.
	ServerIP string
	// Sets holds the served dist path of every enabled install set, in
	// open order, e.g. ["/6.5.30/dist", "/foundations/dist"]. The miniroot has already
	// opened Sets[0]; every later entry is opened with "open", then Sets[0] is
	// reopened last to make the release overlay authoritative. Sets must not be
	// empty.
	Sets []string
}

// Commands returns the exact sequence inst(1M) can load unattended via
// "admin source <host>:<path>": one bare command per line, trailing
// newline. The primary set is already open, so it opens each later set
// in Params.Sets, then runs the fixed selection block:
//
//	done
//	keep *
//	install standard
//	keep java_dev.sw.base
//
// This never includes "install feature", "install maint", "install
// prereqs", "conflicts", or "quit" — positional conflict resolution is
// intentionally left out. The final "go" is safe to replay because the
// known package choice is applied by name before it; see docs/install.md.
//
// instigator serves this byte sequence as /install.cmds, which the
// operator loads with "admin source".
func Commands(p Params) string {
	var b strings.Builder
	for _, set := range p.Sets[1:] {
		fmt.Fprintf(&b, "open %s:%s\n", p.ServerIP, set)
	}
	// The miniroot has the primary source open already, but a later
	// foundation source can select an older same-named product. Reopening
	// the primary source makes the 6.5.30 release metadata win.
	fmt.Fprintf(&b, "open %s:%s\n", p.ServerIP, p.Sets[0])
	// Finish the source-selection menu before processing selections.
	b.WriteString("done\n")
	b.WriteString("keep *\n")
	b.WriteString("install standard\n")
	// The two Java base products conflict. Keep the development product
	// deselected so the standard Java EOE runtime remains selected.
	b.WriteString("keep java_dev.sw.base\n")
	b.WriteString("go\n")
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
