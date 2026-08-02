// This boundary keeps root `go ... ./...` commands from traversing Go source
// bundled inside npm dependencies under node_modules. Fleet has no Go packages
// in web/; the frontend remains managed by package.json/package-lock.json.
module github.com/ElcanoTek/fleet/web

go 1.26.5
