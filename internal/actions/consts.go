package actions

// Shared structured-log field keys used across the actions package, extracted
// to satisfy goconst (repeated string literals → named constants).
const (
	fieldContainer = "container"
	fieldImage     = "image"
	fieldDigest    = "digest"
	fieldOldDigest = "old_digest"
	fieldGreen     = "green"
	fieldRollback  = "rollback"
	fieldCanonical = "canonical"
	fieldSelf      = "self"
)
