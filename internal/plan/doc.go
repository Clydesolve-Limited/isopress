// Package plan implements the planning engine: capacity profile resolution,
// the mandatory-only chunker, the worst-fit-decreasing balancer, and the plan
// reporter that quantifies media count and locked-gap waste
// (DESIGN_SPEC 1.3, section 4). Planning is metadata-only and never reads
// source bytes.
package plan
