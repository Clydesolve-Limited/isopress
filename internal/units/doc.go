// Package units implements size parsing (decimal GB vs binary GiB per
// DESIGN_SPEC 4.1) and the human-readable formatting used by the plan report
// and the plain progress renderer, plus the media capacity profiles with
// their nominal and usable figures (DESIGN_SPEC 1.3, sections 4.1 and 9.3),
// and the canonical image-capacity estimator shared by the planner's
// balancer and the builder's pre-check (sections 4.3 and 6.3).
package units
