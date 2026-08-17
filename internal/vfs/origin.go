package vfs

import "errors"

// ErrNotFound is returned for a logical path that does not resolve in the
// assembled tree.
var ErrNotFound = errors.New("not found")

// OriginKind is where a served file's bytes come from.
type OriginKind int

const (
	// OriginImage is a file inside a configured EFS disc image.
	OriginImage OriginKind = iota + 1
	// OriginDirectory is a file inside a configured read-only directory
	// layer, opened with os.OpenRoot.
	OriginDirectory
	// OriginGenerated is a file instigator synthesizes in memory: the
	// inst.init command file, the admin-source copy, .related_dists, and
	// the runbook. It has no backing media.
	OriginGenerated
)

func (k OriginKind) String() string {
	switch k {
	case OriginImage:
		return "image"
	case OriginDirectory:
		return "directory"
	case OriginGenerated:
		return "generated"
	default:
		return "unknown"
	}
}

// Origin is the resolved provenance of one served path: which configured
// layer or generator produced its bytes, and where within that source the
// bytes live. Every file in the tree resolves to exactly one Origin -
// generated files included - so a caller (the served-file manifest log,
// the install recorder) can always name what it just read.
//
// Source is the configured layer name (image and directory origins) or the
// generator name (generated origins). Path is the source-relative path -
// the in-image or in-directory path - and is empty for a generated file,
// whose bytes exist only in memory.
type Origin struct {
	Kind   OriginKind
	Source string
	Path   string
}
