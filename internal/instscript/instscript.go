// Package instscript generates the command file loaded by IRIX inst. It
// opens supplemental sets in order, reopens the primary set last, applies
// the tested selections, and starts installation.
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
	// open order, e.g. ["/6.5.30/dist", "/foundations/dist"]. Must not be
	// empty; Sets[0] is the primary release, already open, and reopened
	// last by Commands.
	Sets []string
}

// Commands returns inst(1M)'s admin-source sequence, one bare command per
// line: open each later set, reopen the primary, then run the fixed
// selection block. It omits "install feature/maint/prereqs", "conflicts",
// and "quit" by design; "go" is safe because the java_dev.sw.base choice
// is already applied by name.
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
