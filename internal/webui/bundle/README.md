# internal/webui/bundle

`dist/` here is the Vite build of `web/` (`make web` writes it; git ignores
it) and is compiled into the fleet binary with `go:embed`. This README is
committed so the embed pattern always matches: a `go build` without a web
build still compiles, and that binary answers page routes with a 503 that
says to run `make build`. See docs/adr/0091-go-serves-the-browser-tier.md.
