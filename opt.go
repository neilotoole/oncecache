package oncecache

// Opt is a functional option accepted by [New]. See [Name], [OnFill],
// [OnEvict], [OnHit], [OnMiss], [OnEvent], and [Log] for the built-in
// implementations.
//
// Opt is a closed interface: third-party packages cannot implement it
// (the marker method is unexported). This keeps the dispatch in
// (*Cache).applyOpts total: every Opt is one of the built-in kinds and
// can be handled exhaustively.
type Opt interface {
	// optioner is an unexported marker method that closes the Opt
	// interface. It also unifies the two internal applier kinds
	// (optApplier and concreteOptApplier) under a single parent.
	optioner()
}

// optApplier is the internal [Opt] variant whose apply method touches the
// parameterized fields of [Cache]. Used by opts that need access to K/V
// (callbacks, event channels).
type optApplier[K comparable, V any] interface {
	Opt
	apply(c *Cache[K, V])
}

// concreteOptApplier is the internal [Opt] variant for opts that only
// configure non-parameterized fields — i.e., they can be written without
// spelling out K and V. Used by [Name].
type concreteOptApplier interface {
	Opt
	applyConcrete(c *concreteCache)
}

// concreteCache is a view over the non-parameterized state of [Cache]. It
// is passed to concreteOptApplier.applyConcrete and holds pointers so
// options can write through without the cache exposing its private fields
// directly.
type concreteCache struct {
	name *string
}

var _ concreteOptApplier = (*Name)(nil)

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

// applyConcrete writes the [Name] through the concreteCache view.
func (o Name) applyConcrete(c *concreteCache) {
	*c.name = string(o)
}

// optioner satisfies the [Opt] marker interface.
func (o Name) optioner() {}
