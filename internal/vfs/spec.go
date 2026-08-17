package vfs

// LayerSpec is one ordered contribution to an install set: a source
// filesystem, the directory within it to draw from, and the directory
// within the logical set to place that under.
//
// Exactly one of Image and Dir is set. Image is an SGI EFS disc image,
// opened once and shared across every view of it. Dir is a pre-extracted
// read-only directory, opened with os.OpenRoot so served paths cannot
// escape it through a symlink.
//
// SourceDir is the subdirectory of the source to map; "." (or "") maps the
// whole root, which is how the first layer of a set contributes stand,
// installtools, and dist together. TargetDir is where the mapped subtree
// lands inside the set: "." for the set root, "dist" for a later layer that
// contributes only its distribution directory. A version-stub disc maps its
// real "dist6.5" SourceDir onto the logical "dist" TargetDir.
//
// Name identifies the layer in origins, collision winners, and the startup
// report.
type LayerSpec struct {
	Name      string
	Image     string
	Dir       string
	SourceDir string
	TargetDir string
}

// SetSpec is one logical install set: an ordered list of layers merged into
// /<Name>, plus the exact collision winners that resolve differing files.
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
