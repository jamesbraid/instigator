package vfs

// LayerSpec is one ordered contribution to an install set: a source
// filesystem and the distribution directory within it to merge.
//
// Exactly one of Image and Dir is set. Image is an SGI EFS disc image,
// opened once and shared across every view of it. Dir is a pre-extracted
// read-only directory, opened with os.OpenRoot so served paths cannot
// escape it through a symlink.
//
// Dist names the distribution directory inside that source, and it always
// lands on the set's own dist. Ordinary media call it "dist", which is the
// default when Dist is empty. A version-stub disc keeps its real catalog
// somewhere else - "dist6.5" at the root, or "dist/dist6.5" hidden behind
// a .redirect - and naming it here rebases it, so inst only ever sees
// /<set>/dist however the media were laid out.
//
// Boot marks the one layer per set whose stand directory is served at
// /<set>/stand, where the PROM fetches fx.64. Only a bootable set needs
// one; sa and the miniroot live under dist and merge like everything else.
//
// Name identifies the layer in origins, collision winners, and the startup
// report.
type LayerSpec struct {
	Name  string
	Image string
	Dir   string
	Dist  string
	Boot  bool
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
