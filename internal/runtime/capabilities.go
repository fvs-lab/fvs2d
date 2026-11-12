package runtime

type Capabilities struct {
	Backend      string
	Available    bool
	MinimumMacOS string
	LazyUnmount  bool
	Diagnostic   string
}
