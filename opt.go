package oncecache

// Opt is a functional option accepted by [New]. See [Name], [OnFill],
// [OnEvict], [OnHit], [OnMiss], [OnEvent], and [Log] for the built-in
// implementations.
//
// Opt is a closed interface: third-party packages cannot implement it
// (the marker method is unexported). This lets (*Cache).applyOpts dispatch
// exhaustively over the built-in opt kinds without an "unknown Opt" escape
// hatch leaking into the public API.
type Opt interface {
	// optioner is an unexported marker method that closes the Opt
	// interface.
	optioner()
}

// optApplier is the internal [Opt] variant whose apply method touches the
// parameterized fields of [Cache]. Used by [OnFill], [OnEvict], [OnHit],
// [OnMiss], and [OnEvent] — opts that need access to K/V to manipulate
// per-cache callback slices or event channels. Non-parameterized opts
// ([Name], [Log]) bypass this interface and are dispatched directly by
// applyOpts.
type optApplier[K comparable, V any] interface {
	Opt
	apply(c *Cache[K, V])
}

// Name is an [Opt] for [New] that sets the cache's display name, accessible
// via [Cache.Name] and used by [Cache.String] and [Cache.LogValue] for
// readable debug/log output. When the same cache instance is configured
// with [Name] more than once, the last value wins.
//
//	c := oncecache.New[int, string](fetch, oncecache.Name("users"))
//
// If [Name] is not specified, a random name like "cache-38a2b7d4" is
// generated so every cache has a distinct identifier in logs.
type Name string

// optioner satisfies the [Opt] marker interface.
func (o Name) optioner() {}
