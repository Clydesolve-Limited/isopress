// Package catalog is the only component that issues SQL.
//
// Per DESIGN_SPEC 1.2 and 1.3, it owns schema creation (schema.go),
// forward migrations (migrate.go), transaction helpers, the advisory flock
// lock (lock.go), and the VACUUM INTO snapshot export (export.go). Services
// depend on catalog interfaces, never on concrete SQL drivers.
package catalog
