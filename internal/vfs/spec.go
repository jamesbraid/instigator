package vfs

// LayerSpec is one ordered contribution to an install set: a source
// reference and the distribution directory within it to merge.
//
// Source is the layer's source reference - a local path (an SGI EFS disc
// image, or a pre-extracted read-only directory) or an http(s) URL - which
// the Resolver turns into a read-only filesystem. An image is opened once
// and shared across every view of it; a directory is opened with os.OpenRoot
// so served paths cannot escape it through a symlink.
//
// Base rebases the layer within that source: Dist and Stand are joined under
// it, so a source whose tree sits below a subdirectory - an extracted
// archive, say - names that subdirectory here and the rest of the layer
// reads as if it were the root. Empty means the source root.
//
// Dist names the distribution directory inside the (possibly rebased)
// source, and it always lands on the set's own dist. Ordinary media call it
// "dist", which is the default when Dist is empty. A version-stub disc keeps
// its real catalog somewhere else - "dist6.5" at the root, or "dist/dist6.5"
// hidden behind a .redirect - and naming it here rebases it, so inst only
// ever sees /<set>/dist however the media were laid out.
//
// Boot marks the one layer per set whose stand directory is served at
// /<set>/stand, where the PROM fetches fx.64. Stand names it, defaulting to
// "stand"; only a bootable set needs one; sa and the miniroot live under
// dist and merge like everything else.
//
// Name identifies the layer in origins, collision winners, and the startup
// report.
type LayerSpec struct {
	Name   string
	Source string
	Base   string
	Dist   string
	Stand  string
	Boot   bool
}

// SetSpec is one logical install set: an ordered list of layers merged into
// /<Name>/dist, plus the exact collision winners that resolve differing
// files. A set with a boot layer also serves /<Name>/stand.
//
// Layers merge in order. Directories union; a regular file present in more
// than one layer must be byte-identical (its origin then being the earliest
// contributing layer) unless Collisions names a winner. Collisions maps a
// full logical tree path (for example "applications/dist/inst.README") to
// the Name of the layer whose copy wins; a differing collision with no
// matching entry fails Build, so the served bytes are never guessed.
type SetSpec struct {
	Name       string
	Layers     []LayerSpec
	Collisions map[string]string
}
