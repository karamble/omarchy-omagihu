# Omagihu FAQ

## Why does omagihu build from source?

The plugin ships QML and Go source, not binaries. `bin/` is not committed, so
the daemon and the two helper commands are compiled once on the machine that
runs them. Building needs the Go toolchain and git.

## The build says the Go toolchain is not on PATH

The preflight tells you which of three situations you are in. This page has the
commands for each.

### mise has Go, but this shell cannot see it

mise activates per shell, so a shell opened before you installed Go does not
have it. Open a new terminal and build again, or point the build straight at
the toolchain mise already has:

```
make GO=$(mise which go)
```

### mise is installed, but has no Go

Omarchy ships mise, and it needs no root:

```
mise use -g go@latest
```

Then open a new terminal and build again.

### No mise, no Go

Install Go with the system package manager:

```
sudo pacman -S go
```

Or install mise first and use the previous answer, which keeps toolchains in
your home directory instead of system wide.

## Which Go version?

The minimum is the version in `go.mod`. The preflight prints it.

The build sets `GOTOOLCHAIN=local`, so it uses the toolchain you installed
rather than downloading a different one over the network. If your Go is older
than `go.mod` asks for, the build says so instead of silently fetching another.

## Why is CGO off?

Go enables cgo whenever it finds a C compiler and disables it when it does not,
so the same source produces different binaries on different machines and picks
a different DNS resolver with it. The build pins it off: nothing here needs a C
toolchain, and the pure Go resolver reads `resolv.conf` directly.

The race detector does need cgo, so `make test` turns it back on for that one
target.

## The panel says omagihu is not built yet

The QML is installed but `bin/` is empty. Run `make` in the plugin directory,
then restart the shell.
