module rbevalgraph

go 1.21.0

toolchain go1.22.2

// gotk4 v0.4.x needs glib 2.82+. Ubuntu 24.04 ships glib 2.80, where it fails
// to build with "could not determine what C.g_get_monotonic_time_ns refers to"
// and similar. v0.3.1 is the last release that compiles against 2.80.
require (
	github.com/diamondburned/gotk4/pkg v0.3.1
	github.com/notnil/chess v1.10.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/KarpelesLab/weak v0.1.1 // indirect
	go4.org/unsafe/assume-no-moving-gc v0.0.0-20231121144256-b99613f794b6 // indirect
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c // indirect
)
