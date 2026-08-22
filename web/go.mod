// This boundary keeps root `go ... ./...` commands from traversing Go source
// bundled inside npm dependencies under node_modules. Fleet has no Go packages
// in web/; the frontend remains managed by package.json/package-lock.json.
module github.com/ElcanoTek/fleet/web

// Major.minor only, deliberately: this module has no packages, so pinning a
// PATCH here just created a second copy of the root go.mod's version that had
// to be bumped in lockstep for no benefit.
go 1.27
