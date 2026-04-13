package oncecache

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// LogConfig holds process-wide defaults for how [Log] formats slog output
// and how [Event.LogValue] / [Entry.LogValue] name their attributes. Mutate
// this package-level variable at program startup to customize log output;
// the values are read on each log call.
//
// Fields:
//   - Msg:       the slog message text used by [Log] (default: "Cache event").
//   - AttrEvent: the top-level group key under which event attrs are nested
//     when a whole [Event] is logged (default: "ev").
//   - AttrCache: attribute name for the cache name (default: "cache").
//   - AttrOp:    attribute name for the [Op] (default: "op").
//   - AttrKey:   attribute name for the entry key (default: "k").
//   - AttrVal:   attribute name for the entry value (default: "v"; only
//     emitted for primitive V types and [slog.LogValuer] — see
//     [Entry.LogValue]).
//   - AttrErr:   attribute name for the entry error (default: "err").
var LogConfig = struct {
	Msg       string
	AttrEvent string
	AttrCache string
	AttrOp    string
	AttrKey   string
	AttrVal   string
	AttrErr   string
}{
	Msg:       "Cache event",
	AttrEvent: "ev",
	AttrCache: "cache",
	AttrOp:    "op",
	AttrKey:   "k",
	AttrVal:   "v",
	AttrErr:   "err",
}

// Log returns an [Opt] for [New] that emits an [slog] record for each
// matching [Event]. It is the simplest way to wire cache activity into
// structured logging.
//
// Parameters:
//   - log: destination logger. If nil, Log returns nil and registers
//     nothing (treated as a no-op Opt).
//   - lvl: the level at which to log. Nil is treated as [slog.LevelInfo].
//   - ops: which operations to log. If empty, all four ops are logged.
//     Duplicates are coalesced.
//
// Multiple Log opts can be combined to log different ops at different
// levels — for example, Fill/Evict at Info and Hit/Miss at Debug:
//
//	c := oncecache.New[int, int](
//		calcFibonacci,
//		oncecache.Name("fibs"),
//		oncecache.Log(log, slog.LevelInfo, oncecache.OpFill, oncecache.OpEvict),
//		oncecache.Log(log, slog.LevelDebug, oncecache.OpHit, oncecache.OpMiss),
//	)
//
// Output format: each record has the message [LogConfig].Msg and a single
// attribute group (default key "ev") whose contents are determined by
// [Event.LogValue]. Customize attribute names via [LogConfig].
//
// For more control than Log provides, use an [OnEvent] channel or the
// synchronous On* callbacks directly; note that [Event], [Entry], and
// [Cache] all implement [slog.LogValuer], so they format cleanly in any
// slog output.
func Log(log *slog.Logger, lvl slog.Leveler, ops ...Op) Opt {
	if log == nil {
		return nil
	}

	if isNil(lvl) {
		lvl = slog.LevelInfo
	}

	if len(ops) == 0 {
		ops = []Op{OpHit, OpMiss, OpFill, OpEvict}
	} else {
		ops = uniq(ops)
	}

	return logOptConfig{
		log: log,
		lvl: lvl,
		ops: ops,
	}
}

var _ Opt = logOptConfig{}

// logOptConfig is the non-parameterized payload returned by [Log]. It is
// non-generic so that callers can write `oncecache.Log(...)` without
// spelling out the [Cache]'s K and V type parameters. The parameterized
// twin [logOpt] is structurally identical and is produced by
// (*Cache).applyOpts via a pointer cast at apply time.
type logOptConfig struct {
	log *slog.Logger
	lvl slog.Leveler
	ops []Op
}

func (o logOptConfig) optioner() {}

var _ Opt = &logOpt[any, any]{}

// logOpt is the parameterized twin of [logOptConfig]; see its doc for the
// rationale. logOpt exists so its methods can touch the parameterized
// [Cache] fields (the on* callback slices).
type logOpt[K comparable, V any] logOptConfig

func (o *logOpt[K, V]) optioner() {}

func (o *logOpt[K, V]) apply(c *Cache[K, V]) {
	for _, op := range o.ops {
		switch op {
		case OpFill:
			c.onFill = append(c.onFill, o.logFill)
		case OpEvict:
			c.onEvict = append(c.onEvict, o.logEvict)
		case OpHit:
			c.onHit = append(c.onHit, o.logHit)
		case OpMiss:
			c.onMiss = append(c.onMiss, o.logMiss)
		default:
			// Shouldn't happen.
			panic(fmt.Sprintf("oncecache: unknown op[%d]: %s", op, op))
		}
	}
}

func (o *logOpt[K, V]) logEvent(ev Event[K, V]) {
	o.log.LogAttrs(context.Background(), o.lvl.Level(), LogConfig.Msg, slog.Any(LogConfig.AttrEvent, ev))
}

func (o *logOpt[K, V]) logHit(ctx context.Context, key K, val V, err error) {
	ev := Event[K, V]{
		Op:    OpHit,
		Entry: Entry[K, V]{Cache: FromContext[K, V](ctx), Key: key, Val: val, Err: err},
	}
	o.logEvent(ev)
}

func (o *logOpt[K, V]) logMiss(ctx context.Context, key K, val V, err error) {
	ev := Event[K, V]{
		Op:    OpMiss,
		Entry: Entry[K, V]{Cache: FromContext[K, V](ctx), Key: key, Val: val, Err: err},
	}
	o.logEvent(ev)
}

func (o *logOpt[K, V]) logFill(ctx context.Context, key K, val V, err error) {
	ev := Event[K, V]{
		Op:    OpFill,
		Entry: Entry[K, V]{Cache: FromContext[K, V](ctx), Key: key, Val: val, Err: err},
	}
	o.logEvent(ev)
}

func (o *logOpt[K, V]) logEvict(ctx context.Context, key K, val V, err error) {
	ev := Event[K, V]{
		Op:    OpEvict,
		Entry: Entry[K, V]{Cache: FromContext[K, V](ctx), Key: key, Val: val, Err: err},
	}
	o.logEvent(ev)
}

// LogValue implements [slog.LogValuer] for [Event]. The emitted group
// contains (at minimum) the cache name, op, and key. For non-[OpMiss]
// events it also includes the stored value (when loggable — see
// [Entry.LogValue]) and error (when non-nil). Attribute names are taken
// from [LogConfig].
func (e Event[K, V]) LogValue() slog.Value {
	attrs := make([]slog.Attr, 3, 5)
	attrs[0] = slog.String(LogConfig.AttrCache, e.Cache.name)
	attrs[1] = slog.String(LogConfig.AttrOp, e.Op.String())
	attrs[2] = slog.Any(LogConfig.AttrKey, e.Key)

	if e.Op != OpMiss {
		if e.isValLogged() {
			attrs = append(attrs, slog.Any(LogConfig.AttrVal, e.Val))
		}
		if e.Err != nil {
			attrs = append(attrs, slog.Any(LogConfig.AttrErr, e.Err))
		}
	}

	return slog.GroupValue(attrs...)
}

// String returns a compact debug representation of the event in the form
// "<cacheName>.<op>[<key>]" with an optional "[! <err>]" suffix when the
// entry has a non-nil error. The value is deliberately omitted because V
// may not be printable; for structured output, use [Event.LogValue].
func (e Event[K, V]) String() string {
	var sb strings.Builder
	sb.WriteString(e.Cache.name)
	sb.WriteRune('.')
	sb.WriteString(e.Op.String())
	sb.WriteRune('[')
	sb.WriteString(fmt.Sprintf("%v", e.Key))
	sb.WriteRune(']')
	if e.Err != nil {
		sb.WriteString("[! ")
		sb.WriteString(e.Err.Error())
		sb.WriteRune(']')
	}
	return sb.String()
}

// String returns a compact debug representation of the entry in the form
// "<cacheName>[<key>]" with an optional "[! <err>]" suffix when the entry
// has a non-nil error. The value is deliberately omitted because V may not
// be printable; for structured output, use [Entry.LogValue].
func (e Entry[K, V]) String() string {
	sb := strings.Builder{}
	sb.WriteString(e.Cache.name)
	sb.WriteRune('[')
	sb.WriteString(fmt.Sprintf("%v", e.Key))
	sb.WriteRune(']')
	if e.Err != nil {
		sb.WriteString("[! ")
		sb.WriteString(e.Err.Error())
		sb.WriteRune(']')
	}
	return sb.String()
}

// LogValue implements [slog.LogValuer] for [Entry]. The emitted group
// always contains the cache name and the key; it includes the stored
// value only when V is a type safe to log via slog (primitive numeric
// types, bool, or any [slog.LogValuer]); and includes the error only
// when non-nil. In particular, string values are NOT logged, to avoid
// unbounded-length log lines for caches whose V is a large string
// payload. Attribute names come from [LogConfig].
func (e Entry[K, V]) LogValue() slog.Value {
	attrs := make([]slog.Attr, 2, 4)
	attrs[0] = slog.String(LogConfig.AttrCache, e.Cache.name)
	attrs[1] = slog.Any(LogConfig.AttrKey, e.Key)

	if e.isValLogged() {
		attrs = append(attrs, slog.Any(LogConfig.AttrVal, e.Val))
	}
	if e.Err != nil {
		attrs = append(attrs, slog.Any(LogConfig.AttrErr, e.Err))
	}

	return slog.GroupValue(attrs...)
}

// isValLogged reports whether the entry's Val field is safe to emit as a
// slog attribute. The rule is deliberately conservative: only fixed-size
// primitive types (numeric kinds, bool) and types that implement
// [slog.LogValuer] (and can thus control their own log representation)
// are included. String is excluded because cache values are often large
// payloads; callers who want string values in logs should wrap their V
// in a type that implements [slog.LogValuer].
func (e Entry[K, V]) isValLogged() bool {
	switch any(e.Val).(type) {
	case slog.LogValuer, bool, nil, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, complex64, complex128:
		return true
	default:
		return false
	}
}
