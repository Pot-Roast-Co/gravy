package host

// LocalHost must satisfy Host, and its process must satisfy Process. A compile-time assertion
// catches an interface drift immediately rather than at the first wiring-up.
var (
	_ Host    = (*LocalHost)(nil)
	_ Process = (*localProcess)(nil)
	_ FS      = localFS{}
)
