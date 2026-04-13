package oncecache_test

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/neilotoole/slogt"

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
	fetch := func(_ context.Context, k int) (string, error) {
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
			if val != "" && val != "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" {
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
		oncecache.OnEvict(func(_ context.Context, k int, val int, _ error) {
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
	defer cancelFn()

	db := loadHRDatabase(t)

	var (
		orgCache  *oncecache.Cache[string, *hrsystem.Org]
		deptCache *oncecache.Cache[string, *hrsystem.Department]
		empCache  *oncecache.Cache[int, *hrsystem.Employee]
	)

	orgCacheCh := make(chan oncecache.Event[string, *hrsystem.Org], 10)
	defer close(orgCacheCh)

	orgCache = oncecache.New[string, *hrsystem.Org](
		db.GetOrg,
		oncecache.Name("orgCache"),
		// oncecache.OnFillChan(orgCacheCh, false),
		oncecache.OnEvent(orgCacheCh, false, oncecache.OpFill, oncecache.OpEvict),
	)

	deptCacheCh := make(chan oncecache.Event[string, *hrsystem.Department], 10)
	defer close(deptCacheCh)

	deptCache = oncecache.New[string, *hrsystem.Department](
		db.GetDepartment,
		oncecache.Name("deptCache"),
		// oncecache.OnFillChan(deptCacheCh, false),
		oncecache.OnEvent(deptCacheCh, false, oncecache.OpFill, oncecache.OpEvict),
	)

	empCache = oncecache.New[int, *hrsystem.Employee](db.GetEmployee, oncecache.Name("empCache"))

	// We use actionCh to signal that an event has been handled.
	actionCh := make(chan oncecache.Op, 100)
	go func() {
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
					if event.Op.IsZero() {
						// This is the final zero event, indicating that the channel is closed.
						return
					}
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
		var c *oncecache.Cache[int, int]
		require.NoError(t, c.Close())
	})

	t.Run("idempotent", func(t *testing.T) {
		c := oncecache.New[int, int](fetchDouble)
		require.NoError(t, c.Close())
		require.NoError(t, c.Close())
	})

	t.Run("clears_entries", func(t *testing.T) {
		ctx := context.Background()
		c := oncecache.New[int, int](fetchDouble)
		_, _ = c.Get(ctx, 1)
		_, _ = c.Get(ctx, 2)
		require.Equal(t, 2, c.Len())
		require.NoError(t, c.Close())
		require.Equal(t, 0, c.Len())
	})

	t.Run("detaches_callbacks", func(t *testing.T) {
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
		// OnEvict is not invoked by Close.
		require.Equal(t, int64(0), evicts.Load())
		// And after Close, Delete also doesn't fire OnEvict (callbacks detached).
		c.Delete(ctx, 1)
		require.Equal(t, int64(0), evicts.Load())
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
		oncecache.OnMiss[int, int](func(_ context.Context, k int) {
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

// TestFetchPanic documents the current behavior when the fetch function
// panics. The panic propagates to the Get caller; [sync.Once] records the
// call as done, so the entry is permanently cached as the zero value with
// nil error and fetch is not reinvoked. OpMiss fires but OpFill does not —
// which violates the "OpMiss is always immediately followed by OpFill" doc
// invariant. See oncecache review item #3.
func TestFetchPanic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var fetchCalls, missCalls, fillCalls atomic.Int64
	c := oncecache.New[int, int](
		func(_ context.Context, _ int) (int, error) {
			fetchCalls.Add(1)
			panic("boom")
		},
		oncecache.OnMiss[int, int](func(_ context.Context, _ int) {
			missCalls.Add(1)
		}),
		oncecache.OnFill(func(_ context.Context, _, _ int, _ error) {
			fillCalls.Add(1)
		}),
	)

	func() {
		defer func() {
			require.Equal(t, "boom", recover())
		}()
		_, _ = c.Get(ctx, 1)
	}()
	require.Equal(t, int64(1), fetchCalls.Load())
	require.Equal(t, int64(1), missCalls.Load())
	require.Equal(t, int64(0), fillCalls.Load()) // doc invariant violated

	// Subsequent Get does not re-invoke fetch and returns (zero, nil).
	v, err := c.Get(ctx, 1)
	require.Zero(t, v)
	require.NoError(t, err)
	require.Equal(t, int64(1), fetchCalls.Load())
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

// gobErr is a gob-registerable error type used by [TestGob_PreservesError].
type gobErr string

func (e gobErr) Error() string { return string(e) }

func init() { gob.Register(gobErr("")) }

// TestGob_PreservesError verifies that fill errors survive gob round-trip.
func TestGob_PreservesError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	myErr := gobErr("boom")
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

	c.Clear(ctx) // Clear empties c, but it's still usable
	fmt.Println("Keys after cache clear:", c.Keys())

	// Close clears c and releases resources. Afterwards, c is unusable,
	// and operations on it may return an error.
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
