package oncecache_test

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/neilotoole/slogt/v2"

	"github.com/neilotoole/oncecache"
	"github.com/neilotoole/oncecache/examples/hrsystem"
)

func fetchEvenOnly(_ context.Context, key int) (string, error) {
	if key%2 == 0 {
		return strconv.Itoa(key), nil
	}
	return "", errors.New("odd numbers not supported")
}

func fetchDouble(_ context.Context, key int) (val int, err error) {
	return key * 2, nil
}

func calcFibonacci(ctx context.Context, n int) (val int, err error) {
	a, b, temp := 0, 1, 0 //nolint:wastedassign
	for i := 0; i < n && ctx.Err() == nil; i++ {
		temp = a
		a = b
		b = temp + a
	}

	if ctx.Err() != nil {
		return 0, ctx.Err()
	}

	return a, nil
}

func TestCache(t *testing.T) {
	ctx := context.Background()
	c := oncecache.New[int, string](fetchEvenOnly)

	require.False(t, c.Has(0))

	got, err := c.Get(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, "0", got)
	require.True(t, c.Has(0))

	got, err = c.Get(ctx, 1)
	require.Error(t, err)
	require.Empty(t, got)

	// Seven is my lucky number though.
	ok := c.MaybeSet(ctx, 7, "seven", nil)
	require.True(t, ok)
	got, err = c.Get(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, "seven", got)

	// Verify that it a value can only be set once.
	ok = c.MaybeSet(ctx, 7, "", errors.New("nope"))
	require.False(t, ok)
	got, err = c.Get(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, "seven", got)

	// But, if we delete the entry, it can be set again.
	c.Delete(ctx, 7)
	got, err = c.Get(ctx, 7)
	require.Error(t, err)
	require.Empty(t, got)

	// Verify that clear works too.
	c.Clear(ctx)
	ok = c.MaybeSet(ctx, 7, "seven", nil)
	require.True(t, ok)
	got, err = c.Get(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, "seven", got)
}

func TestCacheConcurrent(t *testing.T) {
	t.Parallel()

	const concurrency = 1000
	const numbers = 500
	ctx := context.Background()

	// invocations tracks how many times fetcher is invoked for each key.
	// Hint: it should be invoked only once per key.
	invocations := map[int]*atomic.Int64{}
	for i := 0; i < numbers; i++ {
		invocations[i] = &atomic.Int64{}
	}

	fetcher := func(ctx context.Context, key int) (val string, err error) {
		invocations[key].Add(1)
		return fetchEvenOnly(ctx, key)
	}

	c := oncecache.New[int, string](fetcher)

	wg := &sync.WaitGroup{}
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numbers; j++ {
				got, err := c.Get(ctx, j)
				if j%2 == 0 {
					require.NoError(t, err)
					require.Equal(t, strconv.Itoa(j), got)
				} else {
					require.Error(t, err)
					require.Empty(t, got)
				}
			}
		}()
	}
	wg.Wait()

	for i := 0; i < numbers; i++ {
		assert.Equal(t, int64(1), invocations[i].Load(), "key %d", i)
	}
}

// TestConcurrentDeleteAndFetch exercises the race between Delete/Clear and an
// in-flight fetch populating the same entry. Before the filled-flag fix,
// Delete/Clear would read e.val/e.err concurrently with the fetch goroutine's
// write, which the race detector flagged. Running this under `-race` is the
// regression check.
func TestConcurrentDeleteAndFetch(t *testing.T) {
	t.Parallel()

	const iters = 2000
	ctx := context.Background()

	// Fetch does a bit of work to widen the race window.
	const fetchedStr = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	fetch := func(_ context.Context, _ int) (string, error) {
		s := ""
		for i := 0; i < 100; i++ {
			s += "x"
		}
		return s, nil
	}

	var evictCalls atomic.Int64
	c := oncecache.New[int, string](
		fetch,
		oncecache.OnEvict(func(_ context.Context, _ int, val string, err error) {
			evictCalls.Add(1)
			// If filled-flag check works, val is always the fetched value for
			// evicted-after-fill entries, never a zero/torn read.
			if val != "" && val != fetchedStr {
				t.Errorf("torn read on evict: val=%q", val)
			}
			if err != nil {
				t.Errorf("unexpected err on evict: %v", err)
			}
		}),
	)

	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = c.Get(ctx, i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			c.Delete(ctx, i)
		}
	}()
	wg.Wait()

	// We don't assert an exact evict count: how many Deletes observe a filled
	// entry vs. an in-flight one is timing-dependent. We only require that
	// no data race fired and no torn reads happened (asserted above).
	t.Logf("evict callbacks fired: %d/%d", evictCalls.Load(), iters)
}

// TestClearDuringFetch exercises Clear racing with in-flight fills.
func TestClearDuringFetch(t *testing.T) {
	t.Parallel()

	const iters = 500
	ctx := context.Background()

	fetch := func(_ context.Context, k int) (int, error) { return k, nil }

	c := oncecache.New[int, int](
		fetch,
		oncecache.OnEvict(func(_ context.Context, k, val int, _ error) {
			if val != k {
				t.Errorf("torn read on evict: key=%d val=%d", k, val)
			}
		}),
	)

	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = c.Get(ctx, i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters/10; i++ {
			c.Clear(ctx)
		}
	}()
	wg.Wait()
}

// TestContext verifies that the context passed to callbacks is decorated with
// the cache, as retrieved via [oncecache.FromContext].
func TestContext(t *testing.T) {
	ctx := context.Background()
	const cacheName = "test-cache"

	var c *oncecache.Cache[int, int]
	c = oncecache.New[int, int](
		func(ctx context.Context, key int) (val int, err error) {
			gotCache := oncecache.FromContext[int, int](ctx)
			require.Equal(t, c, gotCache)
			require.Equal(t, cacheName, gotCache.Name())

			val, err = fetchDouble(ctx, key)
			t.Logf("Fetch[%s](%v) (%v, %v)", c.Name(), key, val, err)
			return val, err
		},
		oncecache.Name(cacheName),
		oncecache.OnFill(func(ctx context.Context, key, val int, err error) {
			gotCache := oncecache.FromContext[int, int](ctx)
			require.Equal(t, c, gotCache)
			require.Equal(t, cacheName, gotCache.Name())
			t.Logf("OnFill[%s](%v, %v, %v)", c.Name(), key, val, err)
		}),
		oncecache.OnEvict(func(ctx context.Context, key, val int, err error) {
			gotCache := oncecache.FromContext[int, int](ctx)
			require.Equal(t, c, gotCache)
			require.Equal(t, cacheName, gotCache.Name())
			t.Logf("OnEvict[%s](%v, %v, %v)", c.Name(), key, val, err)
		}),
	)

	got, err := c.Get(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, 2, got)

	c.Delete(ctx, 1)
}

const (
	acmeName    = "Acme Corporation"
	engDeptName = "Engineering"
	qaDeptName  = "QA"
	wileyName   = "Wile E. Coyote"
	wileyEmpID  = 1
)

func loadHRDatabase(t *testing.T) *hrsystem.HRDatabase {
	t.Helper()
	log := slogt.New(t)

	db, err := hrsystem.NewHRDatabase(
		log.With("layer", "db"),
		"examples/hrsystem/testdata/acme.json",
	)
	require.NoError(t, err)
	return db
}

// TestCallbacks tests use of the On* callbacks, such as [oncecache.OnFill].
func TestCallbacks(t *testing.T) {
	var (
		ctx       = context.Background()
		db        = loadHRDatabase(t)
		orgCache  *oncecache.Cache[string, *hrsystem.Org]
		deptCache *oncecache.Cache[string, *hrsystem.Department]
		empCache  *oncecache.Cache[int, *hrsystem.Employee]
	)

	orgCache = oncecache.New[string, *hrsystem.Org](
		db.GetOrg,
		oncecache.OnFill(func(ctx context.Context, _ string, org *hrsystem.Org, _ error) {
			// Propagate the org's departments to the deptCache.
			for _, dept := range org.Departments {
				_ = deptCache.MaybeSet(ctx, dept.Name, dept, nil)
				// Note: Setting an entry on deptCache should in turn propagate to
				// empCache, because deptCache is itself configured with an OnFill
				// handler below.
			}
		}),
		oncecache.OnEvict(func(ctx context.Context, _ string, org *hrsystem.Org, _ error) {
			// As with OnFill, we'll propagate eviction.
			for _, dept := range org.Departments {
				deptCache.Delete(ctx, dept.Name)
			}
		}),
	)

	deptCache = oncecache.New[string, *hrsystem.Department](
		db.GetDepartment,
		oncecache.OnFill(func(ctx context.Context, _ string, dept *hrsystem.Department, _ error) {
			for _, emp := range dept.Staff {
				_ = empCache.MaybeSet(ctx, emp.ID, emp, nil)
			}
		}),
		oncecache.OnEvict(func(ctx context.Context, _ string, dept *hrsystem.Department, _ error) {
			for _, emp := range dept.Staff {
				empCache.Delete(ctx, emp.ID)
			}
		}),
	)

	empCache = oncecache.New[int, *hrsystem.Employee](db.GetEmployee)

	// orgCache.Get should trigger entry propagation to the other caches.
	acmeCorp, err := orgCache.Get(ctx, acmeName)
	require.NoError(t, err)
	require.Equal(t, acmeName, acmeCorp.Name)
	require.Equal(t, 1, db.Stats().GetOrg())

	wiley, err := empCache.Get(ctx, wileyEmpID)
	require.NoError(t, err)
	require.Equal(t, wileyName, wiley.Name)
	require.Equal(t, 0, db.Stats().GetEmployee())

	engDept, err := deptCache.Get(ctx, engDeptName)
	require.NoError(t, err)
	require.Equal(t, engDeptName, engDept.Name)
	require.Equal(t, 0, db.Stats().GetDepartment())

	// Now we notifyEvict acmeCorp, which should propagate to the other caches.
	orgCache.Delete(ctx, acmeCorp.Name)

	// Wiley should no longer be cached, so this call should hit the db.
	wiley, err = empCache.Get(ctx, wileyEmpID)
	require.NoError(t, err)
	require.Equal(t, wileyName, wiley.Name)
	require.Equal(t, 1, db.Stats().GetEmployee())
}

// TestOnEventChan tests using the [oncecache.OnEvent] mechanism
// to propagate cache entries between overlapping caches, using channels.
func TestOnEventChan(t *testing.T) {
	log := slogt.New(t)
	ctx, cancelFn := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	// Cancel the consumer goroutine and wait for it to exit before the test
	// returns, so it cannot log a (zero-value) event after completion.
	defer func() {
		cancelFn()
		wg.Wait()
	}()

	db := loadHRDatabase(t)

	var (
		orgCache  *oncecache.Cache[string, *hrsystem.Org]
		deptCache *oncecache.Cache[string, *hrsystem.Department]
		empCache  *oncecache.Cache[int, *hrsystem.Employee]
	)

	orgCacheCh := make(chan oncecache.Event[string, *hrsystem.Org], 10)

	orgCache = oncecache.New[string, *hrsystem.Org](
		db.GetOrg,
		oncecache.Name("orgCache"),
		// oncecache.OnFillChan(orgCacheCh, false),
		oncecache.OnEvent(orgCacheCh, false, oncecache.OpFill, oncecache.OpEvict),
	)

	deptCacheCh := make(chan oncecache.Event[string, *hrsystem.Department], 10)

	deptCache = oncecache.New[string, *hrsystem.Department](
		db.GetDepartment,
		oncecache.Name("deptCache"),
		// oncecache.OnFillChan(deptCacheCh, false),
		oncecache.OnEvent(deptCacheCh, false, oncecache.OpFill, oncecache.OpEvict),
	)

	empCache = oncecache.New[int, *hrsystem.Employee](db.GetEmployee, oncecache.Name("empCache"))

	// We use actionCh to signal that an event has been handled.
	actionCh := make(chan oncecache.Op, 100)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log2 := log.With("layer", "event")
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-orgCacheCh:
				log2.Info("Got event", "e", event)
				org := event.Val
				switch event.Op { //nolint:exhaustive
				case oncecache.OpFill:
					for _, dept := range org.Departments {
						_ = deptCache.MaybeSet(ctx, dept.Name, dept, event.Err)
					}
				case oncecache.OpEvict:
					for _, dept := range org.Departments {
						deptCache.Delete(ctx, dept.Name)
					}
				default:
					panic(fmt.Sprintf("unexpected action: %v", event.Op))
				}
				actionCh <- event.Op
			case event := <-deptCacheCh:
				log2.Info("Got event", "e", event)
				dept := event.Val
				switch event.Op { //nolint:exhaustive
				case oncecache.OpFill:
					for _, emp := range dept.Staff {
						_ = empCache.MaybeSet(ctx, emp.ID, emp, nil)
					}
				case oncecache.OpEvict:
					for _, emp := range dept.Staff {
						empCache.Delete(ctx, emp.ID)
					}
				default:
					panic(fmt.Sprintf("unexpected action: %v", event.Op))
				}
				actionCh <- event.Op
			}
		}
	}()

	// orgCache.Get should trigger entry propagation to the other caches.
	acmeCorp, err := orgCache.Get(ctx, acmeName)
	require.NoError(t, err)
	require.Equal(t, acmeName, acmeCorp.Name)

	// Because we're using a goroutine for cache entry propagation, we need
	// to wait for 3 events to be handled:
	//
	// - fill orgCache[acmeName]
	// - fill deptCache[engDeptName]
	// - fill deptCache[qaDeptName]
	//
	// Note that other entry fills occur: in particular, empCache is populated
	// for each employee. However, this test hasn't set up a listener on empCache,
	// so empCache doesn't generate any events.
	requireDrainActionCh(t, actionCh, time.Millisecond*10, oncecache.OpFill, 3)

	require.Equal(t, 0, db.Stats().GetEmployee())
	wiley, err := empCache.Get(ctx, wileyEmpID)
	require.NoError(t, err)
	require.Equal(t, wileyName, wiley.Name)
	require.Equal(t, 0, db.Stats().GetEmployee(), "shouldn't hit db")

	engDept, err := deptCache.Get(ctx, engDeptName)
	require.NoError(t, err)
	require.Equal(t, engDeptName, engDept.Name)
	require.Equal(t, 0, db.Stats().GetDepartment(), "shouldn't hit db")

	// Now we notifyEvict acmeCorp, which should propagate to the other caches.
	orgCache.Delete(ctx, acmeCorp.Name)
	// Similar to above, we should get three evictions.
	requireDrainActionCh(t, actionCh, time.Millisecond*10, oncecache.OpEvict, 3)

	// Wiley should no longer be cached, so this call should hit the db.
	require.Equal(t, 0, db.Stats().GetEmployee())
	wiley, err = empCache.Get(ctx, wileyEmpID)
	require.NoError(t, err)
	require.Equal(t, wileyName, wiley.Name)
	require.Equal(t, 1, db.Stats().GetEmployee())
}

func TestGob(t *testing.T) {
	ctx := context.Background()

	var fetchCount int
	fetchFunc := func(_ context.Context, key int) (val int, err error) {
		fetchCount++
		return key, nil
	}

	c1 := oncecache.New[int, int](fetchFunc)

	const iters = 10
	for i := 0; i < iters; i++ {
		var v int
		v, err := c1.Get(ctx, i)
		require.NoError(t, err)
		require.Equal(t, i, v)
	}

	require.Equal(t, iters, fetchCount)

	var data []byte
	data, err := c1.GobEncode()
	require.NoError(t, err)

	fetchCount = 0
	c2 := oncecache.New[int, int](fetchFunc)
	require.NoError(t, c2.GobDecode(data))

	for i := 0; i < iters; i++ {
		v, err := c2.Get(ctx, i)
		require.NoError(t, err)
		require.Equal(t, i, v)
	}

	require.Equal(t, 0, fetchCount, "fetch shouldn't have been invoked")
	require.Equal(t, iters, c2.Len())
	require.Equal(t, c1.Name(), c2.Name())
	require.Equal(t, c1.String(), c2.String())
}

// requireDrainActionCh verifies that within timeout, ch receives exactly
// wantCount actions, all of which are wantAction.
func requireDrainActionCh(t *testing.T, ch <-chan oncecache.Op,
	timeout time.Duration, wantAction oncecache.Op, wantCount int,
) {
	t.Helper()

	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(timeout, func() {
		cancel(fmt.Errorf("timed out (%s) waiting for action", timeout))
	})

	var gotCount int
	var gotAction oncecache.Op
	for {
		select {
		case <-ctx.Done():
			if gotCount == wantCount {
				return
			}
			assert.Equal(t, wantCount, gotCount,
				"got %d actions in %s but wanted %d", gotCount, timeout, wantCount)
			require.NoError(t, context.Cause(ctx))
		case gotAction = <-ch:
		}

		if gotAction.IsZero() {
			break
		}

		gotCount++
		require.Equal(t, wantAction.String(), gotAction.String())
		require.LessOrEqual(t, gotCount, wantCount)
	}
	require.Equal(t, wantCount, gotCount)
}

func TestLogOutput(t *testing.T) {
	ctx := context.Background()

	c := oncecache.New[int, int](fetchDouble)

	gotName := c.Name()
	require.NotEmpty(t, gotName)
	t.Log(gotName)

	c = oncecache.New[int, int](fetchDouble, oncecache.Name("cache-foo"))
	gotName = c.Name()
	require.Equal(t, "cache-foo", gotName)

	// Sanity check: make sure Cache.LogValue doesn't shit the bed.
	log := slogt.New(t)
	log.Info("hello", "cache", c)

	s := c.String()
	require.Equal(t, "cache-foo[int, int][0]", s)
	_, _ = c.Get(ctx, 1)
	_, _ = c.Get(ctx, 2)
	_, _ = c.Get(ctx, 3)
	s = c.String()
	require.Equal(t, "cache-foo[int, int][3]", s)

	eventCh := make(chan oncecache.Event[int, int], 3)
	c = oncecache.New[int, int](
		fetchDouble,
		oncecache.Name("event-cache"),
		oncecache.OnEvent(eventCh, false, oncecache.OpFill),
	)

	gotVal, gotErr := c.Get(ctx, 1)
	require.NoError(t, gotErr)
	require.Equal(t, 2, gotVal)

	time.Sleep(time.Millisecond) // Allow event to propagate
	var event oncecache.Event[int, int]
	select {
	case event = <-eventCh:
	default:
		t.Fatal("Expected event")
	}
	require.Equal(t, oncecache.OpFill, event.Op)
	t.Logf("event: %s", event)
	t.Logf("entry: %s", event.Entry)

	log.Info("Got event", "event", event)
	log.Info("Got entry", "entry", event.Entry)
}

func TestLog(t *testing.T) {
	buf, log := newBufLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := oncecache.New[int, int](
		calcFibonacci,
		oncecache.Name("fibs"),
		oncecache.Log(log, slog.LevelInfo, oncecache.OpFill, oncecache.OpEvict),
		oncecache.Log(log, slog.LevelDebug, oncecache.OpHit, oncecache.OpMiss),
	)

	_, _ = c.Get(ctx, 10)
	_, _ = c.Get(ctx, 10)
	_, _ = c.Get(ctx, 10)
	c.Delete(ctx, 10)
	_, _ = c.Get(ctx, 10)
	_, _ = c.Get(ctx, 7)
	_, _ = c.Get(ctx, 7)

	c.Delete(ctx, 7)
	_ = c.MaybeSet(ctx, 7, 55, nil)
	_ = c.MaybeSet(ctx, 7, 55, nil)
	_, _ = c.Get(ctx, 7)

	const want = `level=DEBUG msg="Cache event" ev.cache=fibs ev.op=miss ev.k=10
level=INFO msg="Cache event" ev.cache=fibs ev.op=fill ev.k=10 ev.v=55
level=DEBUG msg="Cache event" ev.cache=fibs ev.op=hit ev.k=10 ev.v=55
level=DEBUG msg="Cache event" ev.cache=fibs ev.op=hit ev.k=10 ev.v=55
level=INFO msg="Cache event" ev.cache=fibs ev.op=evict ev.k=10 ev.v=55
level=DEBUG msg="Cache event" ev.cache=fibs ev.op=miss ev.k=10
level=INFO msg="Cache event" ev.cache=fibs ev.op=fill ev.k=10 ev.v=55
level=DEBUG msg="Cache event" ev.cache=fibs ev.op=miss ev.k=7
level=INFO msg="Cache event" ev.cache=fibs ev.op=fill ev.k=7 ev.v=13
level=DEBUG msg="Cache event" ev.cache=fibs ev.op=hit ev.k=7 ev.v=13
level=INFO msg="Cache event" ev.cache=fibs ev.op=evict ev.k=7 ev.v=13
level=INFO msg="Cache event" ev.cache=fibs ev.op=fill ev.k=7 ev.v=55
level=DEBUG msg="Cache event" ev.cache=fibs ev.op=hit ev.k=7 ev.v=55
`
	got := buf.String()
	t.Log("\n", got)
	require.Equal(t, want, got)
}

// newBufLogger returns a slog.Logger that writes to a bytes.Buffer, and doesn't
// output "source" or "time" attributes. This makes it suitable for testing log
// output.
func newBufLogger() (*bytes.Buffer, *slog.Logger) {
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{
		AddSource: false,
		Level:     slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == "time" {
				return slog.Attr{}
			}

			return a
		},
	})
	return buf, slog.New(h)
}

// TestClose exercises [Cache.Close] behaviors.
func TestClose(t *testing.T) {
	t.Parallel()

	t.Run("nil_cache", func(t *testing.T) {
		t.Parallel()
		var c *oncecache.Cache[int, int]
		require.NoError(t, c.Close())
	})

	t.Run("idempotent", func(t *testing.T) {
		t.Parallel()
		c := oncecache.New[int, int](fetchDouble)
		require.NoError(t, c.Close())
		require.NoError(t, c.Close())
	})

	t.Run("clears_entries", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		c := oncecache.New[int, int](fetchDouble)
		_, _ = c.Get(ctx, 1)
		_, _ = c.Get(ctx, 2)
		require.Equal(t, 2, c.Len())
		require.NoError(t, c.Close())
		require.Equal(t, 0, c.Len())
	})

	// Close does NOT fire OnEvict for the entries it clears.
	t.Run("no_evict_on_close", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		var evicts atomic.Int64
		c := oncecache.New[int, int](
			fetchDouble,
			oncecache.OnEvict(func(_ context.Context, _, _ int, _ error) {
				evicts.Add(1)
			}),
		)
		_, _ = c.Get(ctx, 1)
		require.NoError(t, c.Close())
		require.Equal(t, int64(0), evicts.Load(),
			"Close must not fire OnEvict for the entries it clears")
	})

	// Callbacks survive Close: re-using the cache after Close still fires
	// callbacks as normal. Close only clears entries; it does not detach
	// callbacks.
	t.Run("callbacks_survive_close", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		var fills, evicts atomic.Int64
		c := oncecache.New[int, int](
			fetchDouble,
			oncecache.OnFill(func(_ context.Context, _, _ int, _ error) {
				fills.Add(1)
			}),
			oncecache.OnEvict(func(_ context.Context, _, _ int, _ error) {
				evicts.Add(1)
			}),
		)
		_, _ = c.Get(ctx, 1) // fill
		require.Equal(t, int64(1), fills.Load())
		require.NoError(t, c.Close())

		// After Close: refill and delete — both callbacks still fire.
		_, _ = c.Get(ctx, 2)
		require.Equal(t, int64(2), fills.Load())
		c.Delete(ctx, 2)
		require.Equal(t, int64(1), evicts.Load())
	})
}

// TestOnHit verifies the [OnHit] callback fires on cache hits and not misses.
func TestOnHit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var hits atomic.Int64
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnHit(func(_ context.Context, k, v int, err error) {
			require.NoError(t, err)
			require.Equal(t, k*2, v)
			hits.Add(1)
		}),
	)

	_, _ = c.Get(ctx, 7) // miss → fill; no hit
	require.Equal(t, int64(0), hits.Load())
	_, _ = c.Get(ctx, 7) // hit
	require.Equal(t, int64(1), hits.Load())
	_, _ = c.Get(ctx, 7) // hit
	require.Equal(t, int64(2), hits.Load())
	_, _ = c.Get(ctx, 8) // miss → fill; no hit
	require.Equal(t, int64(2), hits.Load())
}

// TestOnMiss verifies the [OnMiss] callback fires on cache misses only.
func TestOnMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var misses atomic.Int64
	var missKeys []int
	var mu sync.Mutex
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnMiss(func(_ context.Context, k, _ int, _ error) {
			mu.Lock()
			missKeys = append(missKeys, k)
			mu.Unlock()
			misses.Add(1)
		}),
	)

	_, _ = c.Get(ctx, 7) // miss
	_, _ = c.Get(ctx, 7) // hit, no miss
	_, _ = c.Get(ctx, 8) // miss
	require.Equal(t, int64(2), misses.Load())
	require.Equal(t, []int{7, 8}, missKeys)

	// MaybeSet does not emit OpMiss.
	require.True(t, c.MaybeSet(ctx, 9, 99, nil))
	require.Equal(t, int64(2), misses.Load())
	_, _ = c.Get(ctx, 9) // hit (already set), no miss
	require.Equal(t, int64(2), misses.Load())
}

// TestMaybeSet_Error verifies that an entry set via MaybeSet with a non-nil
// error preserves that error on subsequent Get calls.
func TestMaybeSet_Error(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c := oncecache.New[int, string](fetchEvenOnly)

	wantErr := errors.New("boom")
	require.True(t, c.MaybeSet(ctx, 42, "x", wantErr))
	v, err := c.Get(ctx, 42)
	require.Equal(t, "x", v)
	require.Equal(t, wantErr, err)

	// Errorful entries are still "filled" — MaybeSet is a no-op.
	require.False(t, c.MaybeSet(ctx, 42, "y", nil))
	v, err = c.Get(ctx, 42)
	require.Equal(t, "x", v)
	require.Equal(t, wantErr, err)
}

// TestDelete_NonExistent verifies Delete is a no-op when the key is absent,
// and that OnEvict is not invoked.
func TestDelete_NonExistent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var evicts atomic.Int64
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvict(func(_ context.Context, _, _ int, _ error) {
			evicts.Add(1)
		}),
	)

	c.Delete(ctx, 42) // key was never set
	require.Equal(t, int64(0), evicts.Load())
	require.Equal(t, 0, c.Len())
}

// TestClear_Empty verifies Clear on an empty cache is a no-op.
func TestClear_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var evicts atomic.Int64
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvict(func(_ context.Context, _, _ int, _ error) {
			evicts.Add(1)
		}),
	)

	c.Clear(ctx)
	require.Equal(t, int64(0), evicts.Load())
	require.Equal(t, 0, c.Len())
}

// TestFetchPanic verifies that a panicking fetch is recovered into a
// wrapped [oncecache.ErrPanic] error stored on the entry, that the
// triggering Get returns that wrapped error rather than propagating the
// panic, and that subsequent Gets return the same error without
// reinvoking fetch. The OnMiss → OnFill lifecycle invariant is preserved.
func TestFetchPanic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var fetchCalls, missCalls, fillCalls atomic.Int64
	c := oncecache.New[int, int](
		func(_ context.Context, _ int) (int, error) {
			fetchCalls.Add(1)
			panic("boom")
		},
		oncecache.OnMiss(func(_ context.Context, _, _ int, _ error) {
			missCalls.Add(1)
		}),
		oncecache.OnFill(func(_ context.Context, _, _ int, _ error) {
			fillCalls.Add(1)
		}),
	)

	v, err := c.Get(ctx, 1)
	require.Zero(t, v)
	require.Error(t, err)
	require.ErrorIs(t, err, oncecache.ErrPanic)
	require.Contains(t, err.Error(), "boom")
	require.Equal(t, int64(1), fetchCalls.Load())
	require.Equal(t, int64(1), missCalls.Load())
	require.Equal(t, int64(1), fillCalls.Load(), "OpFill must follow OpMiss even on panic")

	// Subsequent Get returns the same wrapped error without reinvoking fetch.
	v, err = c.Get(ctx, 1)
	require.Zero(t, v)
	require.ErrorIs(t, err, oncecache.ErrPanic)
	require.Equal(t, int64(1), fetchCalls.Load())
}

// TestOnMissPanic verifies that a panic in an OnMiss callback is recovered
// into the entry's fill error (wrapping ErrPanic) rather than propagating or
// stranding the entry unfilled: fetch is skipped, OnFill still fires with the
// wrapped error, subsequent Gets return the same error, and the entry is
// properly filled (so Delete fires OnEvict).
func TestOnMissPanic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// OnFill/OnEvict callbacks fire synchronously on the calling goroutine,
	// so capturing their args in plain vars (read after the call) is race-free.
	var fetchCalls, fillCalls, evictCalls atomic.Int64
	var fillVal, evictVal int
	var fillErr, evictErr error
	c := oncecache.New[int, int](
		func(_ context.Context, _ int) (int, error) {
			fetchCalls.Add(1)
			return 42, nil
		},
		oncecache.OnMiss(func(_ context.Context, _, _ int, _ error) {
			panic("miss-boom")
		}),
		oncecache.OnFill(func(_ context.Context, _, val int, err error) {
			fillCalls.Add(1)
			fillVal, fillErr = val, err
		}),
		oncecache.OnEvict(func(_ context.Context, _, val int, err error) {
			evictCalls.Add(1)
			evictVal, evictErr = val, err
		}),
	)

	// The triggering Get returns a wrapped error rather than propagating the panic.
	v, err := c.Get(ctx, 1)
	require.Zero(t, v)
	require.ErrorIs(t, err, oncecache.ErrPanic)
	require.Contains(t, err.Error(), "miss-boom")
	require.Equal(t, int64(0), fetchCalls.Load(), "fetch must be skipped when OnMiss panics")

	// OnFill must fire once and receive the zero value plus the wrapped error.
	require.Equal(t, int64(1), fillCalls.Load(), "OpFill must still fire")
	require.Zero(t, fillVal, "OnFill must receive the zero value")
	require.ErrorIs(t, fillErr, oncecache.ErrPanic, "OnFill must receive the wrapped ErrPanic")

	// Subsequent Get returns the same wrapped error; the entry is not a zombie.
	v, err = c.Get(ctx, 1)
	require.Zero(t, v)
	require.ErrorIs(t, err, oncecache.ErrPanic)
	require.Equal(t, int64(0), fetchCalls.Load())

	// The entry is properly filled, so Delete fires OnEvict with the same wrapped error.
	c.Delete(ctx, 1)
	require.Equal(t, int64(1), evictCalls.Load(),
		"Delete must fire OnEvict for the filled (panic-errored) entry")
	require.Zero(t, evictVal, "OnEvict must receive the zero value")
	require.ErrorIs(t, evictErr, oncecache.ErrPanic, "OnEvict must receive the wrapped ErrPanic")
}

// TestGob_Empty verifies gob round-trip of an empty cache preserves the name.
func TestGob_Empty(t *testing.T) {
	t.Parallel()
	c1 := oncecache.New[int, int](fetchDouble, oncecache.Name("empty"))

	data, err := c1.GobEncode()
	require.NoError(t, err)

	c2 := oncecache.New[int, int](fetchDouble)
	require.NoError(t, c2.GobDecode(data))
	require.Equal(t, 0, c2.Len())
	require.Equal(t, "empty", c2.Name())
}

// gobError is a gob-registerable error type used by
// [TestGob_PreservesError].
type gobError string

func (e gobError) Error() string { return string(e) }

// registerGobErrorOnce registers gobError with gob exactly once across all
// tests, even if multiple tests use it. This avoids an init() func (which
// the linter forbids) while also avoiding double-registration across
// parallel tests.
var registerGobErrorOnce sync.Once

func registerGobError() { registerGobErrorOnce.Do(func() { gob.Register(gobError("")) }) }

// TestGob_PreservesError verifies that fill errors survive gob round-trip.
func TestGob_PreservesError(t *testing.T) {
	t.Parallel()
	registerGobError()
	ctx := context.Background()

	myErr := gobError("boom")
	c1 := oncecache.New[int, int](
		func(_ context.Context, k int) (int, error) {
			if k == 0 {
				return 0, myErr
			}
			return k * 2, nil
		},
	)
	_, err := c1.Get(ctx, 0)
	require.EqualError(t, err, "boom")
	_, _ = c1.Get(ctx, 5)

	data, err := c1.GobEncode()
	require.NoError(t, err)

	c2 := oncecache.New[int, int](
		func(_ context.Context, _ int) (int, error) {
			t.Fatal("fetch must not be invoked after GobDecode")
			return 0, nil
		},
	)
	require.NoError(t, c2.GobDecode(data))
	require.Equal(t, 2, c2.Len())

	v, err := c2.Get(ctx, 0)
	require.Zero(t, v)
	require.EqualError(t, err, "boom")

	v, err = c2.Get(ctx, 5)
	require.NoError(t, err)
	require.Equal(t, 10, v)
}

// TestFromContext_NilOrWrongType verifies FromContext returns nil for nil
// contexts, unset keys, and mismatched generic types.
func TestFromContext_NilOrWrongType(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // intentionally passing nil ctx
	require.Nil(t, oncecache.FromContext[int, int](nil))
	require.Nil(t, oncecache.FromContext[int, int](context.Background()))

	c1 := oncecache.New[string, string](func(_ context.Context, k string) (string, error) {
		return k, nil
	})
	ctx := oncecache.NewContext(context.Background(), c1)
	// Matching types: retrieval succeeds.
	require.Equal(t, c1, oncecache.FromContext[string, string](ctx))
	// Mismatched types: returns nil rather than panicking.
	require.Nil(t, oncecache.FromContext[int, int](ctx))
}

// TestNewContext_NilCtx verifies NewContext with a nil context produces a
// usable context backed by [context.Background].
func TestNewContext_NilCtx(t *testing.T) {
	t.Parallel()
	c := oncecache.New[int, int](fetchDouble)
	//nolint:staticcheck // intentionally passing nil ctx
	ctx := oncecache.NewContext[int, int](nil, c)
	require.NotNil(t, ctx)
	require.Equal(t, c, oncecache.FromContext[int, int](ctx))
}

// TestNew_NilOpts verifies that nil [Opt] values passed to [New] are ignored.
func TestNew_NilOpts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := oncecache.New[int, int](
		fetchDouble,
		nil,
		oncecache.Name("with-nils"),
		nil,
	)
	require.Equal(t, "with-nils", c.Name())
	v, _ := c.Get(ctx, 5)
	require.Equal(t, 10, v)
}

// TestLog_NilLogger verifies Log with a nil logger returns a nil Opt that
// does not register callbacks.
func TestLog_NilLogger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.Log(nil, slog.LevelInfo),
	)
	v, err := c.Get(ctx, 3)
	require.NoError(t, err)
	require.Equal(t, 6, v)
}

// TestOp_String covers [Op.String] for valid ops and the "unknown" fallback,
// and [Op.IsZero] for the zero value.
func TestOp_String(t *testing.T) {
	t.Parallel()
	require.Equal(t, "hit", oncecache.OpHit.String())
	require.Equal(t, "miss", oncecache.OpMiss.String())
	require.Equal(t, "fill", oncecache.OpFill.String())
	require.Equal(t, "evict", oncecache.OpEvict.String())
	require.Equal(t, "unknown", oncecache.Op(0).String())
	require.Equal(t, "unknown", oncecache.Op(99).String())
	require.True(t, oncecache.Op(0).IsZero())
	require.False(t, oncecache.OpHit.IsZero())
}

// TestOnEvent_NonBlocking_DropsOnFull verifies that non-blocking OnEvent
// drops events when the receiver cannot keep up.
func TestOnEvent_NonBlocking_DropsOnFull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ch := make(chan oncecache.Event[int, int], 1)
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvent(ch, false, oncecache.OpFill),
	)

	_, _ = c.Get(ctx, 1)
	_, _ = c.Get(ctx, 2)
	_, _ = c.Get(ctx, 3)
	require.Equal(t, 1, len(ch), "non-blocking OnEvent must drop events when full")
}

// TestOnEvent_Blocking verifies that blocking OnEvent delivers every event.
func TestOnEvent_Blocking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ch := make(chan oncecache.Event[int, int], 3)
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvent(ch, true, oncecache.OpFill),
	)

	_, _ = c.Get(ctx, 1)
	_, _ = c.Get(ctx, 2)
	_, _ = c.Get(ctx, 3)
	require.Equal(t, 3, len(ch))
}

// TestOnEvent_BlockingCancellation verifies that a blocking OnEvent send
// aborts when the triggering context is cancelled, so a stalled consumer
// cannot hang the producer indefinitely.
func TestOnEvent_BlockingCancellation(t *testing.T) {
	t.Parallel()

	ch := make(chan oncecache.Event[int, int]) // unbuffered, no receiver
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvent(ch, true, oncecache.OpFill),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	// Without ctx-cancellation in the OnEvent send path, this would hang.
	done := make(chan struct{})
	go func() {
		_, _ = c.Get(ctx, 1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Get hung; OnEvent blocking send did not honor ctx cancellation")
	}
}

// TestEvent_Ctx verifies that delivered Events carry the triggering ctx,
// and that FromContext on Event.Ctx yields the source cache.
func TestEvent_Ctx(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")

	ch := make(chan oncecache.Event[int, int], 1)
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvent(ch, false, oncecache.OpFill),
	)

	_, _ = c.Get(ctx, 1)

	var ev oncecache.Event[int, int]
	select {
	case ev = <-ch:
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
	require.NotNil(t, ev.Ctx)
	require.Equal(t, "marker", ev.Ctx.Value(ctxKey{}))
	// Event.Ctx is decorated with the source cache.
	require.Equal(t, c, oncecache.FromContext[int, int](ev.Ctx))
}

// TestConcurrentCloseAndGet verifies that Close races safely against Get.
// With Close simplified to just clear entries (not detach callbacks), the
// race detector should stay quiet. Post-race, every remaining entry's
// value must match the fetchDouble contract — detecting any torn read.
func TestConcurrentCloseAndGet(t *testing.T) {
	t.Parallel()

	const iters = 1000
	c := oncecache.New[int, int](fetchDouble)
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			v, err := c.Get(context.Background(), i)
			require.NoError(t, err)
			require.Equal(t, i*2, v, "value invariant must hold under concurrent Close")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters/10; i++ {
			_ = c.Close()
		}
	}()
	wg.Wait()
}

// TestGobDecode_Corrupted verifies that GobDecode returns an error for
// malformed input and leaves the cache in a usable state.
func TestGobDecode_Corrupted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := oncecache.New[int, int](fetchDouble, oncecache.Name("orig"))

	require.Error(t, c.GobDecode([]byte("not gob data at all")))
	require.Error(t, c.GobDecode(nil))
	require.Error(t, c.GobDecode([]byte{}))

	// Cache survives the corrupt-decode attempts.
	v, err := c.Get(ctx, 5)
	require.NoError(t, err)
	require.Equal(t, 10, v)
	require.Equal(t, "orig", c.Name())
}

// TestCallback_RegistrationOrder verifies that when multiple callbacks of
// the same op are registered, they fire in the order they were registered.
func TestCallback_RegistrationOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var order []int
	var mu sync.Mutex
	mark := func(n int) func(context.Context, int, int, error) {
		return func(_ context.Context, _, _ int, _ error) {
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
		}
	}
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnFill(mark(1)),
		oncecache.OnFill(mark(2)),
		oncecache.OnFill(mark(3)),
	)
	_, _ = c.Get(ctx, 42)
	require.Equal(t, []int{1, 2, 3}, order)
}

// TestDelete_Reentry verifies that an OnEvict callback may safely call
// methods on the same cache (Get, Has, Len, Keys, even Delete on a
// different key) without deadlocking. This is the snapshot-then-callback
// guarantee added in the Delete/Clear concurrency fix.
func TestDelete_Reentry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var c *oncecache.Cache[int, int]
	c = oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvict(func(_ context.Context, k, _ int, _ error) {
			// Touch the cache from inside the eviction callback. With the
			// pre-fix code that held c.mu across callbacks, all of these
			// would deadlock.
			_ = c.Has(k)
			_ = c.Len()
			_ = c.Keys()
			if k == 1 {
				c.Delete(ctx, 99) // a different (absent) key
			}
		}),
	)
	_, _ = c.Get(ctx, 1)
	c.Delete(ctx, 1)
}

// TestClear_Reentry verifies that an OnEvict callback fired by Clear may
// safely call methods on the same cache without deadlocking.
func TestClear_Reentry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var c *oncecache.Cache[int, int]
	c = oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvict(func(_ context.Context, _, _ int, _ error) {
			_ = c.Has(0)
			_ = c.Len()
		}),
	)
	_, _ = c.Get(ctx, 1)
	_, _ = c.Get(ctx, 2)
	c.Clear(ctx)
}

// TestErrPanic_IsTarget verifies that errors.Is(err, ErrPanic) matches the
// wrapped error returned by Get after a fetch panic.
func TestErrPanic_IsTarget(t *testing.T) {
	t.Parallel()
	c := oncecache.New[int, int](
		func(_ context.Context, _ int) (int, error) {
			panic(errors.New("boom-as-error"))
		},
	)
	_, err := c.Get(context.Background(), 1)
	require.Error(t, err)
	require.ErrorIs(t, err, oncecache.ErrPanic)
	require.Equal(t, oncecache.ErrPanic, errors.Unwrap(err))
	require.Contains(t, err.Error(), "boom-as-error")
}

// TestFetchPanic_MaybeSetIsNoop verifies that after a panicked fetch
// has filled the entry with a wrapped error, MaybeSet for that key is
// a no-op (the entry is already considered filled).
func TestFetchPanic_MaybeSetIsNoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := oncecache.New[int, string](
		func(_ context.Context, _ int) (string, error) { panic("boom") },
	)
	_, _ = c.Get(ctx, 1) // panic recovered → entry filled with wrapped err
	require.False(t, c.MaybeSet(ctx, 1, "override", nil))
	_, err := c.Get(ctx, 1)
	require.ErrorIs(t, err, oncecache.ErrPanic)
}

// TestGet_NilFetch verifies that constructing a cache with a nil fetch
// and then calling Get on an unfilled key returns a wrapped ErrPanic
// (the recovered nil-pointer dereference) rather than propagating the
// panic. The cache is otherwise usable via MaybeSet.
func TestGet_NilFetch(t *testing.T) {
	t.Parallel()
	c := oncecache.New[int, int](nil)

	v, err := c.Get(context.Background(), 1)
	require.Zero(t, v)
	require.ErrorIs(t, err, oncecache.ErrPanic,
		"nil fetch should be recovered into a wrapped ErrPanic, not propagated")

	// MaybeSet works with nil fetch.
	require.True(t, c.MaybeSet(context.Background(), 2, 22, nil))
	v, err = c.Get(context.Background(), 2)
	require.NoError(t, err)
	require.Equal(t, 22, v)
}

// TestConcurrentMaybeSet verifies that, under heavy concurrent MaybeSet
// for the same key, exactly one call wins (returns true) and all others
// return false. The winner's value is what subsequent Get observes.
func TestConcurrentMaybeSet(t *testing.T) {
	t.Parallel()
	const concurrency = 200
	c := oncecache.New[int, int](fetchDouble)

	var winners atomic.Int64
	wg := &sync.WaitGroup{}
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			if c.MaybeSet(context.Background(), 7, i, nil) {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(1), winners.Load(), "exactly one MaybeSet must win")

	// Whatever value won is consistent across subsequent reads.
	v1, _ := c.Get(context.Background(), 7)
	v2, _ := c.Get(context.Background(), 7)
	require.Equal(t, v1, v2)
	require.GreaterOrEqual(t, v1, 0)
	require.Less(t, v1, concurrency)
}

// TestMaybeSet_VsGetConcurrent verifies that under a race between Get
// (which would invoke fetch) and MaybeSet (which would set directly),
// exactly one populates the entry; both eventual reads agree.
func TestMaybeSet_VsGetConcurrent(t *testing.T) {
	t.Parallel()
	const iters = 500
	for i := 0; i < iters; i++ {
		var fetchCalls atomic.Int64
		c := oncecache.New[int, int](
			func(_ context.Context, k int) (int, error) {
				fetchCalls.Add(1)
				return k * 2, nil
			},
		)
		wg := &sync.WaitGroup{}
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = c.MaybeSet(context.Background(), 1, 999, nil)
		}()
		go func() {
			defer wg.Done()
			_, _ = c.Get(context.Background(), 1)
		}()
		wg.Wait()

		v, err := c.Get(context.Background(), 1)
		require.NoError(t, err)
		// The winner is either MaybeSet (v=999) or Get's fetch (v=2).
		require.Contains(t, []int{2, 999}, v)
		// Fetch ran at most once (zero if MaybeSet won the race).
		require.LessOrEqual(t, fetchCalls.Load(), int64(1))
	}
}

// TestOnHit_ErrorEntry verifies that OnHit fires when Get returns an
// already-stored fill error (errorful entries are valid hits).
func TestOnHit_ErrorEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	wantErr := errors.New("nope")
	var hits atomic.Int64
	c := oncecache.New[int, string](
		func(_ context.Context, _ int) (string, error) { return "", wantErr },
		oncecache.OnHit(func(_ context.Context, _ int, val string, err error) {
			require.Empty(t, val)
			require.Equal(t, wantErr, err)
			hits.Add(1)
		}),
	)
	_, _ = c.Get(ctx, 1) // miss → fill (error)
	_, err := c.Get(ctx, 1)
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, int64(1), hits.Load())
}

// TestOnFill_PanicPropagates verifies that an OnFill panic propagates to
// the Get caller and is NOT recovered (only FetchFunc and OnMiss panics are
// recovered into a fill error). The entry is still filled before OnFill
// runs, so subsequent Gets succeed.
func TestOnFill_PanicPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnFill(func(_ context.Context, _, _ int, _ error) { panic("cb-boom") }),
	)
	require.PanicsWithValue(t, "cb-boom", func() { _, _ = c.Get(ctx, 1) })
	// The entry was filled before OnFill ran, so subsequent Gets succeed
	// (and fire OnHit if registered, not OnFill again).
	v, err := c.Get(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, 2, v)
}

// TestOnEvict_PanicPropagates verifies that OnEvict panics propagate to
// the Delete caller. The entry is removed before OnEvict runs.
func TestOnEvict_PanicPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvict(func(_ context.Context, _, _ int, _ error) { panic("evict-boom") }),
	)
	_, _ = c.Get(ctx, 1)
	require.PanicsWithValue(t, "evict-boom", func() { c.Delete(ctx, 1) })
	require.False(t, c.Has(1), "entry must be removed even though OnEvict panicked")
}

// TestLog_NilLevel verifies that passing a nil leveler to Log defaults
// to slog.LevelInfo and produces output.
func TestLog_NilLevel(t *testing.T) {
	t.Parallel()
	buf, log := newBufLogger()
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.Name("nillevel"),
		oncecache.Log(log, nil, oncecache.OpFill),
	)
	_, _ = c.Get(context.Background(), 1)
	out := buf.String()
	require.Contains(t, out, "level=INFO")
	require.Contains(t, out, "ev.cache=nillevel")
	require.Contains(t, out, "ev.op=fill")
}

// TestEntry_LogValue_Types verifies isValLogged behavior across V types:
// numerics, bool, slog.LogValuer types are logged; string and arbitrary
// structs are not.
func TestEntry_LogValue_Types(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("int_logged", func(t *testing.T) {
		t.Parallel()
		buf, log := newBufLogger()
		c := oncecache.New[int, int](
			fetchDouble,
			oncecache.Name("ints"),
			oncecache.Log(log, slog.LevelInfo, oncecache.OpFill),
		)
		_, _ = c.Get(ctx, 5)
		require.Contains(t, buf.String(), "ev.v=10")
	})

	t.Run("string_not_logged", func(t *testing.T) {
		t.Parallel()
		buf, log := newBufLogger()
		c := oncecache.New[int, string](
			func(_ context.Context, _ int) (string, error) { return "secret", nil },
			oncecache.Name("strs"),
			oncecache.Log(log, slog.LevelInfo, oncecache.OpFill),
		)
		_, _ = c.Get(ctx, 1)
		require.NotContains(t, buf.String(), "secret",
			"string V must not appear in slog output")
	})

	t.Run("logvaluer_logged", func(t *testing.T) {
		t.Parallel()
		buf, log := newBufLogger()
		c := oncecache.New[int, hasLogValue](
			func(_ context.Context, _ int) (hasLogValue, error) {
				return hasLogValue{tag: "TAGGED"}, nil
			},
			oncecache.Name("lv"),
			oncecache.Log(log, slog.LevelInfo, oncecache.OpFill),
		)
		_, _ = c.Get(ctx, 1)
		require.Contains(t, buf.String(), "TAGGED")
	})

	t.Run("struct_not_logged", func(t *testing.T) {
		t.Parallel()
		buf, log := newBufLogger()
		c := oncecache.New[int, plainStruct](
			func(_ context.Context, _ int) (plainStruct, error) {
				return plainStruct{secret: "nope"}, nil
			},
			oncecache.Name("ps"),
			oncecache.Log(log, slog.LevelInfo, oncecache.OpFill),
		)
		_, _ = c.Get(ctx, 1)
		require.NotContains(t, buf.String(), "nope",
			"plain struct V must not appear in slog output")
	})
}

// hasLogValue is a V type implementing slog.LogValuer.
type hasLogValue struct {
	tag string
}

func (h hasLogValue) LogValue() slog.Value { return slog.StringValue(h.tag) }

// plainStruct is a V type that does NOT implement slog.LogValuer.
type plainStruct struct {
	secret string
}

// TestEntry_String verifies the compact debug format with and without err.
func TestEntry_String(t *testing.T) {
	t.Parallel()
	c := oncecache.New[int, int](fetchDouble, oncecache.Name("nm"))

	ev := oncecache.Entry[int, int]{Cache: c, Key: 7, Val: 14}
	require.Equal(t, "nm[7]", ev.String())

	ev.Err = errors.New("oof")
	require.Equal(t, "nm[7][! oof]", ev.String())
}

// TestEvent_String verifies the compact debug format for events.
func TestEvent_String(t *testing.T) {
	t.Parallel()
	c := oncecache.New[int, int](fetchDouble, oncecache.Name("nm"))

	e := oncecache.Event[int, int]{
		Op:    oncecache.OpHit,
		Entry: oncecache.Entry[int, int]{Cache: c, Key: 7, Val: 14},
	}
	require.Equal(t, "nm.hit[7]", e.String())

	e.Err = errors.New("oof")
	require.Equal(t, "nm.hit[7][! oof]", e.String())
}

// TestOp_IsZero_All verifies that all defined Op constants are non-zero
// and only the zero-value Op reports IsZero.
func TestOp_IsZero_All(t *testing.T) {
	t.Parallel()
	for _, op := range []oncecache.Op{oncecache.OpHit, oncecache.OpMiss, oncecache.OpFill, oncecache.OpEvict} {
		require.False(t, op.IsZero(), "%s must not be zero", op)
	}
	require.True(t, oncecache.Op(0).IsZero())
}

// TestNewContext_PreservesParent verifies that NewContext does not destroy
// pre-existing values on the parent context.
func TestNewContext_PreservesParent(t *testing.T) {
	t.Parallel()
	type k struct{}
	parent := context.WithValue(context.Background(), k{}, "parent-value")
	c := oncecache.New[int, int](fetchDouble)
	ctx := oncecache.NewContext(parent, c)
	require.Equal(t, "parent-value", ctx.Value(k{}))
	require.Equal(t, c, oncecache.FromContext[int, int](ctx))
}

// TestFromContext_NestedCachesInnerWins verifies that when ctx has been
// decorated with two caches of the same K/V type, the most-recently-stored
// (innermost) cache is returned.
func TestFromContext_NestedCachesInnerWins(t *testing.T) {
	t.Parallel()
	c1 := oncecache.New[int, int](fetchDouble, oncecache.Name("outer"))
	c2 := oncecache.New[int, int](fetchDouble, oncecache.Name("inner"))
	ctx := oncecache.NewContext(oncecache.NewContext(context.Background(), c1), c2)
	got := oncecache.FromContext[int, int](ctx)
	require.Equal(t, "inner", got.Name())
}

// TestCache_AsSlogAttribute verifies that a Cache can be passed directly
// to slog and produces a useful structured representation.
func TestCache_AsSlogAttribute(t *testing.T) {
	t.Parallel()
	buf, log := newBufLogger()
	c := oncecache.New[int, string](
		func(_ context.Context, _ int) (string, error) { return "v", nil },
		oncecache.Name("attr-cache"),
	)
	_, _ = c.Get(context.Background(), 1)
	_, _ = c.Get(context.Background(), 2)
	log.Info("status", "cache", c)
	out := buf.String()
	require.Contains(t, out, "attr-cache")
	require.Contains(t, out, "entries=2")
	require.Contains(t, out, "key=int")
	require.Contains(t, out, "val=string")
}

// TestName_NilAndZeroValue verifies Cache.Name is safe to call on a nil
// receiver and on an uninitialized (zero-value) cache, returning "" rather
// than panicking — the property [Event.LogValue]/[Entry.LogValue] rely on
// when slog resolves a zero Event/Entry.
func TestName_NilAndZeroValue(t *testing.T) {
	t.Parallel()

	var nilCache *oncecache.Cache[int, int]
	require.Equal(t, "", nilCache.Name(), "nil receiver must yield empty name")

	var zeroCache oncecache.Cache[int, int]
	require.Equal(t, "", zeroCache.Name(), "zero-value cache must yield empty name")
}

// TestStress runs random concurrent Get/MaybeSet/Delete/Clear/Has/Keys
// operations against the cache. Its primary job is to surface races,
// deadlocks, and panics under -race. A Get or MaybeSet for any key k
// produces a value that is either fetchDouble(k) = k*2 or the MaybeSet
// value k*10 — any other observed value is a torn read and fails the
// test invariant.
func TestStress(t *testing.T) {
	t.Parallel()
	const goroutines = 32
	const opsPerGoroutine = 2000
	const keyspace = 64

	var fetchCalls atomic.Int64
	c := oncecache.New[int, int](
		func(_ context.Context, k int) (int, error) {
			fetchCalls.Add(1)
			return k * 2, nil
		},
		oncecache.OnFill(func(_ context.Context, _, _ int, _ error) {}),
		oncecache.OnEvict(func(_ context.Context, _, _ int, _ error) {}),
		oncecache.OnHit(func(_ context.Context, _, _ int, _ error) {}),
	)

	checkValue := func(key, val int) {
		if val != key*2 && val != key*10 {
			t.Errorf("torn read: key=%d got val=%d (expected %d or %d)",
				key, val, key*2, key*10)
		}
	}

	wg := &sync.WaitGroup{}
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		seed := int64(g)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			ctx := context.Background()
			for i := 0; i < opsPerGoroutine; i++ {
				key := rng.Intn(keyspace)
				switch rng.Intn(6) {
				case 0:
					v, err := c.Get(ctx, key)
					if err == nil {
						checkValue(key, v)
					}
				case 1:
					_ = c.MaybeSet(ctx, key, key*10, nil)
				case 2:
					c.Delete(ctx, key)
				case 3:
					if rng.Intn(50) == 0 {
						c.Clear(ctx)
					}
				case 4:
					_ = c.Has(key)
				case 5:
					_ = c.Keys()
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("fetch invocations: %d (over %d ops, keyspace %d)",
		fetchCalls.Load(), goroutines*opsPerGoroutine, keyspace)
}

// TestOnEvent_AllOpsDefault verifies that an empty ops list defaults to
// delivering all four op kinds, and exercises the OpHit / OpMiss / OpEvict
// branches of eventOpt.apply.
func TestOnEvent_AllOpsDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ch := make(chan oncecache.Event[int, int], 16)
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvent(ch, false), // no ops → all four
	)

	_, _ = c.Get(ctx, 1) // miss + fill
	_, _ = c.Get(ctx, 1) // hit
	c.Delete(ctx, 1)     // evict

	got := make(map[oncecache.Op]int)
loop:
	for {
		select {
		case ev := <-ch:
			got[ev.Op]++
		case <-time.After(50 * time.Millisecond):
			break loop
		}
	}
	require.Equal(t, 1, got[oncecache.OpMiss])
	require.Equal(t, 1, got[oncecache.OpFill])
	require.Equal(t, 1, got[oncecache.OpHit])
	require.Equal(t, 1, got[oncecache.OpEvict])
}

// TestLog_DefaultOps verifies that calling Log with an empty ops list
// logs all four op kinds.
func TestLog_DefaultOps(t *testing.T) {
	t.Parallel()
	buf, log := newBufLogger()
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.Name("dops"),
		oncecache.Log(log, slog.LevelInfo), // no ops → all
	)
	_, _ = c.Get(context.Background(), 1) // miss + fill
	_, _ = c.Get(context.Background(), 1) // hit
	c.Delete(context.Background(), 1)     // evict

	out := buf.String()
	for _, op := range []string{"miss", "fill", "hit", "evict"} {
		require.Contains(t, out, "ev.op="+op, "missing op=%s in output", op)
	}
}

// TestGobDecode_ZeroCache verifies that decoding into a literal
// `var c Cache[K, V]` produces a usable cache: Get on decoded keys
// returns the decoded values, MaybeSet works, and Get on a new key
// (no fetch func) yields a wrapped ErrPanic instead of panicking.
func TestGobDecode_ZeroCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := oncecache.New[int, int](fetchDouble, oncecache.Name("zero-src"))
	_, _ = c1.Get(ctx, 5)
	_, _ = c1.Get(ctx, 6)
	data, err := c1.GobEncode()
	require.NoError(t, err)

	// Decode into a literal zero-value cache.
	var c2 oncecache.Cache[int, int]
	require.NoError(t, c2.GobDecode(data))
	require.Equal(t, "zero-src", c2.Name())
	require.Equal(t, 2, c2.Len())

	// Get on decoded keys works and returns decoded values.
	v, err := c2.Get(ctx, 5)
	require.NoError(t, err)
	require.Equal(t, 10, v)

	// MaybeSet on a new key works.
	require.True(t, c2.MaybeSet(ctx, 99, 999, nil))
	v, err = c2.Get(ctx, 99)
	require.NoError(t, err)
	require.Equal(t, 999, v)

	// Get on a key with no decoded entry and nil fetch returns a wrapped
	// ErrPanic (the recovered nil-pointer dereference).
	_, err = c2.Get(ctx, 777)
	require.ErrorIs(t, err, oncecache.ErrPanic)
}

// TestGob_PanicFilledEntry verifies that entries whose fill error wraps
// [oncecache.ErrPanic] round-trip cleanly through GobEncode/GobDecode —
// the wrapped-error relationship is preserved on the decoded side.
func TestGob_PanicFilledEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := oncecache.New[int, int](
		func(_ context.Context, _ int) (int, error) { panic("gob-me") },
	)
	_, err := c1.Get(ctx, 1) // recovers into wrapped ErrPanic
	require.ErrorIs(t, err, oncecache.ErrPanic)

	data, err := c1.GobEncode()
	require.NoError(t, err, "gob must support panic-filled entries")

	c2 := oncecache.New[int, int](fetchDouble)
	require.NoError(t, c2.GobDecode(data))
	v, err := c2.Get(ctx, 1)
	require.Zero(t, v)
	require.ErrorIs(t, err, oncecache.ErrPanic,
		"decoded cache must preserve the ErrPanic relationship")
	require.Contains(t, err.Error(), "gob-me",
		"decoded cache must preserve the panic message")
}

// TestNew_PtrToName verifies that passing *Name (rather than Name) works
// — a pointer to the typed alias is accepted rather than panicking.
func TestNew_PtrToName(t *testing.T) {
	t.Parallel()
	n := oncecache.Name("ptr-cache")
	c := oncecache.New[int, int](fetchDouble, &n)
	require.Equal(t, "ptr-cache", c.Name())
}

// BenchmarkGet_Hit measures a single-goroutine steady-state cache hit.
func BenchmarkGet_Hit(b *testing.B) {
	ctx := context.Background()
	c := oncecache.New[int, int](fetchDouble)
	_, _ = c.Get(ctx, 42)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, 42)
	}
}

// BenchmarkGet_Miss measures cache miss + fill for unique keys.
func BenchmarkGet_Miss(b *testing.B) {
	ctx := context.Background()
	c := oncecache.New[int, int](fetchDouble)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, i)
	}
}

// BenchmarkGet_Parallel_Hit measures concurrent hits on a hot key.
func BenchmarkGet_Parallel_Hit(b *testing.B) {
	ctx := context.Background()
	c := oncecache.New[int, int](fetchDouble)
	_, _ = c.Get(ctx, 42)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = c.Get(ctx, 42)
		}
	})
}

// BenchmarkGet_Parallel_Miss measures concurrent cache misses across
// unique keys — each call creates a new entry and invokes fetch exactly
// once. Contrast with [BenchmarkGet_Parallel_Hit]: this one exercises the
// map-insertion path under contention.
func BenchmarkGet_Parallel_Miss(b *testing.B) {
	ctx := context.Background()
	c := oncecache.New[int, int](fetchDouble)
	var key atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = c.Get(ctx, int(key.Add(1)))
		}
	})
}

// BenchmarkGet_Hit_WithCallbacks measures the "slow path" hit cost when
// all four callback kinds are registered. Compare to [BenchmarkGet_Hit]
// for the callback-dispatch overhead.
func BenchmarkGet_Hit_WithCallbacks(b *testing.B) {
	noop4 := func(_ context.Context, _, _ int, _ error) {}
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnHit(noop4),
		oncecache.OnMiss(noop4),
		oncecache.OnFill(noop4),
		oncecache.OnEvict(noop4),
	)
	ctx := context.Background()
	_, _ = c.Get(ctx, 42)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, 42)
	}
}

// BenchmarkGet_Miss_WithCallbacks measures the slow-path miss cost. Compare
// to [BenchmarkGet_Miss] for the callback-dispatch overhead on miss.
func BenchmarkGet_Miss_WithCallbacks(b *testing.B) {
	noop4 := func(_ context.Context, _, _ int, _ error) {}
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnHit(noop4),
		oncecache.OnMiss(noop4),
		oncecache.OnFill(noop4),
	)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, i)
	}
}

// BenchmarkGet_Miss_Panic measures the per-miss overhead when fetch
// panics and the panic is recovered into a wrapped [oncecache.ErrPanic].
// Compare to [BenchmarkGet_Miss] to isolate the recover/wrap cost.
func BenchmarkGet_Miss_Panic(b *testing.B) {
	c := oncecache.New[int, int](
		func(_ context.Context, _ int) (int, error) { panic("bench-panic") },
	)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, i)
	}
}

// BenchmarkMaybeSet_Existing measures MaybeSet on an already-filled
// entry — the common case in composite-cache propagation where the same
// value is offered repeatedly. The call returns false without writing.
func BenchmarkMaybeSet_Existing(b *testing.B) {
	c := oncecache.New[int, int](fetchDouble)
	ctx := context.Background()
	c.MaybeSet(ctx, 42, 84, nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.MaybeSet(ctx, 42, 84, nil)
	}
}

// BenchmarkDelete measures the Get+Delete cycle without an OnEvict
// callback (fast path: no snapshot, no callback invocation).
func BenchmarkDelete(b *testing.B) {
	ctx := context.Background()
	c := oncecache.New[int, int](fetchDouble)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, i)
		c.Delete(ctx, i)
	}
}

// BenchmarkDelete_WithCallback measures the Get+Delete cycle with an
// OnEvict callback, exercising the snapshot-then-invoke-outside-lock
// path added in the Delete/Clear concurrency fix. Compare to
// [BenchmarkDelete] for the callback-path overhead.
func BenchmarkDelete_WithCallback(b *testing.B) {
	ctx := context.Background()
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvict(func(_ context.Context, _, _ int, _ error) {}),
	)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, i)
		c.Delete(ctx, i)
	}
}

// BenchmarkHas measures the cheap presence check on an existing key.
func BenchmarkHas(b *testing.B) {
	c := oncecache.New[int, int](fetchDouble)
	_, _ = c.Get(context.Background(), 42)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Has(42)
	}
}

// BenchmarkOnEvent_NonBlocking measures the per-hit overhead of emitting
// an [OnEvent] on a saturated (unbuffered, no receiver) channel with
// block=false — every event is dropped. Baseline for event-system cost.
func BenchmarkOnEvent_NonBlocking(b *testing.B) {
	ctx := context.Background()
	ch := make(chan oncecache.Event[int, int]) // unbuffered, no reader
	c := oncecache.New[int, int](
		fetchDouble,
		oncecache.OnEvent(ch, false, oncecache.OpHit),
	)
	_, _ = c.Get(ctx, 42) // prime

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(ctx, 42)
	}
}

//nolint:revive
func ExampleCache_Keys() {
	// Ignore error handling for brevity.
	ctx := context.Background()
	c := oncecache.New[int, int](calcFibonacci)

	for key := 4; key < 7; key++ {
		val, _ := c.Get(ctx, key) // Prime the cache for keys 4, 5, 6
		fmt.Println(key, val)
	}

	keys := c.Keys() // Keys returns indeterminate order
	slices.Sort(keys)
	fmt.Println("Keys in cache:", keys)
	fmt.Println("Num entries:", c.Len())
	fmt.Println("Has key 2?", c.Has(2))

	c.Delete(ctx, 5)
	keys = c.Keys()
	slices.Sort(keys)
	fmt.Println("Keys in cache after Delete(5):", keys)

	// MaybeSet sets the value if the key is not already in the cache.
	didSet := c.MaybeSet(ctx, 4, 3, nil) // No-op: 4 already in cache
	fmt.Println("Did set 4?", didSet)
	didSet = c.MaybeSet(ctx, 7, 13, nil) // Cache write: 7 not in cache
	fmt.Println("Did set 7?", didSet)

	c.Clear(ctx) // Clear empties c, firing any OnEvict callbacks.
	fmt.Println("Keys after cache clear:", c.Keys())

	// Close empties the cache without firing OnEvict. Callbacks are
	// retained and the cache remains fully usable for later Get /
	// MaybeSet / Delete calls.
	_ = c.Close()

	// Output:
	// 4 3
	// 5 5
	// 6 8
	// Keys in cache: [4 5 6]
	// Num entries: 3
	// Has key 2? false
	// Keys in cache after Delete(5): [4 6]
	// Did set 4? false
	// Did set 7? true
	// Keys after cache clear: []
}

//nolint:revive
func ExampleCache_Get() {
	// Ignore error handling for brevity.
	ctx := context.Background()
	c := oncecache.New[int, int](calcFibonacci)

	key := 6
	val, _ := c.Get(ctx, key) // Cache MISS - calcFibonacci is invoked
	fmt.Println(key, val)
	val, _ = c.Get(ctx, key) // Cache HIT
	fmt.Println(key, val)

	key = 9
	val, _ = c.Get(ctx, key) // Cache MISS - calcFibonacci is invoked
	fmt.Println(key, val)

	// Output:
	// 6 8
	// 6 8
	// 9 34
}
