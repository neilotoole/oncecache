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
// Synchronous callbacks run on the goroutine that triggered the event.
// [OnFill], [OnMiss], and [OnHit] callbacks must not call [Cache.Get] or
// [Cache.MaybeSet] for the same key on the same cache — that re-enters
// the entry's [sync.Once] and deadlocks — but they may freely act on
// other keys or other caches (the foundation of composite-cache
// propagation). [OnEvict] has no such restriction: it runs after the
// cache's internal lock is released and may call any method on any
// cache. For long-running work, prefer [OnEvent] with a buffered channel.
package oncecache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
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
// If FetchFunc panics, the cache recovers the panic and stores it as the
// entry's fill error, wrapped so [errors.Is] matches [ErrPanic]. The
// triggering [Cache.Get] call returns the zero value together with that
// wrapped error rather than propagating the panic. Subsequent [Cache.Get]
// calls for the same key return the same wrapped error, without
// reinvoking FetchFunc, until the entry is evicted. The [OnFill] callbacks
// fire normally with the wrapped error. This preserves the lifecycle
// invariant: a miss is followed by a fill — successful or panic-wrapped.
type FetchFunc[K comparable, V any] func(ctx context.Context, key K) (val V, err error)

// ErrPanic is the sentinel wrapped in the fill error when a [FetchFunc] (or
// an [OnMiss] callback) panics during a [Cache.Get] fill. Use [errors.Is] to
// detect it:
//
//	if _, err := c.Get(ctx, key); errors.Is(err, oncecache.ErrPanic) {
//		// ... handle panic-during-fill
//	}
var ErrPanic = errors.New("oncecache: panic during fill")

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
// population: any [Cache.Get] for an unpopulated key returns the zero
// value with an error wrapping [ErrPanic] (the recovered nil-pointer
// dereference).
func New[K comparable, V any](fetch FetchFunc[K, V], opts ...Opt) *Cache[K, V] {
	c := &Cache[K, V]{
		entries: map[K]*entry[K, V]{},
		fetch:   fetch,
	}
	defaultName := randomName()
	c.name.Store(&defaultName)

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

// applyOpts applies functional options to c. It dispatches on the dynamic
// type of each [Opt]:
//
//   - [Name] sets the cache's display name directly.
//   - [logOptConfig] (returned by [Log]) is a non-parameterized payload
//     that we reconstitute as a typed logOpt[K, V] before applying.
//   - Anything else must satisfy [optApplier][K, V] — a generic-aware
//     applier used by [OnFill], [OnEvict], [OnHit], [OnMiss], and
//     [OnEvent], all of which can touch the cache's parameterized fields
//     (callback slices) directly.
//
// Unrecognized Opt types panic to surface programmer errors early. The
// [Opt] interface is closed (its marker method is unexported), so any
// reachable Opt is necessarily one of the kinds above.
func (c *Cache[K, V]) applyOpts(opts []Opt) {
	for _, opt := range opts {
		if isNil(opt) {
			continue
		}
		switch o := opt.(type) {
		case Name:
			s := string(o)
			c.name.Store(&s)
		case *Name:
			// Name has a value receiver on its marker method, so *Name
			// also satisfies Opt. Accept pointer-to-Name for forgiveness.
			s := string(*o)
			c.name.Store(&s)
		case logOptConfig:
			(*logOpt[K, V])(&o).apply(c)
		case optApplier[K, V]:
			o.apply(c)
		default:
			panic(fmt.Sprintf("Invalid Opt type %T", opt))
		}
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
	// callback-iteration overhead when no relevant callbacks exist. The
	// cache is passed as an argument rather than stored on each entry,
	// saving one pointer per entry.
	maybeSetValueFn func(ctx context.Context, c *Cache[K, V], e *entry[K, V], key K, val V, err error) bool
	getValueFn      func(ctx context.Context, c *Cache[K, V], e *entry[K, V], key K) (V, error)

	// name is set via the Name opt, or a random value if not specified.
	// Stored as atomic.Pointer[string] so [Cache.Name] / [Cache.String] /
	// [Cache.LogValue] can safely read it without c.mu, even concurrently
	// with [Cache.GobDecode] (which overwrites it).
	name atomic.Pointer[string]

	// on* slices are populated at construction time from opts and are
	// never mutated thereafter, so they may be read without the lock.
	onFill  []callbackFunc[K, V]
	onEvict []callbackFunc[K, V]
	onHit   []callbackFunc[K, V]
	onMiss  []callbackFunc[K, V]

	// mu guards entries and (transiently) name.
	mu sync.Mutex
}

// Name returns the cache's name, useful in logs and debug output. The name
// is set via the [Name] opt to [New]; otherwise a random name of the form
// "cache-XXXXXXXX" (eight hex digits) is generated. Safe for concurrent
// use, including with [Cache.GobDecode].
//
// Name returns "" for a nil receiver or an uninitialized (zero-value) Cache,
// so it is safe to call on the zero [Event]/[Entry] values that [slog] may
// resolve (see [Event.LogValue]).
func (c *Cache[K, V]) Name() string {
	if c == nil {
		return ""
	}
	if p := c.name.Load(); p != nil {
		return *p
	}
	return ""
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
		c.Name(), *new(K), *new(V), c.Len(),
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
		slog.String("name", c.Name()),
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

// Clear empties the cache, invoking any [OnEvict] callbacks once per
// previously-filled entry. The eviction callback order is not specified.
//
// Callbacks fire after c.mu has been released, so a callback may safely
// call methods on the same cache (including [Cache.Get], [Cache.Delete],
// [Cache.Has], etc.) without deadlocking.
//
// If an entry is currently being filled when Clear is called, no [OnEvict]
// callback is invoked for that entry; the in-flight fill is effectively
// orphaned and its [OnFill] callbacks (if any) will still fire when fetch
// completes (against an entry no longer in the cache).
func (c *Cache[K, V]) Clear(ctx context.Context) {
	c.mu.Lock()
	if len(c.onEvict) == 0 {
		clear(c.entries)
		c.mu.Unlock()
		return
	}

	// Snapshot the keys + filled entries under the lock, then release the
	// lock before invoking callbacks. This avoids deadlocking when a
	// callback re-enters the same cache.
	type kv struct {
		key K
		e   *entry[K, V]
	}
	snap := make([]kv, 0, len(c.entries))
	for k, e := range c.entries {
		if e.filled.Load() {
			snap = append(snap, kv{key: k, e: e})
		}
	}
	clear(c.entries)
	c.mu.Unlock()

	cbCtx := NewContext(ctx, c)
	for _, item := range snap {
		for _, fn := range c.onEvict {
			fn(cbCtx, item.key, item.e.val, item.e.err)
		}
	}
}

// Delete removes the entry for key, invoking any [OnEvict] callbacks if the
// entry was filled. Delete is a no-op if the key is not present.
//
// Callbacks fire after c.mu has been released, so a callback may safely
// call methods on the same cache without deadlocking.
//
// If the entry is currently being filled when Delete is called, no
// [OnEvict] callback is invoked; the in-flight fill is effectively
// orphaned and its [OnFill] callbacks (if any) will still fire.
func (c *Cache[K, V]) Delete(ctx context.Context, key K) {
	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	delete(c.entries, key)
	c.mu.Unlock()

	if len(c.onEvict) == 0 || !e.filled.Load() {
		return
	}
	cbCtx := NewContext(ctx, c)
	for _, fn := range c.onEvict {
		fn(cbCtx, key, e.val, e.err)
	}
}

// MaybeSet sets the value and fill error for key if the entry is not already
// filled, returning true if the value was set. This allows external code to
// prime the cache or propagate values from another source.
//
// If there's already a filled entry for key — populated by a prior
// [Cache.Get] or [Cache.MaybeSet] — MaybeSet does not replace it and
// returns false. If the entry is still being filled by an in-flight
// fetch, MaybeSet blocks until that fetch completes (because it shares
// the entry's [sync.Once]), and then returns false.
//
// The err argument, when non-nil, is stored alongside val just like a
// fetch error; subsequent [Cache.Get] calls for this key return
// (val, err) without reinvoking fetch.
//
// When MaybeSet does populate the entry, it invokes any [OnFill] callbacks
// synchronously and emits an [OpFill] event via any [OnEvent] channels.
// [OnMiss] is not emitted, since MaybeSet is not a [Cache.Get] miss.
func (c *Cache[K, V]) MaybeSet(ctx context.Context, key K, val V, err error) (ok bool) {
	e := c.getEntry(key)
	return c.maybeSetValueFn(ctx, c, e, key, val, err)
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
// If fetch (or an [OnMiss] callback) panics, the panic is recovered and
// converted into a fill error wrapping [ErrPanic]; Get returns (zero, that
// wrapped error) instead of propagating the panic. When an OnMiss callback
// panics, fetch is skipped. [OpFill] still fires with the wrapped error in
// either case. See [FetchFunc] and [OnMiss] for details.
func (c *Cache[K, V]) Get(ctx context.Context, key K) (V, error) {
	e := c.getEntry(key)
	return c.getValueFn(ctx, c, e, key)
}

// getEntry returns the entry for key, creating (but not yet populating) one
// if it doesn't exist. The returned entry's val/err are zero until some
// caller's once.Do completes. Holds c.mu only for the map lookup/insert.
func (c *Cache[K, V]) getEntry(key K) *entry[K, V] {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		// Hit (the hot path): explicit unlock, no defer overhead.
		c.mu.Unlock()
		return e
	}

	// Miss: defer the unlock so a panic on the nil-map write — reachable
	// only on an unsupported zero-value Cache — cannot leak c.mu.
	defer c.mu.Unlock()
	e := &entry[K, V]{}
	c.entries[key] = e
	return e
}

// Close empties the cache. It is idempotent, always returns nil, and is
// safe to call on a nil [Cache] receiver (no-op) and concurrently with
// other [Cache] methods.
//
// Close does not invoke [OnEvict] for the entries it clears, and it does
// not detach callbacks: the cache remains fully usable afterwards. Close
// is essentially a no-eviction-callback variant of [Cache.Clear], provided
// so [Cache] satisfies [io.Closer]. Callers that wish to release memory
// referenced by callback closures should drop their reference to the
// cache so it can be GC'd.
func (c *Cache[K, V]) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
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
	registerFillPanicErrorOnGob()
	c.mu.Lock()
	defer c.mu.Unlock()

	gbd := &gobData[K, V]{
		Name:    *c.name.Load(),
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

// GobDecode implements [gob.GobDecoder]. Only the cache name and entries
// are restored from p. Any pre-existing entries in c are cleared before
// decoding; decoded entries are marked as filled, so subsequent
// [Cache.Get] calls return them directly without invoking fetch.
//
// Behavior depends on how c was initialized:
//   - Decoding into a [Cache] constructed via [New]: the existing fetch
//     func and callbacks are preserved. [Cache.Get] on a key absent from
//     the decoded payload triggers the existing fetch func as usual.
//   - Decoding into a zero-value [Cache] (as `gob.Decode` into
//     `var c Cache[K, V]` does): the internal map and dispatch functions
//     are initialized on demand, but the cache has no fetch func and no
//     callbacks. [Cache.Get] for a key absent from the decoded payload
//     yields an error wrapping [ErrPanic] (the recovered nil-fetch
//     dereference).
//
// The error type(s) used in entries with non-nil fill errors must be
// registered with [gob.Register] before encoding/decoding. The package
// pre-registers its internal panic-wrapper type, so panic-filled entries
// also round-trip.
//
// Nil map values in a crafted or corrupted gob payload are skipped rather
// than dereferenced; they do not cause a panic.
//
// GobDecode does not fire [OnEvict] for the pre-existing entries it clears,
// nor [OnFill] for the decoded entries it installs.
func (c *Cache[K, V]) GobDecode(p []byte) error {
	registerFillPanicErrorOnGob()
	c.mu.Lock()
	defer c.mu.Unlock()

	gbd := &gobData[K, V]{}
	if err := gob.NewDecoder(bytes.NewReader(p)).Decode(gbd); err != nil {
		return err
	}

	name := gbd.Name
	c.name.Store(&name)
	if c.entries == nil {
		c.entries = make(map[K]*entry[K, V], len(gbd.Entries))
	} else {
		clear(c.entries)
	}
	// Initialize dispatch functions on demand so a zero-value Cache is
	// usable for Get / MaybeSet after GobDecode. A decoded cache has no
	// callbacks, so the fast variants are always correct here.
	if c.getValueFn == nil {
		c.getValueFn = getValueFast[K, V]
	}
	if c.maybeSetValueFn == nil {
		c.maybeSetValueFn = maybeSetValueFast[K, V]
	}
	for k, e := range gbd.Entries {
		if e == nil {
			// A corrupt or crafted gob payload can contain nil map
			// values. Skip rather than panic on dereference.
			continue
		}
		ent := &entry[K, V]{val: e.Val, err: e.Err}
		ent.once.Do(func() {}) // Consume the sync.Once
		ent.filled.Store(true)
		c.entries[k] = ent
	}

	return nil
}

// entry is the internal representation of a cache entry. Contrast with the
// external [Entry] type.
//
// Fields val and err are written exactly once, inside once.Do, and must not
// be read by any code path that doesn't either run inside once.Do or observe
// filled.Load() == true. This ordering lets external readers ([Cache.Delete],
// [Cache.Clear], [Cache.GobEncode]) skip entries whose fill is still
// in-flight, avoiding a race with the filling goroutine.
//
// The entry does not hold a back-pointer to its source [Cache]; fill helpers
// receive the cache as a parameter instead.
type entry[K comparable, V any] struct {
	val    V
	err    error
	once   sync.Once
	filled atomic.Bool
}

// fillPanicError wraps a recovered panic value into an error that can be
// stored in a cache entry. It unwraps to [ErrPanic] so callers can test
// via [errors.Is]. The recovered panic value is stored as its formatted
// string representation (not as any) so that errorful entries remain
// gob-encodable — see [Cache.GobEncode].
type fillPanicError struct {
	recovered string
}

func (p *fillPanicError) Error() string { return fmt.Sprintf("%s: %s", ErrPanic, p.recovered) }
func (p *fillPanicError) Unwrap() error { return ErrPanic }

// GobEncode / GobDecode let fillPanicError round-trip cleanly even though
// its only field is unexported — gob otherwise encodes no fields, losing
// the recovered message.
func (p *fillPanicError) GobEncode() ([]byte, error) {
	return []byte(p.recovered), nil
}

func (p *fillPanicError) GobDecode(data []byte) error {
	p.recovered = string(data)
	return nil
}

// gobRegisterFillPanic ensures fillPanicError is registered with
// encoding/gob exactly once per process, so panic-filled cache entries
// round-trip through GobEncode/GobDecode without requiring the caller to
// register an unexported type they can't name.
var gobRegisterFillPanic sync.Once

// registerFillPanicErrorOnGob registers fillPanicError with gob on first
// call. Called from [Cache.GobEncode] and [Cache.GobDecode].
func registerFillPanicErrorOnGob() {
	gobRegisterFillPanic.Do(func() {
		gob.Register(&fillPanicError{})
	})
}

// callFetch invokes c.fetch, recovering any panic and converting it into
// an error wrapping [ErrPanic]. The recovered panic is NOT re-thrown:
// returning the wrapped error is more useful than propagating the panic to
// the caller, and it avoids leaving the entry in a half-initialized state.
//
// Panics in OnFill, OnHit, and OnEvict callbacks are deliberately not
// recovered; they propagate to the triggering caller. OnMiss is the
// exception: because it runs inside the entry's sync.Once before fetch, an
// unrecovered panic there would consume the Once and strand the entry
// unfilled, so callOnMiss recovers it into a fill error instead.
func callFetch[K comparable, V any](ctx context.Context, c *Cache[K, V], key K) (val V, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero V
			val = zero
			err = &fillPanicError{recovered: fmt.Sprintf("%v", r)}
		}
	}()
	return c.fetch(ctx, key)
}

// callOnMiss invokes the OnMiss callbacks, recovering any panic and
// converting it into an error wrapping [ErrPanic] (mirroring [callFetch]).
// A non-nil return means an OnMiss callback panicked; the caller stores that
// error as the entry's fill result and skips fetch, so the entry ends up
// filled with a panic-wrapped error rather than stranded unfilled. The
// callbacks receive the zero value and a nil error, since the entry is not
// yet populated at miss time.
func callOnMiss[K comparable, V any](ctx context.Context, c *Cache[K, V], key K) (err error) {
	if len(c.onMiss) == 0 {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = &fillPanicError{recovered: fmt.Sprintf("%v", r)}
		}
	}()
	var zero V
	for _, fn := range c.onMiss {
		fn(ctx, key, zero, nil)
	}
	return nil
}

// maybeSetValueSlow is the MaybeSet core used when OnFill callbacks are
// registered. See maybeSetValueFast for the no-callback path.
func maybeSetValueSlow[K comparable, V any](
	ctx context.Context, c *Cache[K, V], e *entry[K, V], key K, val V, err error,
) bool {
	var ok bool
	e.once.Do(func() {
		ok = true
		e.val = val
		e.err = err
		e.filled.Store(true)
		cbCtx := NewContext(ctx, c)
		for _, fn := range c.onFill {
			fn(cbCtx, key, val, err)
		}
	})
	return ok
}

// maybeSetValueFast is the MaybeSet core used when no OnFill callbacks are
// registered. It skips callback iteration and the NewContext decoration.
func maybeSetValueFast[K comparable, V any](
	_ context.Context, _ *Cache[K, V], e *entry[K, V], _ K, val V, err error,
) bool {
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
// fetch (with panic recovery), marks the entry filled, then fires OnFill —
// all inside the entry's sync.Once. On hit it fires OnHit outside the Once
// (safe: the Once's completion synchronizes the val/err writes for later
// readers).
//
// A panic in an OnMiss callback is recovered and becomes the entry's fill
// error (wrapping [ErrPanic]), exactly as a fetch panic would; fetch is
// skipped in that case but [OnFill] still fires with the wrapped error.
func getValueSlow[K comparable, V any](ctx context.Context, c *Cache[K, V], e *entry[K, V], key K) (V, error) {
	var miss bool
	e.once.Do(func() {
		miss = true
		cbCtx := NewContext(ctx, c)

		if err := callOnMiss(cbCtx, c, key); err != nil {
			e.err = err // e.val stays the zero value; fetch is skipped.
		} else {
			e.val, e.err = callFetch(cbCtx, c, key)
		}
		e.filled.Store(true)

		for _, fn := range c.onFill {
			fn(cbCtx, key, e.val, e.err)
		}
	})

	if !miss && len(c.onHit) > 0 {
		cbCtx := NewContext(ctx, c)
		for _, fn := range c.onHit {
			fn(cbCtx, key, e.val, e.err)
		}
	}

	return e.val, e.err
}

// getValueFast is the Get core used when no Get-triggered callbacks are
// registered, which is the common case. It skips the OnMiss/OnFill/OnHit
// iteration entirely.
func getValueFast[K comparable, V any](ctx context.Context, c *Cache[K, V], e *entry[K, V], key K) (V, error) {
	e.once.Do(func() {
		cbCtx := NewContext(ctx, c)
		e.val, e.err = callFetch(cbCtx, c, key)
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

// randomName returns a default cache name of the form "cache-XXXXXXXX"
// where X is a hex digit. Random bytes are drawn from crypto/rand; the
// value is used only for display in logs and debug output, and is not
// meant to be cryptographically strong.
func randomName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("cache-%x", b)
}

// isNil reports whether x is nil, or whether x is a typed-nil reference
// type (pointer, channel, func, interface, map, or slice). Non-nilable
// kinds (struct, int, bool, etc.) are never nil and return false.
func isNil(x any) bool {
	if x == nil {
		return true
	}
	//nolint:exhaustive // only nilable kinds are listed; all others fall through to false.
	switch v := reflect.ValueOf(x); v.Kind() {
	case reflect.Pointer, reflect.Chan, reflect.Func,
		reflect.Interface, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
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
