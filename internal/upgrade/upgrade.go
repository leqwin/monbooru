// Package upgrade holds the "this source serves a file we do not have"
// predicate in its two spellings - one over a loaded origin, one over an
// image_sources row - so the detail page's [upgrade] action, the replace
// handler's gate, and the `upgrade:` filter cannot disagree about what
// counts as a candidate.
package upgrade

import "github.com/monbooru/monbooru/internal/models"

// Eligible reports whether this origin's file is known (recorded hash
// mismatch) or presumed (similarity match with no hash claim to compare)
// to differ from the local one. A verified match withholds it: an upgrade
// would no-op. A kept origin is the operator's ruling and outranks both.
func Eligible(s models.ImageSource) bool {
	if s.URL == "" || s.UpgradeKept {
		return false
	}
	return s.MD5Match == "differ" || (s.Similarity > 0 && s.MD5Match == "")
}

// CandidateWhere is Eligible as SQL over an image_sources row under the
// given alias. idx_image_sources_upgradable in db.Bootstrap carries the
// same predicate; keep the two in step or the planner stops using it.
func CandidateWhere(alias string) string {
	a := alias + "."
	return a + "url <> '' AND " + a + "upgrade_kept = 0 AND (" +
		a + "md5_match = 'differ' OR (" + a + "similarity > 0 AND " + a + "md5_match = ''))"
}
