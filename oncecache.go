// Package oncecache contains a strongly-typed, concurrency-safe, context-aware,
// dependency-free, in-memory, on-demand object [Cache], focused on write-once,
// read-often ergonomics.
//
// # Model
//
// Construct a [Cache] via [New] with a [FetchFunc]. The first [Cache.Get] for
// a given key runs the fetch func and stores the result; subsequent [Cache.Get]
// calls return that stored result without reinvoking fetch. Entries may also
// be populated externally via [Cache.MaybeSet], and removed via [Cache.Delete]
// or [Cache.Clear], after which they may be populated afresh.
//
// A cache entry is a (key, value, error) triple. An entry with a non-nil error
// is still a valid, fully-populated entry; subsequent [Cache.Get] calls return
// the stored error without reinvoking fetch. The "once" in oncecache refers to
// this once-per-entry-lifetime population guarantee.
//
// # Scope
//
// oncecache is targeted at write-once, read-often situations — where the
// value for a key is expensive to compute or fetch, is likely to be read many
// times, and is not expected to change. It is not a general-purpose cache:
// there is no TTL, no size bound, no background reaper, and no LRU eviction.
// Callers manage entry lifetime explicitly via [Cache.Delete] and
// [Cache.Clear].
//
// # Events and callbacks
//
// The package provides an event/callback mechanism useful for logging,
// metrics, and cache propagation across composite caches. Synchronous
// callbacks are registered with [OnFill], [OnEvict], [OnHit], and [OnMiss];
// channel-based async delivery is available via [OnEvent]; and [Log]
// configures basic [slog.Logger] integration.
//
// # Concurrency
//
// [Cache] is safe for concurrent use by multiple goroutines. For any given
// key, [FetchFunc] is invoked at most once per entry lifetime: if multiple
// goroutines call [Cache.Get] for the same unfilled key, the first caller
// runs fetch and subsequent callers block until the fetch completes, then all
// receive the same result.
//
// Synchronous callbacks run on the goroutine that triggered the event. They
// must not touch the same cache for the same key (that re-enters the entry's
// [sync.Once] and deadlocks). For long-running callbacks, prefer [OnEvent]
// with a buffered channel.
package oncecache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/gob"
	"fmt"
	"hash/crc32"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"

	"golang.org/x/exp/maps"
)

// FetchFunc is called by [Cache.Get] to fill an unpopulated cache entry. It
// returns the value for key, or an error if the fetch failed.
//
// Errors are cached just like successful values: a subsequent [Cache.Get] for
// the same key returns the same error without reinvoking FetchFunc, until the
// entry is removed via [Cache.Delete] or [Cache.Clear].
//
// For any given key, FetchFunc is invoked at most once per entry lifetime,
// regardless of concurrent [Cache.Get] calls. Concurrent callers with the
// same key block until the in-flight fetch completes, then all receive the
// same (val, err) result.
//
// The ctx passed to FetchFunc is decorated with the source [Cache] and can
// be retrieved via [FromContext]. Cancellation of ctx does not automatically
// abort the fetch — FetchFunc should honor ctx.Done() internally if needed.
//
// FetchFunc should not call [Cache.Get] or [Cache.MaybeSet] on the same
// [Cache] for the same key it was invoked for; doing so re-enters the
// entry's [sync.Once] and deadlocks.
//
// If FetchFunc panics, the panic propagates to the triggering [Cache.Get]
// call; the entry is recorded as "once"-consumed, and subsequent [Cache.Get]
// calls return the zero value with a nil error, without reinvoking FetchFunc
// (until the entry is explicitly evicted).
type FetchFunc[K comparable, V any] func(ctx context.Context, key K) (val V, err error)

// New constructs a [Cache] parameterized over key type K and value type V. The
// fetch func is invoked on-demand by [Cache.Get] to populate an entry for a
// given key; alternatively, entries may be populated externally via
// [Cache.MaybeSet]. Either way, each entry is populated only once, until it
// is explicitly evicted via [Cache.Delete] or [Cache.Clear].
//
// Opts configures the cache:
//   - [Name] sets the cache's display name (used by [Cache.String] and
//     [Cache.LogValue]). If omitted, a random name like "cache-38a2b7d4" is
//     generated.
//   - [OnFill], [OnEvict], [OnHit], [OnMiss] register synchronous callbacks.
//   - [OnEvent] configures channel-based async event delivery.
//   - [Log] wires up a [slog.Logger] for basic logging.
//
// Nil [Opt] values in opts are ignored. Opts are applied in order; later opts
// may override earlier ones (for [Name]), or add to them (for callbacks).
//
// The returned [Cache] is safe for concurrent use by multiple goroutines.
//
// Passing a nil fetch is permitted but limits the cache to [Cache.MaybeSet]
// population: any [Cache.Get] for an unpopulated key will panic.
func New[K comparable, V any](fetch FetchFunc[K, V], opts ...Opt) *Cache[K, V] {
	c := &Cache[K, V]{
		name:    randomName(),
		entries: map[K]*entry[K, V]{},
		fetch:   fetch,
	}

	c.applyOpts(opts)

	// Fast-path selection: if no Get-emitted callbacks are registered, we can
	// skip the callback-iteration bookkeeping inside once.Do. The fast/slow
	// path is chosen once at construction time so Get/MaybeSet pay no
	// per-call cost for deciding.
	if len(c.onFill)+len(c.onMiss)+len(c.onHit) == 0 {
		c.getValueFn = getValueFast[K, V]
	} else {
		c.getValueFn = getValueSlow[K, V]
	}

	if len(c.onFill) == 0 {
		c.maybeSetValueFn = maybeSetValueFast[K, V]
	} else {
		c.maybeSetValueFn = maybeSetValueSlow[K, V]
	}

	return c
}

// applyOpts applies functional options to c.
//
// There are three dispatch paths, reflecting the three flavors of [Opt] in
// this package:
//
//  1. [optApplier][K, V] — the common case. The opt knows the cache's
//     K and V type parameters and can touch parameterized fields directly
//     (e.g. the callback slices). Used by [OnFill], [OnEvict], [OnHit],
//     [OnMiss], [OnEvent].
//
//  2. [concreteOptApplier] — opts that configure only non-parameterized
//     fields. These cannot carry K/V and so receive a [concreteCache] view
//     that exposes pointers to the non-parameterized state. Used by [Name].
//
//  3. [logOptConfig] — [Log] returns a non-parameterized value (so callers
//     can write oncecache.Log(...) without spelling K and V). Here we
//     reconstitute it as a typed logOpt[K, V] and apply it. This third
//     branch is a known wart; see the review notes.
//
// Unrecognized Opt types panic to surface programmer errors early.
func (c *Cache[K, V]) applyOpts(opts []Opt) {
	for _, opt := range opts {
		if isNil(opt) {
			continue
		}

		// 1. Type-parameterized opts.
		if applier, ok := opt.(optApplier[K, V]); ok {
			if _, ok = opt.(concreteOptApplier); ok {
				// An Opt should implement exactly one of the two interfaces;
				// implementing both means the dispatch below would be
				// ambiguous.
				panic(fmt.Sprintf("Opt type %T must not implement both optApplier and concreteOptApplier", opt))
			}
			applier.apply(c)
			continue
		}

		// 2. Non-parameterized opts touching concrete fields only.
		cc := &concreteCache{name: &c.name}
		if applier, ok := opt.(concreteOptApplier); ok {
			applier.applyConcrete(cc)
			continue
		}

		// 3. [Log]: reconstitute as logOpt[K, V] and apply.
		if cfg, ok := opt.(logOptConfig); ok {
			(*logOpt[K, V])(&cfg).apply(c)
			continue
		}

		panic(fmt.Sprintf("Invalid Opt type %T", opt))
	}
}

// Cache is a concurrency-safe, in-memory, on-demand cache that ensures a
// given cache entry is populated only once, either implicitly via [Cache.Get]
// (which invokes the [FetchFunc] supplied to [New]), or externally via
// [Cache.MaybeSet]. A cache entry can be explicitly removed via [Cache.Delete]
// or [Cache.Clear], after which it may be populated afresh.
//
// A cache entry is a (key, value, error) triple. An entry with a non-nil
// error is still a valid, fully-populated entry: subsequent [Cache.Get] calls
// return that error without reinvoking fetch. Entry population occurs only
// once (hence "oncecache") unless the entry is evicted.
//
// # Concurrency
//
// All methods are safe for concurrent use by multiple goroutines. For any
// given key, the fetch func runs at most once per entry lifetime; concurrent
// [Cache.Get] calls for an unfilled key block until the in-flight fetch
// completes. Callbacks registered via [OnFill], [OnEvict], [OnHit], or
// [OnMiss] run synchronously on the goroutine that triggered the event.
//
// # Not a general-purpose cache
//
// There is no TTL, no size bound, no background reaper, and no LRU eviction.
// Memory grows monotonically as new keys are populated; callers manage
// lifetime explicitly via [Cache.Delete] and [Cache.Clear].
//
// The zero value is not usable; construct a [Cache] via [New].
type Cache[K comparable, V any] struct {
	// fetch populates entries on Cache.Get when the key is absent.
	fetch FetchFunc[K, V]

	// entries holds the live cache map. Protected by mu for insertion,
	// lookup, and deletion. The per-entry val/err fields are further
	// protected by the entry's own sync.Once + filled flag; see entry's
	// doc for the synchronization contract.
	entries map[K]*entry[K, V]

	// maybeSetValueFn and getValueFn are selected at construction time
	// based on which callbacks are registered. The "fast" variants skip
	// callback-iteration overhead when no relevant callbacks exist.
	maybeSetValueFn func(ctx context.Context, e *entry[K, V], key K, val V, err error) bool
	getValueFn      func(ctx context.Context, e *entry[K, V], key K) (V, error)

	// name is set via the Name opt, or a random value if not specified.
	// Read-mostly after construction; may be overwritten by GobDecode.
	name string

	// on* slices are populated at construction time from opts and never
	// mutated thereafter (except by Close, which nils them). Reading
	// their length without the lock is therefore safe in the steady
	// state.
	onFill  []callbackFunc[K, V]
	onEvict []callbackFunc[K, V]
	onHit   []callbackFunc[K, V]
	onMiss  []callbackFunc[K, V]

	// mu guards entries and (transiently) name.
	mu sync.Mutex
}

// Name returns the cache's name, useful in logs and debug output. The name
// is set via the [Name] opt to [New]; otherwise a random name of the form
// "cache-XXXXXXXX" (eight hex digits) is generated.
func (c *Cache[K, V]) Name() string {
	return c.name
}

// Len returns the number of entries in the cache, including entries that
// hold an error and entries whose fill is still in-flight. Safe for
// concurrent use.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// String returns a debug-friendly string representation of the cache in the
// form "<name>[<keyType>, <valType>][<len>]", e.g. "cache-foo[int, string][3]".
// For structured logging, see [Cache.LogValue].
func (c *Cache[K, V]) String() string {
	return fmt.Sprintf("%s[%T, %T][%d]",
		c.name, *new(K), *new(V), c.Len(),
	)
}

// LogValue implements [slog.LogValuer] for [slog.Logger] integration. The
// emitted group contains:
//   - name:    the cache name ([Cache.Name]).
//   - entries: the current entry count ([Cache.Len]).
//   - type:    a nested group with "key" and "val" fields naming the type
//     parameters K and V.
func (c *Cache[K, V]) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", c.name),
		slog.Int("entries", c.Len()),
		slog.Group("type",
			"key", fmt.Sprintf("%T", *new(K)),
			"val", fmt.Sprintf("%T", *new(V)),
		),
	)
}

// Has reports whether the cache contains an entry for key. An entry holding
// a non-nil fill error, and an entry whose fill is still in-flight, both
// count as present. Safe for concurrent use.
func (c *Cache[K, V]) Has(key K) bool {
	c.mu.Lock()
	_, ok := c.entries[key]
	c.mu.Unlock()
	return ok
}

// Keys returns a snapshot of the cache's keys in indeterminate order.
// Modifying the returned slice does not affect the cache. Safe for
// concurrent use.
func (c *Cache[K, V]) Keys() []K {
	c.mu.Lock()
	defer c.mu.Unlock()

	r := make([]K, 0, len(c.entries))
	for k := range c.entries {
		r = append(r, k)
	}
	return r
}

// Clear clears the cache entries, invoking any [OnEvict] callbacks on each
// cache entry. The entry callback order is not specified. The cache is locked
// until Clear (including any callbacks) returns.
//
// If an entry is currently being filled when Clear is called, no [OnEvict]
// callback is invoked for that entry; the in-flight fill is effectively
// orphaned and its [OnFill] callbacks (if any) will still fire.
func (c *Cache[K, V]) Clear(ctx context.Context) {
	if len(c.onEvict) == 0 {
		c.mu.Lock()
		clear(c.entries)
		c.mu.Unlock()
		return
	}

	ctx = NewContext(ctx, c)
	c.mu.Lock()
	for key, e := range c.entries {
		delete(c.entries, key)
		if e == nil {
			continue // Shouldn't be possible
		}
		if !e.filled.Load() {
			continue
		}

		for _, fn := range e.cache.onEvict {
			fn(ctx, key, e.val, e.err)
		}
	}
	c.mu.Unlock()
}

// Delete deletes the entry for the given key, invoking any [OnEvict] callbacks.
// The cache is locked until Delete (including any callbacks) returns.
//
// If the entry is currently being filled when Delete is called, no [OnEvict]
// callback is invoked; the in-flight fill is effectively orphaned and its
// [OnFill] callbacks (if any) will still fire.
func (c *Cache[K, V]) Delete(ctx context.Context, key K) {
	if len(c.onEvict) == 0 {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return
	}

	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return
	}

	delete(c.entries, key)
	if !e.filled.Load() {
		c.mu.Unlock()
		return
	}
	ctx = NewContext(ctx, c)
	for _, fn := range e.cache.onEvict {
		fn(ctx, key, e.val, e.err)
	}
	c.mu.Unlock()
}

// MaybeSet sets the value and fill error for key if the entry is not already
// filled, returning true if the value was set. This allows external code to
// prime the cache or propagate values from another source.
//
// If there's already an entry for key — whether it was populated by
// [Cache.Get], a prior [Cache.MaybeSet], or even if it's still being filled
// by an in-flight fetch — MaybeSet is a no-op and returns false. The err
// argument, when non-nil, is stored alongside val just like a fetch error;
// subsequent [Cache.Get] calls for this key return (val, err) without
// reinvoking fetch.
//
// When MaybeSet does populate the entry, it invokes any [OnFill] callbacks
// synchronously and emits an [OpFill] event via any [OnEvent] channels.
// [OnMiss] is not emitted, since MaybeSet is not a [Cache.Get] miss.
func (c *Cache[K, V]) MaybeSet(ctx context.Context, key K, val V, err error) (ok bool) {
	e := c.getEntry(key)
	return c.maybeSetValueFn(ctx, e, key, val, err)
}

// Get returns the cached value (and fill error) for key. On a cache miss,
// the [FetchFunc] supplied to [New] is invoked to populate the entry, and
// the resulting (val, err) is cached and returned. On a cache hit — including
// an entry whose fill returned an error — the stored (val, err) is returned
// directly without reinvoking fetch.
//
// Callback/event sequence:
//   - Miss: [OnMiss] fires, fetch runs, then [OnFill] fires. [OpMiss] and
//     [OpFill] events are emitted in that order.
//   - Hit: [OnHit] fires; [OpHit] is emitted.
//
// Concurrent Get calls for the same unfilled key block until the in-flight
// fetch completes and then all receive the same (val, err).
//
// If fetch panics, the panic propagates to this Get caller; see [FetchFunc]
// for the post-panic entry state.
func (c *Cache[K, V]) Get(ctx context.Context, key K) (V, error) {
	e := c.getEntry(key)
	return c.getValueFn(ctx, e, key)
}

// getEntry returns the entry for key, creating (but not yet populating) one
// if it doesn't exist. The returned entry's val/err are zero until some
// caller's once.Do completes. Holds c.mu only for the map lookup/insert.
func (c *Cache[K, V]) getEntry(key K) *entry[K, V] {
	c.mu.Lock()
	e, ok := c.entries[key]
	if ok {
		c.mu.Unlock()
		return e
	}

	e = &entry[K, V]{cache: c}
	c.entries[key] = e
	c.mu.Unlock()
	return e
}

// Close clears all entries and detaches all registered callbacks. It is
// idempotent and always returns nil; it is also safe to call on a nil
// [Cache] receiver, which is a no-op.
//
// After Close:
//   - [Cache.Len] returns 0.
//   - [Cache.Get] and [Cache.MaybeSet] still work, but no [OnFill],
//     [OnMiss], [OnHit], or [OnEvict] callbacks will fire (they have been
//     detached), and [OnEvent] channels no longer receive events for this
//     cache.
//   - Subsequent [Cache.Delete] and [Cache.Clear] succeed silently without
//     firing [OnEvict].
//
// [OnEvict] is not invoked for the entries cleared by Close itself.
//
// Close is provided primarily to release callback references (useful when
// callbacks close over large objects) and to empty the cache in one call;
// it does not mark the cache as permanently closed.
func (c *Cache[K, V]) Close() error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.onFill = nil
	c.onEvict = nil
	c.onHit = nil
	c.onMiss = nil
	clear(c.entries)
	return nil
}

// Compile-time assertion that [Cache] satisfies the gob encoder/decoder
// contract. The concrete type parameterization is immaterial; gob cares
// only about the method set.
var (
	_ gob.GobEncoder = (*Cache[int, int])(nil)
	_ gob.GobDecoder = (*Cache[int, int])(nil)
)

// gobData is the on-wire representation of a [Cache]. Only the cache name
// and its filled entries are serialized; the fetch func and callbacks are
// not.
type gobData[K comparable, V any] struct {
	Entries map[K]*gobEntry[K, V]
	Name    string
}

// gobEntry is the on-wire representation of a single cache entry: just the
// value and fill error. The sync.Once and filled flag are reconstructed on
// decode.
type gobEntry[K comparable, V any] struct {
	Val V
	Err error
}

// GobEncode implements [gob.GobEncoder]. Only the cache name and entries are
// encoded. The fetch func and callbacks are not encoded. Entries whose fill is
// still in-flight are omitted from the encoded output.
func (c *Cache[K, V]) GobEncode() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	gbd := &gobData[K, V]{
		Name:    c.name,
		Entries: make(map[K]*gobEntry[K, V], len(c.entries)),
	}

	for k, ent := range c.entries {
		if !ent.filled.Load() {
			continue
		}
		gbd.Entries[k] = &gobEntry[K, V]{Val: ent.val, Err: ent.err}
	}

	buf := &bytes.Buffer{}
	if err := gob.NewEncoder(buf).Encode(gbd); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// GobDecode implements [gob.GobDecoder]. Only the cache name and entries are
// restored from p; the fetch func and callbacks on c are preserved as-is.
// Any pre-existing entries in c are cleared before decoding; decoded entries
// are marked as filled, so subsequent [Cache.Get] calls return them directly
// without invoking fetch.
//
// The error type(s) used in entries with non-nil fill errors must be
// registered with [gob.Register] before encoding/decoding.
//
// GobDecode does not fire [OnEvict] for the pre-existing entries it clears,
// nor [OnFill] for the decoded entries it installs.
func (c *Cache[K, V]) GobDecode(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	gbd := &gobData[K, V]{}
	if err := gob.NewDecoder(bytes.NewReader(p)).Decode(gbd); err != nil {
		return err
	}

	c.name = gbd.Name
	maps.Clear(c.entries)
	for k, e := range gbd.Entries {
		ent := &entry[K, V]{val: e.Val, err: e.Err, cache: c}
		ent.once.Do(func() {}) // Consume the sync.Once
		ent.filled.Store(true)
		c.entries[k] = ent
	}

	return nil
}

// entry is the internal representation of a cache entry. Contrast with the
// external [Entry] type.
//
// Fields val and err are written exactly once, inside once.Do, and must not be
// read by any code path that doesn't either run inside once.Do or observe
// filled.Load() == true. This ordering lets external readers (Delete, Clear,
// GobEncode) skip entries whose fill is still in-flight, avoiding a race with
// the filling goroutine.
type entry[K comparable, V any] struct {
	val    V
	err    error
	cache  *Cache[K, V]
	once   sync.Once
	filled atomic.Bool
}

// maybeSetValueSlow is the MaybeSet core used when OnFill callbacks are
// registered. See maybeSetValueFast for the no-callback path.
func maybeSetValueSlow[K comparable, V any](ctx context.Context, e *entry[K, V], key K, val V, err error) bool {
	var ok bool
	e.once.Do(func() {
		ok = true
		e.val = val
		e.err = err
		e.filled.Store(true)
		ctx = NewContext(ctx, e.cache)
		for _, fn := range e.cache.onFill {
			fn(ctx, key, val, err)
		}
	})
	return ok
}

// maybeSetValueFast is the MaybeSet core used when no OnFill callbacks are
// registered. It skips callback iteration and the NewContext decoration.
func maybeSetValueFast[K comparable, V any](_ context.Context, e *entry[K, V], _ K, val V, err error) bool {
	var ok bool
	e.once.Do(func() {
		e.val = val
		e.err = err
		e.filled.Store(true)
		ok = true
	})
	return ok
}

// getValueSlow is the Get core used when any of the Get-triggered callbacks
// (OnMiss, OnFill, OnHit) are registered. On miss it fires OnMiss, runs
// fetch, marks the entry filled, then fires OnFill — all inside the entry's
// sync.Once. On hit it fires OnHit outside the Once (safe: the Once's
// completion synchronizes the val/err writes for later readers).
func getValueSlow[K comparable, V any](ctx context.Context, e *entry[K, V], key K) (V, error) {
	var miss bool
	e.once.Do(func() {
		miss = true
		ctx = NewContext(ctx, e.cache)
		for _, fn := range e.cache.onMiss {
			fn(ctx, key, e.val, e.err)
		}

		e.val, e.err = e.cache.fetch(ctx, key)
		e.filled.Store(true)

		for _, fn := range e.cache.onFill {
			fn(ctx, key, e.val, e.err)
		}
	})

	if !miss && len(e.cache.onHit) > 0 {
		ctx = NewContext(ctx, e.cache)
		for _, fn := range e.cache.onHit {
			fn(ctx, key, e.val, e.err)
		}
	}

	return e.val, e.err
}

// getValueFast is the Get core used when no Get-triggered callbacks are
// registered, which is the common case. It skips the OnMiss/OnFill/OnHit
// iteration entirely.
func getValueFast[K comparable, V any](ctx context.Context, e *entry[K, V], key K) (V, error) {
	e.once.Do(func() {
		ctx = NewContext(ctx, e.cache)
		e.val, e.err = e.cache.fetch(ctx, key)
		e.filled.Store(true)
	})

	return e.val, e.err
}

// ctxKey is the unexported context key under which cache instances are
// stored by [NewContext] and retrieved by [FromContext].
type ctxKey struct{}

// NewContext returns ctx decorated with [Cache] c. If ctx is nil, a fresh
// [context.Background] is used as the parent.
//
// This is the mechanism by which fetch and callback invocations receive
// access to their source [Cache]: Cache itself calls NewContext before
// dispatching fetch or callbacks, and user code retrieves the cache via
// [FromContext]. Callers seldom need to invoke NewContext directly except
// in tests or to build a ctx carrying a specific cache for downstream code.
func NewContext[K comparable, V any](ctx context.Context, c *Cache[K, V]) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the [Cache] previously stored in ctx by [NewContext],
// or nil if ctx is nil, has no cache value, or the stored cache is
// parameterized with different K/V types than requested.
//
// Use this inside a [FetchFunc] or synchronous callback to access the source
// cache without closing over it at construction time.
func FromContext[K comparable, V any](ctx context.Context) *Cache[K, V] {
	if ctx == nil {
		return nil
	}

	val := ctx.Value(ctxKey{})
	if val == nil {
		return nil
	}

	if c, ok := val.(*Cache[K, V]); ok {
		return c
	}

	return nil
}

// randomName returns a default cache name of the form "cache-XXXXXXXX" where
// X is a hex digit. The value is drawn from crypto/rand and hashed via
// CRC-32 to produce a short, roughly-unique identifier suitable for logs.
// It is not intended to be cryptographically secure — collisions across
// many caches are acceptable since names are only used for display.
func randomName() string {
	b := make([]byte, 128)
	_, _ = rand.Read(b)
	return fmt.Sprintf("cache-%x", crc32.ChecksumIEEE(b))
}

// isNil reports whether x is nil, or whether x is a typed-nil reference
// type (pointer, channel, func, interface, map, slice). The deferred
// recover guards against reflect.Value.IsNil panicking on non-nilable
// kinds (e.g. struct, int): for those kinds we want to return false, which
// is the value already initialized in the named return after the panic.
func isNil(x any) bool {
	defer func() { recover() }() //nolint:errcheck
	return x == nil || reflect.ValueOf(x).IsNil()
}

// uniq returns a new slice containing the elements of a in their original
// order, with duplicates removed. Used to normalize user-supplied op lists
// in [OnEvent] and [Log].
func uniq[T comparable](a []T) []T {
	result := make([]T, 0, len(a))
	seen := make(map[T]struct{}, len(a))

	for _, val := range a {
		if _, ok := seen[val]; ok {
			continue
		}

		seen[val] = struct{}{}
		result = append(result, val)
	}

	return result
}
