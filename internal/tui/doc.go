// Package tui is the Bubble Tea client: the frame every screen plugs into.
//
// The frame owns layout, the global keymap, the connection to the daemon and the event
// subscription. Screens own their content and nothing else.
//
// The package holds no domain logic. It renders api.Service results and sends api.Service calls,
// so anything it appears to need from core is a missing method on Service rather than a reason
// to reach past it — that seam is what keeps a future GUI cheap.
package tui
