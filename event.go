package oncecache

import (
	"context"
	"fmt"
)

// callbackFunc is the shared signature for every registered callback. The
// ctx is decorated with the source [Cache] (retrievable via [FromContext]);
// key is the affected key; val and err are the entry's stored value and
// fill error. On [OpMiss], val and err are the zero value, since the entry
// has not yet been populated at the time the miss is observed.
type callbackFunc[K comparable, V any] func(ctx context.Context, key K, val V, err error)

// Entry is the external (user-facing) representation of a cache entry, as
// passed to event subscribers. It is a value type, independent of the
// cache's internal state: modifying its fields has no effect on the cache.
//
// An [Entry] is typically observed via an [Event] delivered to an [OnEvent]
// channel, or constructed by one of the package's logging helpers. Callers
// rarely construct Entry values directly.
type Entry[K comparable, V any] struct {
	// Cache is the source cache that produced this entry. It is set for
	// every entry emitted by this package.
	Cache *Cache[K, V]

	// Key is the cache key this entry corresponds to.
	Key K

	// Val is the stored value. On an [OpMiss] event, Val is the zero value
	// of V because the miss is observed before fetch runs.
	Val V

	// Err is the stored fill error (from [FetchFunc] or [Cache.MaybeSet]),
	// or nil. A non-nil Err does not make the entry invalid; the error is
	// treated like any other cached result.
	Err error
}

// Event is a notification of a cache operation, as delivered to [OnEvent]
// channels. It extends [Entry] with the operation kind ([Op]) and the
// triggering context.
type Event[K comparable, V any] struct {
	// Entry carries the affected (Cache, Key, Val, Err).
	Entry[K, V]

	// Ctx is the context of the [Cache] operation that produced the
	// event, decorated with the source [Cache] (retrievable via
	// [FromContext]). Consumers may use Ctx for trace propagation or to
	// observe cancellation of the producing goroutine. Always non-nil
	// for events produced by this package.
	Ctx context.Context

	// Op identifies the operation that triggered the event: one of
	// [OpHit], [OpMiss], [OpFill], or [OpEvict].
	Op Op
}

// OnEvent returns an [Opt] for [New] that configures the cache to emit
// [Event] values on ch. If ops is empty, events for all four operations
// ([OpHit], [OpMiss], [OpFill], [OpEvict]) are delivered; otherwise, only
// events whose [Op] is in ops are delivered. Duplicate ops are coalesced.
//
// The block parameter controls backpressure behavior when ch is full:
//   - block=true: the [Cache] operation that produced the event blocks on
//     the send. Use with an unbuffered (or small-buffered) ch to prevent
//     the consumer from falling behind the cache. The send aborts if the
//     triggering ctx is cancelled, dropping the event.
//   - block=false: the event is dropped if ch cannot receive immediately.
//     Use with a buffered ch when event loss under backpressure is
//     acceptable.
//
// Each delivered [Event] carries the triggering context as [Event.Ctx],
// so consumers can extract the source [Cache] via [FromContext], honor
// cancellation, or propagate trace state.
//
// The caller owns ch. OnEvent never closes ch.
//
// For long-running work, OnEvent with a buffered channel is usually better
// than the synchronous [OnFill]/[OnEvict]/[OnHit]/[OnMiss] callbacks. For
// basic logging, see [Log].
func OnEvent[K comparable, V any](ch chan<- Event[K, V], block bool, ops ...Op) Opt {
	ops = uniq(ops)
	if len(ops) == 0 {
		ops = []Op{OpHit, OpMiss, OpFill, OpEvict}
	}
	return eventOpt[K, V]{ch: ch, block: block, ops: ops}
}

// eventOpt is the [Opt] returned by [OnEvent]. It carries the destination
// channel, the set of ops to deliver, and the block/drop mode. Its apply
// method installs a synthetic callback for each op that packages a
// callback invocation into an [Event] and sends it (blocking, with ctx
// cancellation, or non-blocking with drop).
type eventOpt[K comparable, V any] struct {
	ch    chan<- Event[K, V]
	ops   []Op
	block bool
}

func (o eventOpt[K, V]) optioner() {}

func (o eventOpt[K, V]) apply(c *Cache[K, V]) { //nolint:unused // linter is wrong, method is invoked.
	for _, op := range o.ops {
		fn := func(ctx context.Context, key K, val V, err error) {
			event := Event[K, V]{
				Op:    op,
				Ctx:   ctx,
				Entry: Entry[K, V]{Cache: c, Key: key, Val: val, Err: err},
			}

			if o.block {
				// Blocking, with ctx cancellation so a dead consumer
				// doesn't hang the producer indefinitely.
				select {
				case o.ch <- event:
				case <-ctx.Done():
				}
				return
			}

			// Non-blocking: drop on full.
			select {
			case o.ch <- event:
			default:
			}
		}

		switch op {
		case OpFill:
			c.onFill = append(c.onFill, fn)
		case OpEvict:
			c.onEvict = append(c.onEvict, fn)
		case OpHit:
			c.onHit = append(c.onHit, fn)
		case OpMiss:
			c.onMiss = append(c.onMiss, fn)
		default:
			panic(fmt.Sprintf("unknown op: %v: %s", op, op))
		}
	}
}

// callbackOpt is the [Opt] type returned by [OnFill], [OnEvict], [OnHit],
// and [OnMiss]. It carries the registered function and the [Op] it should
// fire for. The apply method appends fn to the corresponding on* slice on
// the [Cache] during construction.
type callbackOpt[K comparable, V any] struct {
	fn callbackFunc[K, V]
	op Op
}

func (o callbackOpt[K, V]) optioner() {}

func (o callbackOpt[K, V]) apply(c *Cache[K, V]) { //nolint:unused // linter is wrong, method is invoked.
	switch o.op {
	case OpFill:
		c.onFill = append(c.onFill, o.fn)
	case OpEvict:
		c.onEvict = append(c.onEvict, o.fn)
	case OpHit:
		c.onHit = append(c.onHit, o.fn)
	case OpMiss:
		c.onMiss = append(c.onMiss, o.fn)
	default:
		// Shouldn't happen.
		panic(fmt.Sprintf("unknown op: %v: %s", o.op, o.op))
	}
}

// OnFill returns an [Opt] for [New] that registers fn as a synchronous
// callback invoked after a cache entry is populated — either on demand
// (via [Cache.Get] and [FetchFunc]) or externally (via [Cache.MaybeSet]).
// fn receives the key, the stored value, and the stored fill error.
//
// OnFill callbacks run synchronously on the goroutine that triggered the
// fill: the triggering [Cache.Get] or [Cache.MaybeSet] call blocks until
// every registered OnFill returns. Prefer [OnEvent] for long-running or
// blocking work.
//
// Multiple OnFill callbacks may be registered; they fire in registration
// order. Common uses include metrics emission and propagating entries
// between composite caches (see the hrsystem example for the latter).
//
// ctx is decorated with the source [Cache], retrievable via [FromContext].
func OnFill[K comparable, V any](fn func(ctx context.Context, key K, val V, err error)) Opt {
	return callbackOpt[K, V]{op: OpFill, fn: fn}
}

// OnEvict returns an [Opt] for [New] that registers fn as a synchronous
// callback invoked after a cache entry is removed via [Cache.Delete] or
// [Cache.Clear]. fn receives the key and the entry's stored (val, err) at
// the time of eviction.
//
// OnEvict callbacks run synchronously on the goroutine that triggered the
// eviction, after [Cache]'s internal lock has been released. fn may
// therefore safely call methods on the same cache (or on other caches —
// this is the foundation of the composite-cache propagation pattern).
//
// If the entry is still being filled when [Cache.Delete] or [Cache.Clear]
// runs, OnEvict is not invoked for that entry; see [Cache.Delete].
func OnEvict[K comparable, V any](fn func(ctx context.Context, key K, val V, err error)) Opt {
	return callbackOpt[K, V]{op: OpEvict, fn: fn}
}

// OnHit returns an [Opt] for [New] that registers fn as a synchronous
// callback invoked when [Cache.Get] returns an already-populated entry
// (a cache hit). fn receives the key and the stored (val, err).
//
// An entry with a non-nil fill error is a valid hit: OnHit is invoked with
// the stored error, and [OpHit] is emitted.
//
// The triggering [Cache.Get] blocks until every OnHit returns. For
// long-running work, prefer [OnEvent].
func OnHit[K comparable, V any](fn func(ctx context.Context, key K, val V, err error)) Opt {
	return callbackOpt[K, V]{op: OpHit, fn: fn}
}

// OnMiss returns an [Opt] for [New] that registers fn as a synchronous
// callback invoked when [Cache.Get] observes a cache miss, before fetch
// runs. OnMiss fires only once per entry lifetime; it is followed by an
// [OpFill] (with either the fetch result or a panic-wrapped error — see
// [FetchFunc]).
//
// The val and err arguments to fn are always the zero values, since the
// entry has not yet been populated at miss time; they exist solely so that
// OnMiss shares the standard callback signature with [OnFill], [OnHit],
// and [OnEvict] (and so the V type parameter is inferable from fn).
//
// OnMiss callbacks run synchronously; the triggering [Cache.Get] blocks
// until they return. Prefer [OnEvent] for long-running work.
//
// If an OnMiss callback panics, the panic is recovered and becomes the
// entry's fill error (wrapping [ErrPanic]), exactly as a [FetchFunc] panic
// would; fetch is skipped, [OpFill] still fires with the wrapped error, and
// the triggering [Cache.Get] returns (zero, wrapped error) rather than
// propagating the panic.
//
// OnMiss is not emitted by [Cache.MaybeSet], since MaybeSet is not a
// [Cache.Get] miss path.
func OnMiss[K comparable, V any](fn func(ctx context.Context, key K, val V, err error)) Opt {
	return callbackOpt[K, V]{op: OpMiss, fn: fn}
}

// Op is an enumeration of cache operations. It is the kind field of [Event]
// and the selector used by [OnEvent] and [Log] to filter which operations
// trigger delivery. The zero value is invalid; use [Op.IsZero] to detect it.
type Op uint8

const (
	// OpHit indicates a cache hit: [Cache.Get] found an already-populated
	// entry for the key. A hit may carry a non-nil [Entry.Err] (an entry
	// whose fill returned an error is still a valid, cacheable result).
	OpHit Op = 1

	// OpMiss indicates a cache miss: [Cache.Get] found no entry for the
	// key and is about to invoke [FetchFunc]. OpMiss fires once per entry
	// lifetime and is immediately followed by [OpFill] — including when
	// [FetchFunc] or an [OnMiss] callback panics, in which case the panic
	// is recovered into an error wrapping [ErrPanic] and OpFill carries
	// that error. See [FetchFunc] and [OnMiss].
	OpMiss Op = 2

	// OpFill indicates that a cache entry has been populated. It is
	// preceded by [OpMiss] when the fill was driven by [Cache.Get], or
	// emitted standalone when the fill was driven by [Cache.MaybeSet]. An
	// entry filled with a non-nil error still emits OpFill; the error is
	// cached like any other result.
	OpFill Op = 3

	// OpEvict indicates a cache entry has been removed via [Cache.Delete]
	// or [Cache.Clear]. An in-flight fill (one whose [FetchFunc] has not
	// yet completed) does not emit OpEvict when it is removed: see
	// [Cache.Delete].
	OpEvict Op = 4
)

// IsZero reports whether o is the zero value (the invalid [Op]).
func (o Op) IsZero() bool {
	return o == 0
}

// String returns the op name: "hit", "miss", "fill", or "evict". For any
// other value (including the zero [Op]) it returns "unknown".
func (o Op) String() string {
	switch o {
	case OpFill:
		return "fill"
	case OpEvict:
		return "evict"
	case OpHit:
		return "hit"
	case OpMiss:
		return "miss"
	default:
		return "unknown"
	}
}
