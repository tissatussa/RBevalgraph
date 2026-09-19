module rbevalgraph

go 1.21

// gotk4 v0.4.x needs glib 2.82+. Ubuntu 24.04 ships glib 2.80, where it fails
// to build with "could not determine what C.g_get_monotonic_time_ns refers to"
// and similar. v0.3.1 is the last release that compiles against 2.80.
require (
	github.com/diamondburned/gotk4/pkg v0.3.1
	github.com/notnil/chess v1.10.0
	gopkg.in/yaml.v3 v3.0.1
)
